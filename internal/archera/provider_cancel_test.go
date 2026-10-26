package archera

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/stretchr/testify/require"
)

// blockingResolver parks GetSecret until released, signaling when entered.
type blockingResolver struct {
	flakyResolver
	entered chan struct{}
	release chan struct{}
}

func (b *blockingResolver) GetSecret(context.Context, string) (string, error) {
	b.entered <- struct{}{}
	<-b.release
	return testKey, nil
}

func TestClient_WaiterHonoursItsOwnContextWhileAnotherBuilds(t *testing.T) {
	r := &blockingResolver{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p := NewProvider(fullSettings(), r, nil)

	type result struct {
		c   any
		err error
	}
	first := make(chan result, 1)
	go func() {
		c, err := p.Client(context.Background())
		first <- result{c, err}
	}()
	<-r.entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() { _, err := p.Client(ctx); second <- err }()
	select {
	case err := <-second:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		close(r.release)
		t.Fatal("waiting caller ignored its own ctx while another caller built the client")
	}

	close(r.release)
	select {
	case res := <-first:
		require.NoError(t, res.err)
		require.NotNil(t, res.c)
	case <-time.After(2 * time.Second):
		t.Fatal("first caller did not finish")
	}
	require.NotNil(t, p.client.Load(), "client cached after the first caller succeeds")
}

func TestClient_CachedClientSkipsHeldSemaphore(t *testing.T) {
	p := NewProvider(fullSettings(), envResolver(t), nil)
	c, err := p.Client(context.Background())
	require.NoError(t, err)

	p.sem <- struct{}{} // a build is "in flight"
	defer func() { <-p.sem }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := p.Client(ctx)
	require.NoError(t, err)
	require.Equal(t, c, got)
}

func TestClient_CancelledCallerWinningTheSlotNeverResolves(t *testing.T) {
	res := &flakyResolver{}
	p := NewProvider(fullSettings(), res, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 200; i++ {
		_, err := p.Client(ctx)
		require.ErrorIs(t, err, context.Canceled)
	}
	require.Zero(t, res.calls.Load(), "resolver must not be called with a dead ctx")
	require.Zero(t, len(p.sem), "a refused caller must release the slot")

	live := make(chan error, 1)
	go func() { _, err := p.Client(context.Background()); live <- err }()
	select {
	case err := <-live:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("slot leaked: a live caller could not acquire it")
	}
}

// slowResolver counts calls and sleeps so racing callers overlap the build.
type slowResolver struct {
	flakyResolver
	n atomic.Int32
}

func (s *slowResolver) GetSecret(context.Context, string) (string, error) {
	s.n.Add(1)
	time.Sleep(100 * time.Millisecond)
	return testKey, nil
}

func TestClient_ConcurrentCallersShareOneBuild(t *testing.T) {
	r := &slowResolver{}
	p := NewProvider(fullSettings(), r, nil)
	const n = 20
	got := make([]any, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Client(context.Background())
			if err == nil {
				got[i] = c
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, r.n.Load(), "resolver called exactly once")
	require.NotNil(t, got[0])
	for i := range got {
		require.Same(t, got[0], got[i])
	}
}

// ctxBlockedResolver parks until the caller's ctx ends, then returns the
// ctx-wrapped error a real backend would.
type ctxBlockedResolver struct {
	flakyResolver
	entered chan struct{}
}

func (r *ctxBlockedResolver) GetSecret(ctx context.Context, _ string) (string, error) {
	r.calls.Add(1)
	r.entered <- struct{}{}
	<-ctx.Done()
	return "", fmt.Errorf("synthetic backend: %w", ctx.Err())
}

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prevOut, prevLevel := logging.SetOutput(&logs), logging.GetLevel()
	t.Cleanup(func() {
		logging.SetOutput(prevOut)
		logging.SetLevelValue(prevLevel)
	})
	logging.SetLevelValue(logging.LevelWarn)
	return &logs
}

func TestClient_CancelDuringResolveReportsCancellationAndCachesNothing(t *testing.T) {
	logs := captureWarnings(t)
	r := &ctxBlockedResolver{entered: make(chan struct{}, 1)}
	p := NewProvider(fullSettings(), r, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := p.Client(ctx); done <- err }()
	<-r.entered
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, ErrKeyUnavailable)
	case <-time.After(2 * time.Second):
		t.Fatal("Client did not return after its ctx was canceled")
	}
	require.Empty(t, logs.String(), "a canceled build is not a credential failure")
	require.Nil(t, p.client.Load())
	require.Zero(t, len(p.sem))

	p.resolver = &flakyResolver{}
	c, err := p.Client(context.Background())
	require.NoError(t, err)
	require.NotNil(t, c)
}

// badKeyDoneCtxResolver returns an unusable key while the caller's ctx is
// already done.
type badKeyDoneCtxResolver struct {
	flakyResolver
	cancel context.CancelFunc
}

func (r *badKeyDoneCtxResolver) GetSecret(context.Context, string) (string, error) {
	r.cancel()
	return testKey + "\x01suffix", nil
}

func TestClient_ConstructionFailureUnderDoneCtxStaysKeyUnavailable(t *testing.T) {
	logs := captureWarnings(t)
	ctx, cancel := context.WithCancel(context.Background())
	p := NewProvider(fullSettings(), &badKeyDoneCtxResolver{cancel: cancel}, nil)
	_, err := p.Client(ctx)
	require.ErrorIs(t, err, ErrKeyUnavailable)
	require.NotErrorIs(t, err, context.Canceled)
	require.Contains(t, logs.String(), "archera: client initialization failed")
	require.Nil(t, p.client.Load())
}

func TestClient_ResolverErrorWithLiveCtxStaysKeyUnavailable(t *testing.T) {
	logs := captureWarnings(t)
	p := NewProvider(fullSettings(), &flakyResolver{fails: 1}, nil)
	_, err := p.Client(context.Background())
	require.ErrorIs(t, err, ErrKeyUnavailable)
	require.NotErrorIs(t, err, context.Canceled)
	require.Contains(t, logs.String(), "archera: credential resolution failed")
}

type panickingResolver struct{ flakyResolver }

func (*panickingResolver) GetSecret(context.Context, string) (string, error) { panic("synthetic") }

func TestClient_PanicInResolverReleasesTheSlot(t *testing.T) {
	p := NewProvider(fullSettings(), &panickingResolver{}, nil)
	require.Panics(t, func() { _, _ = p.Client(context.Background()) })
	require.Zero(t, len(p.sem), "a panic must not leak the build slot")
}
