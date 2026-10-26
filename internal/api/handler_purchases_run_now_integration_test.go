//go:build integration

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// runNowService buys every recommendation except the call numbered failOn.
type runNowService struct {
	fanoutServiceClient
	calls      atomic.Int32
	failOn     int32
	onPurchase func(context.Context)
}

func (s *runNowService) PurchaseCommitment(ctx context.Context, rec common.Recommendation, opts common.PurchaseOptions) (common.PurchaseResult, error) {
	call := s.calls.Add(1)
	if s.onPurchase != nil {
		s.onPurchase(ctx)
	}
	if err := ctx.Err(); err != nil {
		return common.PurchaseResult{}, err
	}
	if call == s.failOn {
		return common.PurchaseResult{}, errors.New("provider rejected the purchase")
	}
	return s.fanoutServiceClient.PurchaseCommitment(ctx, rec, opts)
}

type runNowProvider struct {
	fanoutProvider
	service *runNowService
}

func (p *runNowProvider) GetServiceClient(context.Context, common.ServiceType, string) (provider.ServiceClient, error) {
	return p.service, nil
}

type runNowFactory struct{ provider *runNowProvider }

func (f runNowFactory) CreateAndValidateProvider(context.Context, string, *provider.Config) (provider.Provider, error) {
	return f.provider, nil
}

// newRunNowFixture is the pause-claim fixture (real Postgres store, pending
// row on an auto-purchase plan) with the handler wired to a real manager whose
// provider fails on purchase call failOn (0 = never), run by the row's creator
// holding execute:purchases.
func newRunNowFixture(t *testing.T, failOn int32, recs []config.RecommendationRecord) (*pauseClaimFixture, *runNowService) {
	t.Helper()
	f := newPauseClaimFixture(t)
	for i := range recs {
		recs[i].CloudAccountID = f.execution.CloudAccountID
	}
	f.execution.Recommendations = recs
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, f.execution))
	service := &runNowService{failOn: failOn}
	mockAuth := new(MockAuthService)
	session := &Session{UserID: *f.execution.CreatedByUserID, Email: "creator@example.com"}
	mockAuth.On("ValidateSession", mock.Anything, "admin-token").Return(session, nil).Maybe()
	mockAuth.grantAdminPurchaser()
	f.handler.auth = mockAuth
	f.handler.purchase = purchase.NewManager(purchase.ManagerConfig{
		ConfigStore: f.store, EmailSender: &stubEmailNotifier{}, CredentialStore: &fanoutCredStore{},
		ProviderFactory: runNowFactory{provider: &runNowProvider{service: service}},
	})
	return f, service
}

func (f *pauseClaimFixture) runNow() (any, error) {
	return f.handler.runPlannedPurchase(f.ctx, f.request, f.execution.ExecutionID)
}

func twoRunNowRecs() []config.RecommendationRecord {
	recs := append(fanoutRecs(), fanoutRecs()...)
	recs[1].ResourceType = "m5.xlarge"
	return recs
}

func TestRunNow_StampsExecutedAuditFieldsAndReturnsRowStatus(t *testing.T) {
	f, service := newRunNowFixture(t, 0, fanoutRecs())
	before := time.Now()
	result, err := f.runNow()
	require.NoError(t, err)

	row, err := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", row.Status)
	assert.Equal(t, row.Status, result.(map[string]any)["status"])
	assert.EqualValues(t, 1, service.calls.Load())
	require.NotNil(t, row.ExecutedAt, "run-now must stamp executed_at")
	assert.WithinRange(t, *row.ExecutedAt, before.Add(-time.Second), time.Now())
	require.NotNil(t, row.ExecutedByUserID, "run-now must stamp executed_by_user_id")
	assert.Equal(t, *f.execution.CreatedByUserID, *row.ExecutedByUserID)
	require.NotNil(t, row.PreApprovalSkipReason)
	assert.Equal(t, "run-now", *row.PreApprovalSkipReason)
}

func TestRunNow_PartialFailureIsServerErrorWithRowStatus(t *testing.T) {
	f, service := newRunNowFixture(t, 2, twoRunNowRecs())
	result, err := f.runNow()
	assert.Nil(t, result)

	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client-facing error, got %v", err)
	assert.Equal(t, 502, ce.code, "money moved: this is an execution failure, not a 409 conflict")
	row, getErr := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
	require.NoError(t, getErr)
	assert.Equal(t, "partially_completed", row.Status)
	assert.Equal(t, row.Status, ce.Details()["status"])
	assert.EqualValues(t, 2, service.calls.Load())
	assert.NotNil(t, row.ExecutedAt)
}

