package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/ctxutil"
)

// scheduledTaskHTTPTimeout bounds a scheduled task triggered over HTTP. The
// task runs detached from the request context (#784), so this is what stops
// it. It matches the Cloud Run request_timeout configured in Terraform (300s
// in terraform/environments/gcp/*.tfvars and the variable default): Cloud Run
// runs with cpu_idle = true, so past that point the platform treats the
// request as finished, throttles the instance's CPU and may scale it in, and
// a longer bound would promise time the platform does not give. It also
// covers the purchase manager's own detached budget (4m execution + 30s
// terminal save). Azure Container Apps keeps CPU allocated after its ingress
// gives up (240s), so the same bound applies there. Pinned against the
// Terraform values by TestScheduledTaskTimeoutFitsCloudRunRequestTimeout.
const scheduledTaskHTTPTimeout = 300 * time.Second

// scheduledResponseGrace is the extra time past scheduledTaskHTTPTimeout
// allowed for writing the response.
const scheduledResponseGrace = 30 * time.Second

// extendScheduledWriteDeadline lifts the server-wide WriteTimeout (30s) for
// this response, so a task that runs longer can still report its result to a
// scheduler that is waiting for it instead of the connection being dropped.
func extendScheduledWriteDeadline(w http.ResponseWriter) {
	deadline := time.Now().Add(scheduledTaskHTTPTimeout + scheduledResponseGrace)
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("Failed to extend scheduled task write deadline: %v", err)
	}
}

// scheduledTaskTracker gives detached HTTP-triggered scheduled tasks a
// shutdown path. Detaching drops the request's cancellation, so without this a
// task would keep running after server shutdown and main would close the
// database under it. The zero value is ready to use.
type scheduledTaskTracker struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	stopped bool
	stopCh  chan struct{}
}

// start returns the context a scheduled task runs on: parent's values, no
// parent cancellation, bounded by d, and canceled by stop. Call done when the
// task returns. A task started after stop gets an already-canceled context.
func (t *scheduledTaskTracker) start(parent context.Context, d time.Duration) (ctx context.Context, done func()) {
	ctx, cancel := ctxutil.DetachedWithTimeout(parent, d)

	t.mu.Lock()
	if t.stopCh == nil {
		t.stopCh = make(chan struct{})
	}
	if t.stopped {
		t.mu.Unlock()
		cancel()
		return ctx, func() {}
	}
	t.wg.Add(1) // under mu, so it cannot race with stop's Wait
	stopCh := t.stopCh
	t.mu.Unlock()

	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		cancel()
		t.wg.Done()
	}
}

// stop cancels every in-flight task and waits for them to return until ctx is
// done. It reports whether all of them returned in time.
func (t *scheduledTaskTracker) stop(ctx context.Context) bool {
	t.mu.Lock()
	if t.stopCh == nil {
		t.stopCh = make(chan struct{})
	}
	if !t.stopped {
		t.stopped = true
		close(t.stopCh)
	}
	t.mu.Unlock()

	finished := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return true
	case <-ctx.Done():
		return false
	}
}

// ScheduledTaskFailedLogMarker is the stable token logged whenever a scheduled
// task returns an error, including a partial failure (issue #714). The
// per-cloud monitoring modules alert on any occurrence, so it is a contract
// with terraform/modules/monitoring/{aws,gcp,azure}: rename it only together
// with those filters (TestMonitoringLogMarkersMatchGo guards the pairing).
const ScheduledTaskFailedLogMarker = "scheduled_task_failed"

// ErrScheduledPurchasesFailed marks a process_scheduled_purchases run in which
// at least one execution failed. The run returns it instead of success so the
// scheduler (EventBridge/Lambda, Cloud Scheduler, Logic Apps) records a failed
// invocation and the HTTP path answers 500 rather than 200 (issue #714).
var ErrScheduledPurchasesFailed = errors.New("scheduled purchases failed")

// HandleScheduledTask processes a scheduled task by type and logs
// ScheduledTaskFailedLogMarker when it fails, whichever entry point (Lambda
// event, /api/scheduled/ HTTP, CLI) invoked it.
func (app *Application) HandleScheduledTask(ctx context.Context, taskType ScheduledTaskType, params ScheduledTaskParams) (any, error) {
	result, err := app.runScheduledTask(ctx, taskType, params)
	if err != nil {
		log.Printf("%s task=%q error=%q", ScheduledTaskFailedLogMarker, taskType, err.Error()) // #nosec G706 -- taskType comes from a known-value map and both values are %q-quoted, which escapes CR/LF
	}
	return result, err
}
