package purchase

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #1195: every writer that ends an execution row failed or
// partially_completed must raise an alert signal: the PurchaseFailedLogMarker
// line the monitoring modules alarm on. These tests pin the marker per writer,
// including a fan-out root whose children never reached their own notification.

// markerLog captures the logging package's output; the fan-out logs from
// goroutines, so writes are serialized.
type markerLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *markerLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// count returns how many purchase-failed markers were logged for executionID.
func (l *markerLog) count(executionID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), PurchaseFailedLogMarker+" execution_id="+executionID+" ")
}

func captureMarkers(t *testing.T) *markerLog {
	t.Helper()
	l := &markerLog{}
	prev := logging.SetOutput(l)
	t.Cleanup(func() { logging.SetOutput(prev) })
	return l
}

func TestLogExecutionFailure_LogsMarkerOnlyForFailedOrPartialRows(t *testing.T) {
	logs := captureMarkers(t)
	for _, status := range []string{"failed", "partially_completed", "completed", "running", "approved", "pending", ""} {
		id := "exec-" + status
		logged := logExecutionFailure(&config.PurchaseExecution{ExecutionID: id, PlanID: "plan-1", Status: status})
		want := status == "failed" || status == "partially_completed"
		assert.Equal(t, want, logged, "status %q", status)
		assert.Equal(t, map[bool]int{true: 1, false: 0}[want], logs.count(id), "status %q", status)
	}
	assert.Contains(t, logs.buf.String(), PurchaseFailedLogMarker+" execution_id=exec-failed plan_id=plan-1 status=failed")
}

// A fan-out root logs the marker but sends no email, whatever happened to the
// children: a skipped child (the fan-out records ctx.Err() without running it)
// and a recovered panic both leave no notified child row behind.
func TestNotifyRootFailure_FanOutRootLogsMarkerWithoutEmail(t *testing.T) {
	cases := map[string]struct {
		execErr error
		status  string
	}{
		"skipped child": {
			fmt.Errorf("%w: account a: %w", errAllAccountsFailed, context.DeadlineExceeded), "failed",
		},
		"panicked child beside a committed one": {
			&multiAccountPartialError{committed: 1, errors: []string{"account b: panic during fan-out (account=b): boom"}},
			"partially_completed",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureMarkers(t)
			sender := new(MockEmailSender)
			m := &Manager{config: new(MockConfigStore), email: sender}
			root := &config.PurchaseExecution{ExecutionID: "root", Status: "running"}
			m.finalizeExecution(root, tc.execErr)
			require.Equal(t, tc.status, root.Status)

			m.notifyRootFailure(context.Background(), root, tc.execErr)

			assert.Equal(t, 1, logs.count("root"))
			sender.AssertNotCalled(t, "SendPurchaseFailedNotification", mock.Anything, mock.Anything)
		})
	}

	t.Run("non fan-out root emails too", func(t *testing.T) {
		logs := captureMarkers(t)
		sender := allowFailedNotification(new(MockEmailSender))
		m := &Manager{config: new(MockConfigStore), email: sender}
		root := &config.PurchaseExecution{ExecutionID: "root", Status: "failed"}
		m.notifyRootFailure(context.Background(), root, errors.New("provider down"))
		assert.Equal(t, 1, logs.count("root"))
		sender.AssertNumberOfCalls(t, "SendPurchaseFailedNotification", 1)
	})
}

