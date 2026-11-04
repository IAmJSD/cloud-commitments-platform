package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	azurecompute "github.com/LeanerCloud/cloud-commitments-go/providers/azure/services/compute"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// --- Direct-execute RI exchange audit trail (issue #82) ---
//
// Before #82 neither executeExchange (AWS) nor executeAzureExchange wrote any
// record of an exchange executed directly through the API. These tests drive
// both handlers end to end against a recording store and pin that every
// direct execute leaves a row, what the row says, and when it is written.

const (
	auditUserID       = "0b7c6a1e-4f7d-4c55-9d2b-7e1f0c3a9abc"
	auditUserEmail    = "alice@example.com"
	auditCloudAccount = "c1a0d2e3-5b6f-4a7b-8c9d-0e1f2a3bcdef"
	auditOfferingA    = "4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91"
	auditOfferingB    = "7c3394c5-6acd-4128-8d86-e6bae4bb9da2"
)

// finalizeCtxState is what the outcome write saw of its context while it ran;
// the handler cancels that context once the write returns.
type finalizeCtxState struct {
	err         error
	deadline    time.Time
	hasDeadline bool
}

// directExchangeAuditStore records the audit trail writes, and the order in
// which the handler reached the audit insert, the submit claim, the provider
// commit and the outcome write.
type directExchangeAuditStore struct {
	*exchangeClaimLedger

	mu          sync.Mutex
	steps       []string
	saved       []config.RIExchangeDirectExecution
	outcomes    map[string]config.RIExchangeDirectOutcome
	finalizeCtx []finalizeCtxState
	saveErr     error
	finalizeErr error
}

func newDirectExchangeAuditStore() *directExchangeAuditStore {
	return &directExchangeAuditStore{
		exchangeClaimLedger: newExchangeClaimLedger(),
		outcomes:            map[string]config.RIExchangeDirectOutcome{},
	}
}

func (s *directExchangeAuditStore) step(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, name)
}

func (s *directExchangeAuditStore) SaveRIExchangeDirectExecution(_ context.Context, record *config.RIExchangeDirectExecution) error {
	s.step("save")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, *record)
	return nil
}

func (s *directExchangeAuditStore) FinalizeRIExchangeDirectExecution(ctx context.Context, id string, outcome config.RIExchangeDirectOutcome) error {
	s.step("finalize")
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, hasDeadline := ctx.Deadline()
	s.finalizeCtx = append(s.finalizeCtx, finalizeCtxState{err: ctx.Err(), deadline: deadline, hasDeadline: hasDeadline})
	if s.finalizeErr != nil {
		return s.finalizeErr
	}
	if _, done := s.outcomes[id]; done {
		return config.ErrRIExchangeDirectExecutionNotSubmitted
	}
	s.outcomes[id] = outcome
	return nil
}

func (s *directExchangeAuditStore) ClaimRIExchangeIdempotencyKey(ctx context.Context, key string, window time.Duration) (bool, error) {
	s.step("claim")
	return s.exchangeClaimLedger.ClaimRIExchangeIdempotencyKey(ctx, key, window)
}

// onlyRow returns the single audit row and its outcome, failing the test when
// the handler wrote any other number of rows.
func (s *directExchangeAuditStore) onlyRow(t *testing.T) (config.RIExchangeDirectExecution, config.RIExchangeDirectOutcome, bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Len(t, s.saved, 1, "a direct execute must write exactly one audit row")
	outcome, finalized := s.outcomes[s.saved[0].ID]
	return s.saved[0], outcome, finalized
}

func (s *directExchangeAuditStore) recordedSteps() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.steps...)
}

func auditSessionAuth(t *testing.T) *MockAuthService {
	t.Helper()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: auditUserID, Email: auditUserEmail}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, auditUserID, "execute", "ri-exchange").Return(true, nil)
	allowAnyAccountScope(mockAuth)
	mockAuth.On("HasPermissionForConstraintsAPI", mock.Anything, auditUserID, "execute", "ri-exchange", mock.Anything).Return(true, nil).Maybe()
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	return mockAuth
}

