package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	azurecompute "github.com/LeanerCloud/cloud-commitments-go/providers/azure/services/compute"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// --- RI exchange execution outlives the request context (#790) ---

// ctxProbe records what the provider call saw of its context at the moment the
// request was canceled underneath it.
type ctxProbe struct {
	errAfterCancel error
	deadline       time.Time
	hasDeadline    bool
}

// observe cancels the request (the client disconnecting, or the server's 30s
// per-request timeout firing, while the provider call is in flight) and then
// records whether that cancellation reached the provider call's context.
func (p *ctxProbe) observe(ctx context.Context, cancelRequest context.CancelFunc) {
	cancelRequest()
	p.errAfterCancel = ctx.Err()
	p.deadline, p.hasDeadline = ctx.Deadline()
}

func (p *ctxProbe) assertDetached(t *testing.T, start time.Time) {
	t.Helper()
	require.NoError(t, p.errAfterCancel,
		"canceling the request must not cancel an exchange already submitted to the provider")
	require.True(t, p.hasDeadline, "the detached exchange must still be bounded by its own timeout")
	// A literal rather than riExchangeExecutionTimeout, so the test pins the
	// budget itself: it must stay inside the documented lambda_timeout >= 270s
	// requirement (4m execution + 30s terminal save).
	assert.WithinDuration(t, start.Add(4*time.Minute), p.deadline, 5*time.Second)
}

// liveCtx matches a context that has not been canceled and carries the 30s
// terminal-save budget, so a mock expectation fails when a terminal write is
// attempted on the dead request context, as a real database driver would.
func liveCtx() any {
	return mock.MatchedBy(func(ctx context.Context) bool {
		deadline, ok := ctx.Deadline()
		return ctx.Err() == nil && ok && time.Until(deadline) <= 30*time.Second
	})
}

func approvedExchangeStore(t *testing.T, id string) *MockConfigStore {
	t.Helper()
	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD: 1000, RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	store.On("ReserveApprovedRIExchange", mock.Anything, id, "1000.000000", "500.000000").Return("500.000000", nil)
	return store
}

func approvedExchangeRecord(id string) *config.RIExchangeRecord {
	return &config.RIExchangeRecord{
		ID: id, Region: "us-east-1", SourceRIIDs: []string{"ri-1"}, TargetOfferingID: "o", TargetCount: 1, PaymentDue: "40.00",
	}
}

// TestExecuteApprovedExchange_RequestCancelDoesNotCancelExchangeOrLedger: AWS
// accepted the exchange, the approver's request was canceled mid-call, and the
// ledger write recording the money movement must still land.
func TestExecuteApprovedExchange_RequestCancelDoesNotCancelExchangeOrLedger(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-000000000790"
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()

	store := approvedExchangeStore(t, id)
	store.On("CompleteRIExchangeWithPayment", liveCtx(), id, "exch-790", "40.000000").Return(nil).Once()

	var probe ctxProbe
	h := &Handler{config: store, executeExchangeFn: func(ctx context.Context, _ exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		probe.observe(ctx, cancelRequest)
		return "exch-790", &exchange.ExchangeQuoteSummary{PaymentDueUSDStr: "40.000000"}, nil
	}}

	start := time.Now()
	resp, err := h.executeApprovedExchange(reqCtx, id, approvedExchangeRecord(id))
	require.NoError(t, err)
	probe.assertDetached(t, start)
	assert.Equal(t, "completed", resp.(map[string]any)["status"])
}

// TestExecuteApprovedExchange_RequestCancelStillRecordsFailure: the failure
// branch's terminal write must also survive the canceled request, or the
// record stays "processing" with its ceiling reserved.
func TestExecuteApprovedExchange_RequestCancelStillRecordsFailure(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-000000000791"
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()

	store := approvedExchangeStore(t, id)
	store.On("FailRIExchange", liveCtx(), id, "quote no longer valid").Return(nil).Once()

	h := &Handler{config: store, executeExchangeFn: func(_ context.Context, _ exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		cancelRequest()
		return "", nil, errors.New("quote no longer valid")
	}}

	resp, err := h.executeApprovedExchange(reqCtx, id, approvedExchangeRecord(id))
	require.NoError(t, err)
	assert.Equal(t, "failed", resp.(map[string]any)["status"])
}

