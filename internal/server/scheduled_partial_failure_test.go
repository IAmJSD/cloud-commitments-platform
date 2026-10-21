package server

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// Issue #714: a process_scheduled_purchases run in which some executions
// failed returned success, so the scheduler recorded success and no alarm
// could fire. It must now fail the invocation and log the alert marker.

// syncBuffer is a log sink safe for the concurrent writes a handler may make.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureServerLog(t *testing.T) *syncBuffer {
	t.Helper()
	orig := log.Writer()
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return buf
}

func partialFailureApp(result *purchase.ProcessResult) *Application {
	return &Application{
		Purchase: &testutil.MockPurchaseManager{
			ProcessScheduledPurchasesFunc: func(context.Context) (*purchase.ProcessResult, error) {
				return result, nil
			},
		},
	}
}

func TestProcessScheduledPurchases_PartialFailureFailsTheTask(t *testing.T) {
	logs := captureServerLog(t)
	app := partialFailureApp(&purchase.ProcessResult{
		Processed: 3, Executed: 2, Failed: 1, Errors: []string{"exec-9: provider rejected"},
	})

	got, err := app.HandleScheduledTask(context.Background(), TaskProcessScheduledPurchases, ScheduledTaskParams{})

	if !errors.Is(err, ErrScheduledPurchasesFailed) {
		t.Fatalf("err = %v, want ErrScheduledPurchasesFailed", err)
	}
	if !strings.Contains(err.Error(), "exec-9: provider rejected") {
		t.Errorf("error %q does not carry the per-execution failure", err)
	}
	res, ok := got.(*purchase.ProcessResult)
	if !ok || res.Failed != 1 || res.Executed != 2 {
		t.Errorf("result = %#v, want the counts returned alongside the error", got)
	}
	if line := logs.String(); !strings.Contains(line, ScheduledTaskFailedLogMarker+` task="process_scheduled_purchases"`) {
		t.Errorf("log does not carry the %s alert marker:\n%s", ScheduledTaskFailedLogMarker, line)
	}
}

func TestProcessScheduledPurchases_CleanRunStaysSuccessful(t *testing.T) {
	logs := captureServerLog(t)
	app := partialFailureApp(&purchase.ProcessResult{Processed: 2, Executed: 2})

	if _, err := app.HandleScheduledTask(context.Background(), TaskProcessScheduledPurchases, ScheduledTaskParams{}); err != nil {
		t.Fatalf("clean run returned %v", err)
	}
	if strings.Contains(logs.String(), ScheduledTaskFailedLogMarker) {
		t.Errorf("clean run logged the failure marker:\n%s", logs.String())
	}
}

// The HTTP path (Cloud Scheduler, Logic Apps) must answer non-2xx so the
// scheduler records the run as failed.
func TestHandleScheduledHTTP_PartialPurchaseFailureIs500(t *testing.T) {
	captureServerLog(t)
	app := partialFailureApp(&purchase.ProcessResult{Processed: 1, Failed: 1, Errors: []string{"exec-1: boom"}})

	req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/scheduled/process_scheduled_purchases", nil)
	w := httptest.NewRecorder()
	app.handleScheduledHTTP(w, req)

	testutil.AssertEqual(t, 500, w.Code)
}

// Every task failure carries the marker, not only purchase runs.
func TestHandleScheduledTask_AnyTaskErrorLogsMarker(t *testing.T) {
	logs := captureServerLog(t)
	app := &Application{
		Purchase: &testutil.MockPurchaseManager{
			SendUpcomingPurchaseNotificationsFunc: func(context.Context) (*purchase.NotificationResult, error) {
				return nil, errors.New("smtp down\ninjected")
			},
		},
	}

	if _, err := app.HandleScheduledTask(context.Background(), TaskSendNotifications, ScheduledTaskParams{}); err == nil {
		t.Fatal("want an error")
	}
	out := logs.String()
	if !strings.Contains(out, ScheduledTaskFailedLogMarker+` task="send_notifications" error="smtp down\ninjected"`) {
		t.Errorf("marker line missing or not %%q-escaped:\n%s", out)
	}
}