type awsExecutor = func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error)

func awsAuditHandler(t *testing.T, store *directExchangeAuditStore, executor awsExecutor) *Handler {
	t.Helper()
	return &Handler{
		auth:                   auditSessionAuth(t),
		config:                 store,
		reshapeAccountResolver: func(context.Context) (string, error) { return auditCloudAccount, nil },
		executeExchangeFn: func(ctx context.Context, req exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
			store.step("execute")
			return executor(ctx, req)
		},
	}
}

const awsAuditBody = `{
	"ri_ids": ["ri-0a1b2c3d", "ri-4e5f6a7b"],
	"targets": [{"offering_id": "` + auditOfferingA + `", "count": 2}, {"offering_id": "` + auditOfferingB + `", "count": 1}],
	"max_payment_due_usd": "500.00",
	"region": "eu-central-1"
}`

func auditRequest(ctx context.Context, body string) (context.Context, *events.LambdaFunctionURLRequest) {
	return ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"authorization": "Bearer tok"}, Body: body}
}

// TestExecuteExchange_DirectExecuteWritesAuditTrail is the #82 regression test
// for AWS: a successful direct execute leaves one audit row naming the
// account, region, sources, targets, cap and actor, finalized as completed
// with the exchange ID and the amount AWS accepted. Before the fix the store
// saw no write at all.
func TestExecuteExchange_DirectExecuteWritesAuditTrail(t *testing.T) {
	store := newDirectExchangeAuditStore()
	h := awsAuditHandler(t, store, func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		return "riex-0f1e2d3c", &exchange.ExchangeQuoteSummary{PaymentDueUSDStr: "40.250000", PaymentDueRaw: "40.25", CurrencyCode: "USD"}, nil
	})

	res, err := h.executeExchange(auditRequest(context.Background(), awsAuditBody))
	require.NoError(t, err)
	assert.Equal(t, "riex-0f1e2d3c", res.(*ExchangeExecuteResponse).ExchangeID)

	row, outcome, finalized := store.onlyRow(t)
	assert.Equal(t, "aws", row.Provider)
	require.NotNil(t, row.CloudAccountID)
	assert.Equal(t, auditCloudAccount, *row.CloudAccountID)
	assert.Nil(t, row.ProviderAccountID, "AWS exchanges in the deployment's own account")
	require.NotNil(t, row.Region)
	assert.Equal(t, "eu-central-1", *row.Region)
	assert.Equal(t, []string{"ri-0a1b2c3d", "ri-4e5f6a7b"}, row.SourceIDs)
	assert.JSONEq(t, `[{"offering_id":"`+auditOfferingA+`","count":2},{"offering_id":"`+auditOfferingB+`","count":1}]`, string(row.Targets))
	assert.Equal(t, "500", row.MaxPaymentDue)
	assert.Equal(t, "USD", row.MaxPaymentCurrency)
	require.NotNil(t, row.ExecutedByUserID)
	assert.Equal(t, auditUserID, *row.ExecutedByUserID)
	assert.Equal(t, auditUserEmail, row.ExecutedBy)
	assert.Nil(t, row.ExecutedByAPIKeyID)
	require.NoError(t, row.Validate(), "the handler must hand the store a row the table accepts")

	require.True(t, finalized, "the outcome must be recorded, not left submitted")
	assert.Equal(t, config.RIExchangeDirectStatusCompleted, outcome.Status)
	assert.Equal(t, "riex-0f1e2d3c", outcome.ProviderReference)
	require.NotNil(t, outcome.NetPayable)
	assert.Equal(t, "40.25", *outcome.NetPayable)
	assert.Equal(t, "USD", outcome.NetPayableCurrency)
	assert.Empty(t, outcome.Error)
}