// A partial run-now bought one of two commitments (#532): it answers 502 with
// the row status, and it is emailed with a revocation token that revokes the
// stored row, listing only the commitment that was bought.
func TestRunNow_PartialRunEmailsRevocationToken(t *testing.T) {
	f, service := newRunNowFixture(t, 2, twoRunNowRecs())
	notify := "ops@example.com"
	global, err := f.store.GetGlobalConfig(f.ctx)
	require.NoError(t, err)
	global.NotificationEmail = &notify
	require.NoError(t, f.store.SaveGlobalConfig(f.ctx, global))
	f.handler.auth.(*MockAuthService).On("GetUser", mock.Anything, mock.Anything).Return(nil, errors.New("no user")).Maybe()
	notifier := &recordingExecutedNotifier{}
	f.handler.emailNotifier = notifier
	before := time.Now()

	_, runErr := f.runNow()
	ce, ok := IsClientError(runErr)
	require.True(t, ok, "expected a client-facing error, got %v", runErr)
	assert.Equal(t, 502, ce.code)
	assert.EqualValues(t, 2, service.calls.Load())
	row, err := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, "partially_completed", row.Status)

	require.Equal(t, 1, notifier.calls, "a partial run must send the executed email")
	token := notifier.captured.RevocationToken
	require.NotEmpty(t, token)
	assert.NoError(t, validateRevokeToken(row, token), "the emailed token must revoke the stored row")
	require.NotNil(t, row.ApprovalTokenExpiresAt)
	assert.WithinRange(t, *row.ApprovalTokenExpiresAt,
		before.Add(config.RevocationWindow).Add(-time.Second), time.Now().Add(config.RevocationWindow).Add(time.Second))
	assert.True(t, notifier.captured.PartialPurchase)
	require.Len(t, notifier.captured.Recommendations, 1, "only the bought commitment is listed")
	assert.InDelta(t, 300.0, notifier.captured.TotalUpfrontCost, 0.001)
}

func TestRunNow_FourEyesDenialIsForbiddenAndLeavesRowUntouched(t *testing.T) {
	f, service := newRunNowFixture(t, 0, fanoutRecs())
	require.NoError(t, f.store.SaveGlobalConfig(f.ctx, &config.GlobalConfig{RequireDifferentApprover: true}))

	_, err := f.runNow()
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client-facing error, got %v", err)
	assert.Equal(t, 403, ce.code)
	row, getErr := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
	require.NoError(t, getErr)
	assert.Equal(t, "pending", row.Status)
	assert.Nil(t, row.ExecutedAt)
	assert.Zero(t, service.calls.Load())
}

func TestRunNow_LostClaimIsConflict(t *testing.T) {
	f, service := newRunNowFixture(t, 0, fanoutRecs())
	f.execution.Status = "running"
	require.NoError(t, f.store.SavePurchaseExecution(f.ctx, f.execution))

	_, err := f.runNow()
	assertPauseConflict(t, err)
	row, getErr := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
	require.NoError(t, getErr)
	assert.Equal(t, "running", row.Status)
	assert.Nil(t, row.ExecutedAt)
	assert.Zero(t, service.calls.Load())
}

func TestRunNow_CanceledRequestAfterClaimPersistsTerminalOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failOn  int32
		status  string
		history int
	}{
		{name: "completed", status: "completed", history: 1},
		{name: "failed", failOn: 1, status: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, service := newRunNowFixture(t, tc.failOn, fanoutRecs())
			requestCtx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			var claimStatus string
			var claimErr error
			service.onPurchase = func(context.Context) {
				row, err := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
				claimErr = err
				if row != nil {
					claimStatus = row.Status
				}
				cancel()
			}
			_, runErr := f.handler.runPlannedPurchase(requestCtx, f.request, f.execution.ExecutionID)
			require.NoError(t, claimErr)
			require.Equal(t, "running", claimStatus)
			require.ErrorIs(t, requestCtx.Err(), context.Canceled)
			if tc.failOn == 0 {
				require.NoError(t, runErr)
			} else {
				require.Error(t, runErr)
			}
			verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer verifyCancel()
			row, err := f.store.GetExecutionByID(verifyCtx, f.execution.ExecutionID)
			require.NoError(t, err)
			require.Equal(t, tc.status, row.Status)
			var histories int
			require.NoError(t, f.pool.QueryRow(verifyCtx, "SELECT count(*) FROM purchase_history WHERE plan_id=$1", f.planID).Scan(&histories))
			require.Equal(t, tc.history, histories)
			freshRequest := *f.request
			_, retryErr := f.handler.runPlannedPurchase(verifyCtx, &freshRequest, f.execution.ExecutionID)
			require.Error(t, retryErr)
			assert.EqualValues(t, 1, service.calls.Load())
		})
	}
}
