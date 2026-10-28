package purchase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #714: a failed purchase reached no operator. These tests pin that every
// execution row ending failed or partially_completed sends the purchase-failed
// email, once, listing exactly the selected recommendations that did not buy.

const failureNotifyRecipient = "ops@example.com"

func failureNotifyCfg() *config.GlobalConfig {
	notify := failureNotifyRecipient
	return &config.GlobalConfig{AutoPurchaseEnabled: true, NotificationEmail: &notify}
}

// failedEmailRecorder captures every purchase-failed email a mock sender gets.
type failedEmailRecorder struct {
	mu   sync.Mutex
	sent []email.NotificationData
}

func (r *failedEmailRecorder) expect(sender *MockEmailSender) *mock.Call {
	return sender.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.sent = append(r.sent, args.Get(1).(email.NotificationData))
		}).Return(nil)
}

func (r *failedEmailRecorder) all() []email.NotificationData {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]email.NotificationData(nil), r.sent...)
}

func TestNotifyExecutionFailure_FailedRowEmailsUnpurchasedSelectedRecs(t *testing.T) {
	store := new(MockConfigStore)
	store.On("GetGlobalConfig", mock.Anything).Return(failureNotifyCfg(), nil)
	sender := new(MockEmailSender)
	assertMocksOnCleanup(t, store, sender)
	rec := &failedEmailRecorder{}
	rec.expect(sender)
	m := &Manager{config: store, email: sender, dashboardURL: "https://dashboard.example.com/"}

	exec := &config.PurchaseExecution{
		ExecutionID: "exec-1",
		PlanID:      "plan-1",
		Status:      "partially_completed",
		Recommendations: []config.RecommendationRecord{
			{Service: "ec2", ResourceType: "m5.large", Region: "us-east-1", Count: 2, Selected: true, Purchased: true},
			{Service: "rds", ResourceType: "db.r5.large", Engine: "postgres", Region: "eu-west-1", Count: 1, Selected: true},
			{Service: "ec2", ResourceType: "c5.large", Region: "us-east-1", Count: 3},
		},
	}
	m.notifyExecutionFailure(context.Background(), exec)

	sent := rec.all()
	require.Len(t, sent, 1)
	got := sent[0]
	assert.Equal(t, failureNotifyRecipient, got.RecipientEmail, "the email goes to the global notification address")
	assert.Equal(t, "exec-1", got.ExecutionID)
	assert.Equal(t, "plan-1", got.PlanID)
	assert.Equal(t, "https://dashboard.example.com", got.DashboardURL)
	require.Len(t, got.Recommendations, 1, "only the selected rec that did not purchase is listed")
	assert.Equal(t, email.RecommendationSummary{
		Service: "rds", ResourceType: "db.r5.large", Engine: "postgres", Region: "eu-west-1", Count: 1,
	}, got.Recommendations[0])
}

func TestNotifyExecutionFailure_OnlyFailedOrPartialRowsNotify(t *testing.T) {
	for _, status := range []string{"completed", "running", "approved", "pending", ""} {
		t.Run(fmt.Sprintf("status=%q", status), func(t *testing.T) {
			sender := new(MockEmailSender)
			assertMocksOnCleanup(t, sender)
			// Must NOT fire (AssertNotCalled below); .Maybe() keeps a regression
			// an assertion failure instead of a mock panic.
			sender.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).Return(nil).Maybe()
			m := &Manager{config: new(MockConfigStore), email: sender}
			m.notifyExecutionFailure(context.Background(), &config.PurchaseExecution{ExecutionID: "e", Status: status})
			sender.AssertNotCalled(t, "SendPurchaseFailedNotification", mock.Anything, mock.Anything)
		})
	}
	for _, status := range []string{"failed", "partially_completed"} {
		t.Run(fmt.Sprintf("status=%q", status), func(t *testing.T) {
			sender := new(MockEmailSender)
			assertMocksOnCleanup(t, sender)
			sender.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).Return(nil)
			m := &Manager{config: new(MockConfigStore), email: sender}
			m.notifyExecutionFailure(context.Background(), &config.PurchaseExecution{ExecutionID: "e", Status: status})
			sender.AssertNumberOfCalls(t, "SendPurchaseFailedNotification", 1)
		})
	}
}

// A send failure is logged, never propagated: the purchase outcome stands.
func TestNotifyExecutionFailure_SendErrorAndNilSenderAreBestEffort(t *testing.T) {
	sender := new(MockEmailSender)
	assertMocksOnCleanup(t, sender)
	sender.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).Return(errors.New("ses down"))
	m := &Manager{config: new(MockConfigStore), email: sender}
	exec := &config.PurchaseExecution{ExecutionID: "e", Status: "failed", Error: "boom"}
	m.notifyExecutionFailure(context.Background(), exec)
	sender.AssertNumberOfCalls(t, "SendPurchaseFailedNotification", 1)
	assert.Equal(t, "failed", exec.Status)
	assert.Equal(t, "boom", exec.Error)

	noEmail := &Manager{config: new(MockConfigStore)}
	assert.NotPanics(t, func() { noEmail.notifyExecutionFailure(context.Background(), exec) })
}