// TestExecuteExchange_RequestCancelDoesNotCancelAWSExchange covers the direct
// AWS execute endpoint the issue names.
func TestExecuteExchange_RequestCancelDoesNotCancelAWSExchange(t *testing.T) {
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()

	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "user-1"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "user-1", "execute", "ri-exchange").Return(true, nil)
	allowAnyAccountScope(mockAuth)
	mockAuth.On("HasPermissionForConstraintsAPI", mock.Anything, "user-1", "execute", "ri-exchange", mock.Anything).Return(true, nil)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	var probe ctxProbe
	h := &Handler{
		auth:                   mockAuth,
		config:                 newExchangeClaimLedger(),
		reshapeAccountResolver: func(context.Context) (string, error) { return "11111111-2222-3333-4444-555555555555", nil },
		executeExchangeFn: func(ctx context.Context, _ exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
			probe.observe(ctx, cancelRequest)
			return "exch-direct", &exchange.ExchangeQuoteSummary{PaymentDueUSDStr: "10.000000"}, nil
		},
	}

	start := time.Now()
	res, err := h.executeExchange(reqCtx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"authorization": "Bearer tok"},
		Body:    `{"ri_ids":["ri-123"],"target_offering_id":"off-1","target_count":1,"max_payment_due_usd":"250.50","region":"eu-central-1"}`,
	})
	require.NoError(t, err)
	probe.assertDetached(t, start)
	assert.Equal(t, "exch-direct", res.(*ExchangeExecuteResponse).ExchangeID)
}

// TestExecuteAzureExchange_RequestCancelDoesNotCancelExchange covers the Azure
// execute endpoint, whose ExecuteExchange polls a long-running operation and is
// the most exposed to the 30s request timeout.
func TestExecuteAzureExchange_RequestCancelDoesNotCancelExchange(t *testing.T) {
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()

	var probe ctxProbe
	opsClient := new(mockAzureExchangeOpsClient)
	ownsAzureSource(opsClient)
	opsClient.On("CalculateExchange", mock.Anything, mock.Anything, mock.Anything).Return(
		&azurecompute.ExchangePreview{SessionID: "sess-790", NetPayable: toPtr(75.00), NetPayableCurrency: "USD"},
		[]azurecompute.CompatibleOffering{}, nil,
	)
	opsClient.On("ExecuteExchange", mock.Anything, "sess-790").Run(func(args mock.Arguments) {
		probe.observe(args.Get(0).(context.Context), cancelRequest)
	}).Return(&azurecompute.ExchangeResult{SessionID: "sess-790", Status: "Succeeded"}, nil).Once()
	t.Cleanup(func() { opsClient.AssertExpectations(t) })

	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "user-1"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "user-1", "execute", "ri-exchange").Return(true, nil)
	allowAnyAccountScope(mockAuth)
	mockAuth.On("HasPermissionForConstraintsAPI", mock.Anything, "user-1", "execute", "ri-exchange", mock.Anything).Return(true, nil)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	ledger := newExchangeClaimLedger()
	ledger.GetCloudAccountByExternalIDFn = func(context.Context, string, string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: "acct-1"}, nil
	}
	h := &Handler{
		auth:                 mockAuth,
		config:               ledger,
		azureExchangeFactory: func(string) azureExchangeClient { return opsClient },
	}

	start := time.Now()
	res, err := h.executeAzureExchange(reqCtx, azureExecuteRequest(validAzureExecuteBody))
	require.NoError(t, err)
	probe.assertDetached(t, start)
	assert.Equal(t, "Succeeded", res.(*AzureExecuteExchangeResponse).Status)
}

// TestExecuteApprovedExchange_RequestCancelBeforeReserveRecordsFailure covers
// the window between the pending -> processing transition and the provider
// call. A cancel there fails the reservation; the failure write must still land,
// or the record is stranded in "processing": the approval retry gets 409 from
// the transition guard and the row keeps counting against the daily cap.
func TestExecuteApprovedExchange_RequestCancelBeforeReserveRecordsFailure(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-000000000792"
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()

	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{
		RIExchangeMaxDailyUSD: 1000, RIExchangeMaxPerExchangeUSD: 500,
	}, nil)
	store.On("ReserveApprovedRIExchange", mock.Anything, id, "1000.000000", "500.000000").
		Run(func(mock.Arguments) { cancelRequest() }).
		Return("", context.Canceled).Once()
	store.On("FailRIExchange", liveCtx(), id, "daily spending cap check failed").Return(nil).Once()

	called := false
	h := &Handler{config: store, executeExchangeFn: func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		called = true
		return "", nil, nil
	}}

	resp, err := h.executeApprovedExchange(reqCtx, id, approvedExchangeRecord(id))
	require.NoError(t, err)
	assert.Equal(t, "failed", resp.(map[string]any)["status"])
	assert.False(t, called, "a failed reservation must never reach the provider")
}

// TestStampRIExchangeApprover_SurvivesRequestCancel pins the approved_by audit
// write, which runs after money moved, to a detached context.
func TestStampRIExchangeApprover_SurvivesRequestCancel(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-000000000793"
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()

	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("StampRIExchangeApprovedBy", liveCtx(), id, "approver@example.com").Return(nil).Once()

	h := &Handler{config: store}
	h.stampRIExchangeApprover(reqCtx, id, &Session{Email: "approver@example.com"})
}
