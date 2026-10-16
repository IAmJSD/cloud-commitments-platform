package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// TestHandleScheduledHTTP_RequestCancelDoesNotCancelTask is the regression
// test for issue #784: when the scheduler (Cloud Scheduler, Azure Logic
// Apps) gives up and drops the connection, the request context is canceled.
// The scheduled task, which may be mid-purchase, must keep running on a
// context that does not inherit that cancellation, but it must stay bounded.
func TestHandleScheduledHTTP_RequestCancelDoesNotCancelTask(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	type ctxKey struct{}
	type observation struct {
		err         error
		value       any
		hasDeadline bool
		deadline    time.Time
	}
	observed := make(chan observation, 1)

	app := &Application{
		Purchase: &testutil.MockPurchaseManager{
			ProcessScheduledPurchasesFunc: func(ctx context.Context) (*purchase.ProcessResult, error) {
				close(started)
				<-release // the caller cancels the request context meanwhile
				deadline, ok := ctx.Deadline()
				observed <- observation{err: ctx.Err(), value: ctx.Value(ctxKey{}), hasDeadline: ok, deadline: deadline}
				return &purchase.ProcessResult{Processed: 1, Executed: 1}, nil
			},
		},
	}

	// Request-scoped values (request IDs, trace spans) must survive the detach.
	reqCtx, cancelReq := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "request-scoped"))
	defer cancelReq()
	req := httptest.NewRequestWithContext(reqCtx, http.MethodPost, "/api/scheduled/process_scheduled_purchases", nil)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.handleScheduledHTTP(w, req)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled task never started")
	}
	cancelReq() // scheduler disconnects mid-task
	close(release)

	var got observation
	select {
	case got = <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled task never finished")
	}
	<-done

	testutil.AssertNoError(t, got.err)
	testutil.AssertEqual(t, any("request-scoped"), got.value)
	testutil.AssertTrue(t, got.hasDeadline, "detached task context must still carry a deadline")
	remaining := time.Until(got.deadline)
	testutil.AssertTrue(t, remaining > 0 && remaining <= scheduledTaskHTTPTimeout,
		"task deadline must be bounded by scheduledTaskHTTPTimeout")
	testutil.AssertEqual(t, http.StatusOK, w.Code)
}

// TestScheduledHTTP_ResponseOutlivesServerWriteTimeout drives a real server
// built by CreateHTTPServer. Its 30s WriteTimeout is shorter than a sweep or
// exchange can take, so without a per-route write deadline a task that runs
// past it completes but the scheduler never receives the result (it sees a
// dropped connection and retries the same task).
func TestScheduledHTTP_ResponseOutlivesServerWriteTimeout(t *testing.T) {
	app := &Application{
		Purchase: &testutil.MockPurchaseManager{
			ProcessScheduledPurchasesFunc: func(ctx context.Context) (*purchase.ProcessResult, error) {
				time.Sleep(400 * time.Millisecond) // longer than the shortened WriteTimeout below
				return &purchase.ProcessResult{Processed: 1}, nil
			},
		},
	}

	srv := CreateHTTPServer(app, 0)
	srv.WriteTimeout = 100 * time.Millisecond // stands in for the 30s production value

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	testutil.AssertNoError(t, err)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+ln.Addr().String()+"/api/scheduled/process_scheduled_purchases", nil)
	testutil.AssertNoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	testutil.AssertNoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	testutil.AssertNoError(t, err)

	testutil.AssertEqual(t, http.StatusOK, resp.StatusCode)
	testutil.AssertContains(t, string(body), `"status":"success"`)
}

// TestStartHTTPServerCancelsScheduledTaskOnShutdown pins the shutdown path of
// the detached task: on SIGTERM the server drains, and a scheduled task still
// running when the drain window ends is canceled and given the rest of the
// grace period to unwind before StartHTTPServer returns (and main closes the
// database under it).
func TestStartHTTPServerCancelsScheduledTaskOnShutdown(t *testing.T) {
	old := shutdownGracePeriod
	shutdownGracePeriod = 600 * time.Millisecond
	t.Cleanup(func() { shutdownGracePeriod = old })

	started := make(chan struct{})
	unwound := make(chan error, 1)
	app := &Application{
		Purchase: &testutil.MockPurchaseManager{
			ProcessScheduledPurchasesFunc: func(ctx context.Context) (*purchase.ProcessResult, error) {
				close(started)
				select {
				case <-ctx.Done():
					unwound <- ctx.Err()
					return nil, ctx.Err()
				case <-time.After(10 * time.Second):
					unwound <- nil
					return &purchase.ProcessResult{}, nil
				}
			},
		},
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	testutil.AssertNoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	testutil.AssertNoError(t, l.Close())

	returned := make(chan error, 1)
	go func() { returned <- StartHTTPServer(app, port) }()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never started listening on %s: %v", addr, dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	go func() {
		resp, postErr := http.Post("http://"+addr+"/api/scheduled/process_scheduled_purchases", "application/json", nil)
		if postErr == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled task never started")
	}
	sendSIGTERM(t)

	select {
	case err := <-returned:
		testutil.AssertNoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("StartHTTPServer did not return after the shutdown grace period")
	}
	// The task must have observed cancellation before StartHTTPServer returned.
	select {
	case err := <-unwound:
		testutil.AssertTrue(t, err != nil, "scheduled task ran to completion instead of being canceled at shutdown")
	default:
		t.Fatal("StartHTTPServer returned while the detached scheduled task was still running")
	}
}

// TestScheduledTaskTimeoutFitsCloudRunRequestTimeout keeps the detached task
// bound inside the Cloud Run request_timeout configured in Terraform. Cloud Run
// runs with cpu_idle = true (module default cpu_throttling), so once the
// platform considers the request finished the instance's CPU is throttled and
// it may be scaled in; a bound beyond request_timeout would only promise time
// the platform does not give.
func TestScheduledTaskTimeoutFitsCloudRunRequestTimeout(t *testing.T) {
	root := filepath.Join("..", "..", "terraform", "environments", "gcp")
	files, err := filepath.Glob(filepath.Join(root, "*.tfvars*"))
	testutil.AssertNoError(t, err)
	files = append(files, filepath.Join(root, "variables.tf"))

	tfvar := regexp.MustCompile(`(?m)^\s*cloud_run_request_timeout\s*=\s*(\d+)`)
	tfdefault := regexp.MustCompile(`(?s)variable "cloud_run_request_timeout" \{.*?default\s*=\s*(\d+)`)
	checked := 0
	for _, f := range files {
		data, readErr := os.ReadFile(f) // #nosec G304 -- fixed repo-relative paths
		testutil.AssertNoError(t, readErr)
		matches := tfvar.FindAllSubmatch(data, -1)
		matches = append(matches, tfdefault.FindAllSubmatch(data, -1)...)
		for _, m := range matches {
			secs, convErr := strconv.Atoi(string(m[1]))
			testutil.AssertNoError(t, convErr)
			limit := time.Duration(secs) * time.Second
			testutil.AssertTrue(t, scheduledTaskHTTPTimeout <= limit,
				fmt.Sprintf("%s: scheduledTaskHTTPTimeout %v exceeds cloud_run_request_timeout %v", f, scheduledTaskHTTPTimeout, limit))
			checked++
		}
	}
	testutil.AssertTrue(t, checked >= 2, "expected to find cloud_run_request_timeout settings in terraform/environments/gcp")
}