// TestExecuteExchange_AuditRowWrittenBeforeClaimAndCommit pins the ordering
// the trail depends on: the row exists before the irreversible call (so a
// process that dies mid-exchange still leaves a "submitted" row) and before
// the submit claim (so a refused insert leaves no claim blocking a retry).
func TestExecuteExchange_AuditRowWrittenBeforeClaimAndCommit(t *testing.T) {
	store := newDirectExchangeAuditStore()
	h := awsAuditHandler(t, store, func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		return "riex-1", nil, nil
	})

	_, err := h.executeExchange(auditRequest(context.Background(), awsAuditBody))
	require.NoError(t, err)
	assert.Equal(t, []string{"save", "claim", "execute", "finalize"}, store.recordedSteps())
}

// TestExecuteExchange_AuditInsertFailureRefusesExchange: an exchange that
// cannot be recorded is not executed, and the refusal takes no submit claim,
// so the caller can retry as soon as the store recovers.
func TestExecuteExchange_AuditInsertFailureRefusesExchange(t *testing.T) {
	store := newDirectExchangeAuditStore()
	store.saveErr = errors.New("connection refused")
	h := awsAuditHandler(t, store, func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		t.Fatal("the exchange must not reach AWS when its audit row cannot be written")
		return "", nil, nil
	})

	_, err := h.executeExchange(auditRequest(context.Background(), awsAuditBody))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exchange not executed")
	assert.Equal(t, []string{"save"}, store.recordedSteps(), "no claim, no commit, no outcome after a failed insert")
	assert.Empty(t, store.claimedKeys())
}

// TestExecuteExchange_FailureOutcomeClassification: a provider client fault
// proves the exchange did not happen (failed); any other error does not, so
// the trail must not claim it failed (unknown).
func TestExecuteExchange_FailureOutcomeClassification(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		quote      *exchange.ExchangeQuoteSummary
		wantStatus string
		wantPay    *string
	}{
		{
			name:       "aws client fault",
			err:        &smithy.GenericAPIError{Code: "InvalidOfferingId", Message: "bad offering", Fault: smithy.FaultClient},
			wantStatus: config.RIExchangeDirectStatusFailed,
		},
		{
			name:       "transport failure",
			err:        errors.New("AcceptReservedInstancesExchangeQuote: context deadline exceeded"),
			wantStatus: config.RIExchangeDirectStatusUnknown,
		},
		{
			name:       "guardrail refusal keeps the quoted amount",
			err:        errors.New("payment due 900 exceeds max 500"),
			quote:      &exchange.ExchangeQuoteSummary{PaymentDueRaw: "900.000000", CurrencyCode: "USD"},
			wantStatus: config.RIExchangeDirectStatusUnknown,
			wantPay:    toPtr("900"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newDirectExchangeAuditStore()
			h := awsAuditHandler(t, store, func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
				return "", tt.quote, tt.err
			})

			_, err := h.executeExchange(auditRequest(context.Background(), awsAuditBody))
			require.Error(t, err)

			_, outcome, finalized := store.onlyRow(t)
			require.True(t, finalized)
			assert.Equal(t, tt.wantStatus, outcome.Status)
			assert.Equal(t, tt.err.Error(), outcome.Error, "the provider's own error is the audit evidence")
			assert.Equal(t, tt.wantPay, outcome.NetPayable)
		})
	}
}

// TestExecuteExchange_DuplicateSubmitRecordedAsRefused: the retry the #1642
// claim turns away is still an execute attempt, recorded as failed without
// ever reaching AWS.
func TestExecuteExchange_DuplicateSubmitRecordedAsRefused(t *testing.T) {
	store := newDirectExchangeAuditStore()
	store.holdAll = true
	h := awsAuditHandler(t, store, func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		t.Fatal("a refused duplicate must not reach AWS")
		return "", nil, nil
	})

	_, err := h.executeExchange(auditRequest(context.Background(), awsAuditBody))
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 409, ce.code)

	_, outcome, finalized := store.onlyRow(t)
	require.True(t, finalized)
	assert.Equal(t, config.RIExchangeDirectStatusFailed, outcome.Status)
	assert.Contains(t, outcome.Error, "not submitted to the provider")
}