// The detached context lets the notice go out even when the caller's context
// is already canceled (a disconnected approver or an exhausted budget).
func TestNotifyExecutionFailure_SurvivesCanceledCallerContext(t *testing.T) {
	sender := new(MockEmailSender)
	assertMocksOnCleanup(t, sender)
	var sendCtxErr error
	sender.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { sendCtxErr = args.Get(0).(context.Context).Err() }).Return(nil)
	m := &Manager{config: new(MockConfigStore), email: sender}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.notifyExecutionFailure(ctx, &config.PurchaseExecution{ExecutionID: "e", Status: "failed"})
	sender.AssertNumberOfCalls(t, "SendPurchaseFailedNotification", 1)
	assert.NoError(t, sendCtxErr)
}

func TestIsFanOutOutcome(t *testing.T) {
	assert.False(t, isFanOutOutcome(nil))
	assert.False(t, isFanOutOutcome(errors.New("plan not found")))
	assert.False(t, isFanOutOutcome(beforeSubmission(errors.New("scope"))))
	assert.False(t, isFanOutOutcome(&partialPurchaseError{errors: []string{"x"}}))
	assert.True(t, isFanOutOutcome(fmt.Errorf("%w: account a: x", errAllAccountsFailed)))
	assert.True(t, isFanOutOutcome(&multiAccountPartialError{committed: 1, errors: []string{"x"}}))
	assert.False(t, isFanOutOutcome(fmt.Errorf("%w: wrapped", config.ErrAuditLoss)))
}

// End to end through the SQS executor: a multi-account run where one account
// buys and one fails sends exactly one purchase-failed email, for the failed
// account's row, and none for the aggregate root row.
func TestMultiAccountFailureNotifiesOncePerFailedAccountNotRoot(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	sender := new(MockEmailSender)
	rec := &failedEmailRecorder{}
	rec.expect(sender)
	factory, prov, svc := new(MockProviderFactory), new(MockProvider), new(MockServiceClient)
	assertMocksOnCleanup(t, store, sender, factory, prov, svc)

	accounts := []config.CloudAccount{
		{ID: "acct-ok", Name: "OK", Provider: "aws", ExternalID: "111111111111", AWSAuthMode: "access_keys"},
		// role_arn with no STS client wired: credential resolution fails.
		{ID: "acct-bad", Name: "BAD", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "role_arn"},
	}
	exec := &config.PurchaseExecution{
		ExecutionID: "root-714", IdempotencyKey: "lineage-714", Status: "notified", PlanID: "plan-x",
		Recommendations: []config.RecommendationRecord{scopedTestRec("acct-ok"), scopedTestRec("acct-bad")},
	}
	plan := &config.PurchasePlan{ID: "plan-x", Name: "Plan X", AutoPurchase: true, MaxStepCommitment: capOf(1e9)}

	store.On("GetGlobalConfig", mock.Anything).Return(failureNotifyCfg(), nil)
	store.On("GetExecutionByID", ctx, "root-714").Return(exec, nil)
	running := *exec
	running.Status = "running"
	store.On("TransitionExecutionStatus", ctx, "root-714",
		[]string{"approved", "pending", "notified"}, "running", (*string)(nil)).Return(&running, nil)
	store.On("GetPurchasePlan", mock.Anything, "plan-x").Return(plan, nil).Maybe()
	store.GetPlanAccountsFn = func(context.Context, string) ([]config.CloudAccount, error) { return accounts, nil }
	store.On("CompletePlanStep", ctx, "plan-x", mock.Anything).Return(nil).Maybe()
	var mu sync.Mutex
	failedRows := map[string]string{} // execution id -> account id, for rows saved failed
	var rootStatus string
	store.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
		mu.Lock()
		defer mu.Unlock()
		if e.CloudAccountID == nil {
			rootStatus = e.Status
		} else if e.Status == "failed" {
			failedRows[e.ExecutionID] = *e.CloudAccountID
		}
		return nil
	}
	store.On("SavePurchaseHistory", mock.Anything, mock.Anything).Return(nil)
	sender.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil)
	factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(prov, nil)
	prov.On("GetServiceClient", mock.Anything, common.ServiceEC2, mock.Anything).Return(svc, nil)
	svc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-ok"}, nil).Once()

	m := &Manager{config: store, email: sender, providerFactory: factory, credStore: awsAccessKeyCredStore()}
	require.NoError(t, m.handleExecutePurchase(ctx, &AsyncMessage{Type: MessageTypeExecutePurchase, ExecutionID: "root-714"}))

	assert.Equal(t, "partially_completed", rootStatus)
	require.Len(t, failedRows, 1)
	sent := rec.all()
	require.Len(t, sent, 1, "one email for the failed account row, none for the aggregate root")
	assert.NotEqual(t, "root-714", sent[0].ExecutionID)
	assert.Equal(t, "acct-bad", failedRows[sent[0].ExecutionID], "the email is for the failed account's row")
	assert.Equal(t, failureNotifyRecipient, sent[0].RecipientEmail)
	require.Len(t, sent[0].Recommendations, 1)
}
