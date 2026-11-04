package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/secrets"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil/goroutines"
)

// blockingResolver holds ensureDB inside secret resolution, the one step with no
// timeout of its own, so a background initialization attempt is provably still
// running when the shutdown signal arrives.
type blockingResolver struct {
	secrets.Resolver
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func (r *blockingResolver) GetSecret(context.Context, string) (string, error) {
	r.enteredOnce.Do(func() { close(r.entered) })
	<-r.release
	return "", errors.New("released")
}

func (r *blockingResolver) unblock() { r.releaseOnce.Do(func() { close(r.release) }) }

// startServerWithBlockedInit runs StartHTTPServer on a free port with the
// background initializer parked inside GetSecret, and returns once it is parked.
// The resolver is released on cleanup so a failing test cannot leak the goroutine.
func startServerWithBlockedInit(t *testing.T) (port int, resolver *blockingResolver, returned <-chan error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port = l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	resolver = &blockingResolver{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(resolver.unblock)
	app := &Application{
		Version:        "test",
		dbConfig:       &database.Config{PasswordSecret: "db-password"},
		secretResolver: resolver,
		initRetryDelay: time.Millisecond,
	}

	ret := make(chan error, 1)
	go func() { ret <- StartHTTPServer(app, port) }()

	select {
	case <-resolver.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("background initialization never started")
	}

	// Wait until the listener is bound. Otherwise a later "port is closed" check
	// could pass simply because the server had not started listening yet.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never started listening on %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return port, resolver, ret
}

// sendSIGTERM signals this process. It is only safe after StartHTTPServer has
// installed its NotifyContext, which happens before the initializer starts.
func sendSIGTERM(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
}

// TestStartHTTPServerWaitsForBackgroundInitOnShutdown pins #664: StartHTTPServer
// must not return (letting main run app.Close) while the background initializer
// is still inside ensureDB, because both touch app.DB. It also pins the order:
// the listener is closed (Shutdown ran) while the initializer is still parked,
// so the wait comes after draining, not before.
//
// No wall-clock window decides the result (#36): shutdownInitWaitHook marks
// the wait, and the goroutine dump shows StartHTTPServer blocked in it. Once
// it is blocked there it cannot return until initDone closes, which needs the
// test to release the resolver, or the 30s grace period runs out.
func TestStartHTTPServerWaitsForBackgroundInitOnShutdown(t *testing.T) {
	atWait := make(chan int64, 1)
	shutdownInitWaitHook = func() { atWait <- goroutines.CurrentID() }
	t.Cleanup(func() { shutdownInitWaitHook = nil })

	port, resolver, returned := startServerWithBlockedInit(t)
	sendSIGTERM(t)

	var serverGoroutine int64
	select {
	case serverGoroutine = <-atWait:
	case err := <-returned:
		t.Fatalf("StartHTTPServer returned (%v) without reaching the wait for the background initializer", err)
	case <-time.After(5 * time.Second):
		t.Fatal("StartHTTPServer never reached the wait for the background initializer")
	}

	// The hook runs after Shutdown, so the listener is closed while the
	// initializer is still parked.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("listener still accepting connections when StartHTTPServer reached the initializer wait")
	}

	waitUntilBlockedInStartHTTPServerSelect(t, serverGoroutine, returned)
	select {
	case err := <-returned:
		t.Fatalf("StartHTTPServer returned (%v) while background initialization was still running", err)
	default:
	}

	resolver.unblock()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("StartHTTPServer returned %v after initialization stopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartHTTPServer did not return after the initializer exited")
	}
}

// waitUntilBlockedInStartHTTPServerSelect waits until goroutine id is parked
// in a select directly inside StartHTTPServer. Called after
// shutdownInitWaitHook fired, the only such select left is the wait for the
// background initializer. It fails if StartHTTPServer returns first.
func waitUntilBlockedInStartHTTPServerSelect(t *testing.T, id int64, returned <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if blockedInStartHTTPServerSelect(id) {
			return
		}
		select {
		case err := <-returned:
			t.Fatalf("StartHTTPServer returned (%v) instead of waiting for the background initializer", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("StartHTTPServer never blocked waiting for the background initializer")
		}
		runtime.Gosched()
	}
}

// blockedInStartHTTPServerSelect reports whether goroutine id is parked in a
// select ("goroutine N [select]:" or "[select, M minutes]:") with
// StartHTTPServer as its innermost frame.
func blockedInStartHTTPServerSelect(id int64) bool {
	for _, g := range goroutines.Snapshot() {
		if g.ID != id {
			continue
		}
		header, frames, _ := strings.Cut(g.Stack, "\n")
		topFrame, _, _ := strings.Cut(frames, "\n")
		return strings.Contains(header, "[select") &&
			strings.Contains(topFrame, "internal/server.StartHTTPServer(")
	}
	return false
}

// TestStartHTTPServerShutdownWaitIsBounded pins the grace-period bound: an
// initializer that never exits (a wedged secret store ignoring its context)
// must delay shutdown by at most shutdownGracePeriod, not forever.
func TestStartHTTPServerShutdownWaitIsBounded(t *testing.T) {
	old := shutdownGracePeriod
	shutdownGracePeriod = 200 * time.Millisecond
	t.Cleanup(func() { shutdownGracePeriod = old })

	_, _, returned := startServerWithBlockedInit(t)
	sendSIGTERM(t)

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("StartHTTPServer returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartHTTPServer did not return after the shutdown grace period with the initializer still blocked")
	}
}