// TestExecuteExchange_OutcomeRecordedAfterRequestCancel: the outcome write
// runs on a detached, bounded context, so a client that disconnects while AWS
// commits cannot leave the row "submitted". A finalize failure is logged and
// does not turn the committed exchange into an error response.
func TestExecuteExchange_OutcomeRecordedAfterRequestCancel(t *testing.T) {
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()

	store := newDirectExchangeAuditStore()
	store.finalizeErr = errors.New("connection reset")
	h := awsAuditHandler(t, store, func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		cancelRequest()
		return "riex-2", nil, nil
	})

	res, err := h.executeExchange(auditRequest(reqCtx, awsAuditBody))
	require.NoError(t, err, "the exchange committed; failing to record it must not report it as failed")
	assert.Equal(t, "riex-2", res.(*ExchangeExecuteResponse).ExchangeID)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.finalizeCtx, 1)
	seen := store.finalizeCtx[0]
	require.NoError(t, seen.err, "the outcome write must not inherit the canceled request context")
	require.True(t, seen.hasDeadline, "the detached outcome write must still be bounded")
	assert.LessOrEqual(t, time.Until(seen.deadline), riExchangeTerminalSaveTimeout)
}

// TestExecuteExchange_GateRefusalWritesNoAuditRow: only requests that reach
// the submit stage are recorded; a request refused by a gate executed nothing.
func TestExecuteExchange_GateRefusalWritesNoAuditRow(t *testing.T) {
	store := newDirectExchangeAuditStore()
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{RequireDifferentApprover: true}, nil)
	h := awsAuditHandler(t, store, func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
		t.Fatal("a 4-eyes refusal must not reach AWS")
		return "", nil, nil
	})

	_, err := h.executeExchange(auditRequest(context.Background(), awsAuditBody))
	require.Error(t, err)
	assert.Empty(t, store.recordedSteps())
}

// --- Azure ---

const azureAuditBody = `{
	"subscription_id": "sub-1",
	"sources": [{"reservation_id": "res-1", "quantity": 4}],
	"targets": [{"sku": "Standard_D8s_v3", "location": "eastus", "term": "P3Y", "quantity": 2}],
	"max_payment_due": "100.00",
	"currency": "USD"
}`

func azureAuditHandler(t *testing.T, store *directExchangeAuditStore, opsClient *mockAzureExchangeOpsClient) *Handler {
	t.Helper()
	opsClient.On("ListExchangeableReservations", mock.Anything).Return([]azurecompute.ExchangeableReservation{
		{ReservationID: "res-1", BillingScopeID: "/subscriptions/sub-1", Region: "eastus", Quantity: 10},
	}, nil).Maybe()
	opsClient.On("CalculateExchange", mock.Anything, mock.Anything, mock.Anything).Return(
		&azurecompute.ExchangePreview{SessionID: "sess-quoted", NetPayable: toPtr(-12.5), NetPayableCurrency: "USD"},
		[]azurecompute.CompatibleOffering{}, nil,
	).Maybe()
	t.Cleanup(func() { opsClient.AssertExpectations(t) })
	store.GetCloudAccountByExternalIDFn = func(_ context.Context, _, _ string) (*config.CloudAccount, error) {
		return &config.CloudAccount{ID: auditCloudAccount}, nil
	}
	return &Handler{
		auth:                 auditSessionAuth(t),
		config:               store,
		azureExchangeFactory: func(_ string) azureExchangeClient { return opsClient },
	}
}

