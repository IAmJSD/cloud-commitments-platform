package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	type observation struct {
		err         error
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
				observed <- observation{err: ctx.Err(), hasDeadline: ok, deadline: deadline}
				return &purchase.ProcessResult{Processed: 1, Executed: 1}, nil
			},
		},
	}

	reqCtx, cancelReq := context.WithCancel(context.Background())
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