// fanOutRun drives a two-account plan through the SQS executor. Account
// "acct-ok" buys; the other account's behavior is set by the caller through the
// credential store and the save hook. It returns the root's saved status and
// every purchase-failed email.
func fanOutRun(t *testing.T, other config.CloudAccount, loadRaw func(string) ([]byte, error), saveErr func(*config.PurchaseExecution) error) (string, []string) {
	t.Helper()
	ctx := context.Background()
	store := new(MockConfigStore)
	sender := new(MockEmailSender)
	rec := &failedEmailRecorder{}
	rec.expect(sender)
	factory, prov, svc := new(MockProviderFactory), new(MockProvider), new(MockServiceClient)

	accounts := []config.CloudAccount{
		{ID: "acct-ok", Name: "OK", Provider: "aws", ExternalID: "111111111111", AWSAuthMode: "access_keys"},
		other,
	}
	exec := &config.PurchaseExecution{
		ExecutionID: "root-1195", IdempotencyKey: "lineage-1195", Status: "notified", PlanID: "plan-x",
		Recommendations: []config.RecommendationRecord{scopedTestRec("acct-ok"), scopedTestRec(other.ID)},
	}
	plan := &config.PurchasePlan{ID: "plan-x", Name: "Plan X", AutoPurchase: true, MaxStepCommitment: capOf(1e9)}

	store.On("GetGlobalConfig", mock.Anything).Return(failureNotifyCfg(), nil)
	store.On("GetExecutionByID", ctx, "root-1195").Return(exec, nil)
	running := *exec
	running.Status = "running"
	store.On("TransitionExecutionStatus", ctx, "root-1195",
		[]string{"approved", "pending", "notified"}, "running", (*string)(nil)).Return(&running, nil)
	store.On("GetPurchasePlan", mock.Anything, "plan-x").Return(plan, nil).Maybe()
	store.GetPlanAccountsFn = func(context.Context, string) ([]config.CloudAccount, error) { return accounts, nil }
	store.On("CompletePlanStep", ctx, "plan-x", mock.Anything).Return(nil).Maybe()
	var mu sync.Mutex
	var rootStatus string
	store.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
		mu.Lock()
		defer mu.Unlock()
		if e.CloudAccountID == nil {
			rootStatus = e.Status
			return nil
		}
		return saveErr(e)
	}
	store.On("SavePurchaseHistory", mock.Anything, mock.Anything).Return(nil)
	sender.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil)
	factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(prov, nil)
	prov.On("GetServiceClient", mock.Anything, common.ServiceEC2, mock.Anything).Return(svc, nil)
	svc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-ok"}, nil).Once()
	keys := awsAccessKeyCredStore()
	creds := &MockCredentialStore{LoadRawFn: func(ctx context.Context, accountID, credType string) ([]byte, error) {
		if accountID == other.ID {
			return loadRaw(accountID)
		}
		return keys.LoadRawFn(ctx, accountID, credType)
	}}

	m := &Manager{config: store, email: sender, providerFactory: factory, credStore: creds}
	require.NoError(t, m.handleExecutePurchase(ctx, &AsyncMessage{Type: MessageTypeExecutePurchase, ExecutionID: "root-1195"}))

	mu.Lock()
	defer mu.Unlock()
	sent := rec.all()
	emailed := make([]string, 0, len(sent))
	for _, d := range sent {
		emailed = append(emailed, d.ExecutionID)
	}
	return rootStatus, emailed
}

// A child that panics is recovered by the fan-out before it stamps or saves
// its row, so its own deferred notification is a no-op. Before #1195 the
// partially_completed root was skipped as well and nothing alerted.
func TestFanOutRootAlertsWhenPanickedChildNeverNotifies(t *testing.T) {
	logs := captureMarkers(t)
	panicky := config.CloudAccount{ID: "acct-panic", Name: "PANIC", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "access_keys"}
	rootStatus, emailed := fanOutRun(t, panicky,
		func(string) ([]byte, error) { panic("credential store blew up") },
		func(*config.PurchaseExecution) error { return nil })

	assert.Equal(t, "partially_completed", rootStatus)
	assert.Equal(t, 1, logs.count("root-1195"), "the root's marker is the one alert signal for the panicked child")
	assert.Empty(t, emailed, "the root sends no email; the panicked child had no row to email for")
}

// A child whose failed row cannot be saved (AUDIT LOSS) still notifies for
// itself; the root adds its marker without a second email.
func TestFanOutAuditLossChildStillNotifiesAndRootAddsMarker(t *testing.T) {
	logs := captureMarkers(t)
	// role_arn with no STS client wired: credential resolution fails.
	bad := config.CloudAccount{ID: "acct-bad", Name: "BAD", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "role_arn"}
	var childID string
	rootStatus, emailed := fanOutRun(t, bad,
		func(string) ([]byte, error) { return nil, errors.New("unused") },
		func(e *config.PurchaseExecution) error {
			if e.Status == "failed" {
				childID = e.ExecutionID
				return errors.New("db down")
			}
			return nil
		})

	assert.Equal(t, "partially_completed", rootStatus)
	require.NotEmpty(t, childID)
	assert.Equal(t, 1, logs.count(childID), "the child alerts for itself")
	assert.Equal(t, []string{childID}, emailed, "one email, for the child, none for the root")
	assert.Equal(t, 1, logs.count("root-1195"))
}