// TestExecuteAzureExchange_DirectExecuteWritesAuditTrail is the #82
// regression test for Azure: the row carries the subscription, the per-source
// quantity (a partial exchange of a reservation), the targets with their
// term, the re-quote that cleared the guardrails (here a refund) and the
// session, and is finalized with Azure's own status.
func TestExecuteAzureExchange_DirectExecuteWritesAuditTrail(t *testing.T) {
	store := newDirectExchangeAuditStore()
	opsClient := new(mockAzureExchangeOpsClient)
	opsClient.On("ExecuteExchange", mock.Anything, "sess-quoted").Return(&azurecompute.ExchangeResult{
		SessionID: "sess-quoted", NetPayable: toPtr(-12.5), NetPayableCurrency: "USD", Status: "Succeeded",
	}, nil).Once()
	h := azureAuditHandler(t, store, opsClient)

	_, err := h.executeAzureExchange(auditRequest(context.Background(), azureAuditBody))
	require.NoError(t, err)

	row, outcome, finalized := store.onlyRow(t)
	assert.Equal(t, "azure", row.Provider)
	require.NotNil(t, row.CloudAccountID)
	assert.Equal(t, auditCloudAccount, *row.CloudAccountID)
	require.NotNil(t, row.ProviderAccountID)
	assert.Equal(t, "sub-1", *row.ProviderAccountID)
	assert.Nil(t, row.Region, "Azure target locations live in targets")
	assert.Equal(t, []string{"res-1"}, row.SourceIDs)
	assert.JSONEq(t, `[{"reservation_id":"res-1","quantity":4}]`, string(row.Sources))
	assert.JSONEq(t, `[{"sku":"Standard_D8s_v3","location":"eastus","term":"P3Y","quantity":2}]`, string(row.Targets))
	assert.Equal(t, "100", row.MaxPaymentDue)
	assert.Equal(t, "USD", row.MaxPaymentCurrency)
	require.NotNil(t, row.QuotedNetPayable)
	assert.Equal(t, "-12.5", *row.QuotedNetPayable)
	assert.Equal(t, "sess-quoted", row.ProviderReference)
	assert.Equal(t, auditUserEmail, row.ExecutedBy)
	require.NoError(t, row.Validate())

	require.True(t, finalized)
	assert.Equal(t, config.RIExchangeDirectStatusCompleted, outcome.Status)
	assert.Equal(t, "sess-quoted", outcome.ProviderReference)
	assert.Equal(t, "Succeeded", outcome.ProviderStatus)
	require.NotNil(t, outcome.NetPayable)
	assert.Equal(t, "-12.5", *outcome.NetPayable)
	assert.Equal(t, "USD", outcome.NetPayableCurrency)

	assert.Equal(t, []string{"save", "claim", "finalize"}, store.recordedSteps())
}

// TestExecuteAzureExchange_FailureOutcomeClassification mirrors the AWS
// classification, including the case the handler itself warns "may already
// have been submitted to Azure".
func TestExecuteAzureExchange_FailureOutcomeClassification(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus string
	}{
		{"azure client fault", &azcore.ResponseError{StatusCode: 409, ErrorCode: "ReservationAlreadyExchanged"}, config.RIExchangeDirectStatusFailed},
		{"poll timeout", fmt.Errorf("azure: ExecuteExchange: %w", context.DeadlineExceeded), config.RIExchangeDirectStatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newDirectExchangeAuditStore()
			opsClient := new(mockAzureExchangeOpsClient)
			opsClient.On("ExecuteExchange", mock.Anything, "sess-quoted").Return(nil, tt.err).Once()
			h := azureAuditHandler(t, store, opsClient)

			_, err := h.executeAzureExchange(auditRequest(context.Background(), azureAuditBody))
			require.Error(t, err)

			_, outcome, finalized := store.onlyRow(t)
			require.True(t, finalized)
			assert.Equal(t, tt.wantStatus, outcome.Status)
			assert.Equal(t, tt.err.Error(), outcome.Error)
			assert.Nil(t, outcome.NetPayable)
		})
	}
}

// TestExecuteAzureExchange_AuditInsertFailureRefusesExchange mirrors the AWS
// fail-closed test.
func TestExecuteAzureExchange_AuditInsertFailureRefusesExchange(t *testing.T) {
	store := newDirectExchangeAuditStore()
	store.saveErr = errors.New("connection refused")
	opsClient := new(mockAzureExchangeOpsClient)
	h := azureAuditHandler(t, store, opsClient)

	_, err := h.executeAzureExchange(auditRequest(context.Background(), azureAuditBody))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exchange not executed")
	opsClient.AssertNotCalled(t, "ExecuteExchange", mock.Anything, mock.Anything)
	assert.Empty(t, store.claimedKeys())
}

// --- helpers ---

func TestAuditDecimal(t *testing.T) {
	tests := map[string]string{
		"500.00":   "500",
		"100.5":    "100.5",
		"0x10":     "16",
		"1/3":      "0.333333",
		"1e3":      "1000",
		"0.000001": "0.000001",
	}
	for in, want := range tests {
		r, err := exchange.ParseDecimalRat(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, auditDecimal(r), in)
	}
	assert.Equal(t, "-2.5", auditDecimal(big.NewRat(-5, 2)))
}

func TestAuditFloatAndDecimalString(t *testing.T) {
	assert.Nil(t, auditFloat(nil))
	assert.Nil(t, auditFloat(toPtr(math.NaN())))
	assert.Nil(t, auditFloat(toPtr(math.Inf(1))))
	assert.Equal(t, toPtr("75"), auditFloat(toPtr(75.0)))
	assert.Nil(t, auditDecimalString(""))
	assert.Nil(t, auditDecimalString("n/a"))
	assert.Equal(t, toPtr("40.25"), auditDecimalString("40.250000"))
}

// TestDirectExchangeActor_AdminAndAPIKeySessions: the stateless admin API key
// session has a sentinel user ID that is not a user row, so only the textual
// identity is kept; a user API key session records the key alongside the
// user.
func TestDirectExchangeActor_AdminAndAPIKeySessions(t *testing.T) {
	var admin config.RIExchangeDirectExecution
	directExchangeActor(&admin, &Session{UserID: apiKeyAdminUserID})
	assert.Nil(t, admin.ExecutedByUserID)
	assert.Nil(t, admin.ExecutedByAPIKeyID)
	assert.Equal(t, apiKeyAdminUserID, admin.ExecutedBy)

	const keyID = "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8cabcd"
	var keyed config.RIExchangeDirectExecution
	directExchangeActor(&keyed, &Session{UserID: auditUserID, Email: auditUserEmail, UserAPIKeyID: keyID})
	require.NotNil(t, keyed.ExecutedByUserID)
	assert.Equal(t, auditUserID, *keyed.ExecutedByUserID)
	require.NotNil(t, keyed.ExecutedByAPIKeyID)
	assert.Equal(t, keyID, *keyed.ExecutedByAPIKeyID)
	assert.Equal(t, auditUserEmail, keyed.ExecutedBy)
}

// TestAWSDirectExchangeAudit_LegacySingletonTarget: the legacy
// target_offering_id/target_count request is recorded as the one-element
// target list it executes as, and an unattributed deployment as no account.
func TestAWSDirectExchangeAudit_LegacySingletonTarget(t *testing.T) {
	audit := awsDirectExchangeAudit(&Session{UserID: auditUserID, Email: auditUserEmail}, unattributedAccountConstraint,
		ExchangeExecuteRequestBody{RIIDs: []string{"ri-1"}, TargetOfferingID: auditOfferingA, TargetCount: 3, Region: "us-east-1"},
		big.NewRat(250, 1))
	assert.Nil(t, audit.CloudAccountID)
	var targets []ExchangeTargetBody
	require.NoError(t, json.Unmarshal(audit.Targets, &targets))
	assert.Equal(t, []ExchangeTargetBody{{OfferingID: auditOfferingA, Count: 3}}, targets)
	assert.Equal(t, "250", audit.MaxPaymentDue)
	require.NoError(t, audit.Validate())
}
