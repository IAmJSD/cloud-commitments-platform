//go:build integration

package api

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pauseClaimService struct {
	fanoutServiceClient
	calls            atomic.Int32
	entered, release chan struct{}
	once             sync.Once
}

func (s *pauseClaimService) unblock() { s.once.Do(func() { close(s.release) }) }

func (s *pauseClaimService) PurchaseCommitment(ctx context.Context, rec common.Recommendation, opts common.PurchaseOptions) (common.PurchaseResult, error) {
	if s.calls.Add(1) == 1 {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return common.PurchaseResult{}, ctx.Err()
		}
	}
	return s.fanoutServiceClient.PurchaseCommitment(ctx, rec, opts)
}

type pauseClaimProvider struct {
	fanoutProvider
	service *pauseClaimService
}

func (p *pauseClaimProvider) GetServiceClient(context.Context, common.ServiceType, string) (provider.ServiceClient, error) {
	return p.service, nil
}

type pauseClaimFactory struct{ provider *pauseClaimProvider }

func (f pauseClaimFactory) CreateAndValidateProvider(context.Context, string, *provider.ProviderConfig) (provider.Provider, error) {
	return f.provider, nil
}

type pauseClaimFixture struct {
	*createConcurrencyFixture
	ctx       context.Context
	service   *pauseClaimService
	manager   *purchase.Manager
	execution *config.PurchaseExecution
	request   *events.LambdaFunctionURLRequest
	body      string
	workers   sync.WaitGroup
}

// pauseClaimCap is a plan cap far above the fixture recommendations.
var pauseClaimCap = 1e9

// markNotified moves the row to "notified", the only state in which the
// scheduler and SQS paths will auto-execute a recommendation-bearing plan row
// (the notification email is the veto window). pause only applies to "pending",
// so tests call this after pausing and resuming and before the worker runs.
func (f *pauseClaimFixture) markNotified(t *testing.T) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET status = 'notified' WHERE execution_id = $1`, f.execution.ExecutionID)
	require.NoError(t, err)
}

func newPauseClaimFixture(t *testing.T) *pauseClaimFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	f := &pauseClaimFixture{createConcurrencyFixture: newCreateConcurrencyFixture(ctx, t), ctx: ctx}
	plan, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	plan.AutoPurchase = true
	plan.MaxStepCommitment = &pauseClaimCap
	require.NoError(t, f.store.UpdatePurchasePlan(ctx, plan))
	globalCfg, err := f.store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	globalCfg.AutoPurchaseEnabled = true
	require.NoError(t, f.store.SaveGlobalConfig(ctx, globalCfg))
	accounts, err := f.store.GetPlanAccounts(ctx, plan.ID)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	account := accounts[0]
	account.Enabled = true
	account.AWSAuthMode = "access_keys"
	require.NoError(t, f.store.UpdateCloudAccount(ctx, &account))
	var creator string
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT id::text FROM users WHERE email='creator@example.com'").Scan(&creator))
	plan, err = f.store.GetPurchasePlan(ctx, plan.ID)
	require.NoError(t, err)
	require.True(t, plan.AutoPurchase)
	require.Equal(t, 2, plan.RampSchedule.CurrentStep)
	persistedAccount, err := f.store.GetCloudAccount(ctx, account.ID)
	require.NoError(t, err)
	require.True(t, persistedAccount.Enabled)
	require.Equal(t, "access_keys", persistedAccount.AWSAuthMode)
	f.service = &pauseClaimService{entered: make(chan struct{}), release: make(chan struct{})}
	f.manager = purchase.NewManager(purchase.ManagerConfig{
		ConfigStore: f.store, EmailSender: &stubEmailNotifier{}, CredentialStore: &fanoutCredStore{},
		ProviderFactory: pauseClaimFactory{provider: &pauseClaimProvider{service: f.service}},
	})
	f.execution = &config.PurchaseExecution{
		ExecutionID: uuid.NewString(), PlanID: plan.ID, CloudAccountID: &account.ID,
		Status: "pending", StepNumber: 3, ScheduledDate: time.Now(), IdempotencyKey: uuid.NewString(),
		CreatedByUserID: &creator, Recommendations: fanoutRecsForAccount(account.ID),
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, f.execution))
	row, err := f.store.GetExecutionByID(ctx, f.execution.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, "pending", row.Status)
	require.Equal(t, 3, row.StepNumber)
	require.Equal(t, f.execution.CloudAccountID, row.CloudAccountID)
	require.NotEqual(t, common.PurchaseSourceWeb, row.Source)
	f.request = &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}}
	body, err := json.Marshal(purchase.AsyncMessage{Type: purchase.MessageTypeExecutePurchase, ExecutionID: row.ExecutionID})
	require.NoError(t, err)
	f.body = string(body)
	t.Cleanup(func() { f.service.unblock(); cancel(); f.workers.Wait() })
	return f
}

func (f *pauseClaimFixture) startWorker() <-chan error {
	done := make(chan error, 1)
	f.workers.Add(1)
	go func() { defer f.workers.Done(); done <- f.manager.ProcessMessage(f.ctx, f.body) }()
	return done
}

func (f *pauseClaimFixture) waitWorker(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-f.ctx.Done():
		t.Fatal("purchase worker did not finish")
	}
}

func (f *pauseClaimFixture) pause() error {
	_, err := f.handler.pausePlannedPurchase(f.ctx, f.request, f.execution.ExecutionID)
	return err
}

func (f *pauseClaimFixture) resume() error {
	_, err := f.handler.resumePlannedPurchase(f.ctx, f.request, f.execution.ExecutionID)
	return err
}

type pauseClaimState struct {
	Status         string
	Actor          *string
	TransitionedAt *time.Time
}

func (f *pauseClaimFixture) state(t *testing.T) pauseClaimState {
	t.Helper()
	var state pauseClaimState
	require.NoError(t, f.pool.QueryRow(f.ctx,
		"SELECT status, transitioned_by::text, transitioned_at FROM purchase_executions WHERE execution_id=$1",
		f.execution.ExecutionID).Scan(&state.Status, &state.Actor, &state.TransitionedAt))
	return state
}

func (f *pauseClaimFixture) assertCompleted(t *testing.T) {
	t.Helper()
	row, err := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", row.Status)
	assert.NotNil(t, row.CompletedAt)
	assert.EqualValues(t, 1, f.service.calls.Load(), "only one worker may enter the provider")
	var histories int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		"SELECT count(*) FROM purchase_history WHERE plan_id=$1 AND purchase_id=$2",
		f.planID, "ri-"+common.DeriveIdempotencyToken(f.execution.IdempotencyKey, 0)).Scan(&histories))
	assert.Equal(t, 1, histories)
	plan, err := f.store.GetPurchasePlan(f.ctx, f.planID)
	require.NoError(t, err)
	assert.Equal(t, 3, plan.RampSchedule.CurrentStep)
}

func assertPauseConflict(t *testing.T, err error) {
	t.Helper()
	ce, ok := IsClientError(err)
	if assert.True(t, ok, "expected conflict, got %v", err) {
		assert.Equal(t, 409, ce.code)
	}
}

func TestPauseClaim_RunningWorkerCannotBeReclaimed(t *testing.T) {
	f := newPauseClaimFixture(t)
	f.markNotified(t)
	first := f.startWorker()
	select {
	case <-f.service.entered:
	case <-f.ctx.Done():
		t.Fatal("first worker did not reach the provider")
	}
	before := f.state(t)
	require.Equal(t, "running", before.Status)
	require.Nil(t, before.Actor)
	require.NoError(t, f.manager.ProcessMessage(f.ctx, f.body))
	require.EqualValues(t, 1, f.service.calls.Load())
	pauseErr, resumeErr := f.pause(), f.resume()
	// Reach the purchase counter even when the old handler incorrectly accepts both actions.
	require.NoError(t, f.manager.ProcessMessage(f.ctx, f.body))
	assert.EqualValues(t, 1, f.service.calls.Load(), "pause/resume must not allow a second provider entry")
	assertPauseConflict(t, pauseErr)
	assertPauseConflict(t, resumeErr)
	assert.Equal(t, before, f.state(t), "failed transitions must preserve worker ownership and attribution")
	f.service.unblock()
	f.waitWorker(t, first)
	f.assertCompleted(t)
}

func TestPauseClaim_PausedQueueResumesExactlyOnce(t *testing.T) {
	f := newPauseClaimFixture(t)
	require.NoError(t, f.pause())
	paused := f.state(t)
	require.Equal(t, "paused", paused.Status)
	require.Equal(t, f.execution.CreatedByUserID, paused.Actor)
	require.NotNil(t, paused.TransitionedAt)
	require.NoError(t, f.manager.ProcessMessage(f.ctx, f.body))
	require.Zero(t, f.service.calls.Load())
	require.Equal(t, paused, f.state(t))
	require.NoError(t, f.resume())
	require.Equal(t, "pending", f.state(t).Status)
	// A resumed row is pending again, so auto-execution still refuses it until
	// a notification has gone out.
	require.Error(t, f.manager.ProcessMessage(f.ctx, f.body))
	require.Zero(t, f.service.calls.Load())
	f.markNotified(t)
	f.service.unblock()
	require.NoError(t, f.manager.ProcessMessage(f.ctx, f.body))
	require.NoError(t, f.manager.ProcessMessage(f.ctx, f.body))
	f.assertCompleted(t)
}

func TestPauseClaim_RejectsNonPendingStates(t *testing.T) {
	f := newPauseClaimFixture(t)
	for _, status := range []string{"running", "completed", "canceled", "failed", "approved", "notified", "scheduled", "paused"} {
		t.Run(status, func(t *testing.T) {
			f.execution.Status = status
			require.NoError(t, f.store.SavePurchaseExecution(f.ctx, f.execution))
			before := f.state(t)
			assertPauseConflict(t, f.pause())
			assert.Equal(t, before, f.state(t))
		})
	}
	assert.Zero(t, f.service.calls.Load())
}

func TestPauseClaim_ConcurrentPauseAndWorker(t *testing.T) {
	f := newPauseClaimFixture(t)
	start := make(chan struct{})
	paused := make(chan error, 1)
	worker := make(chan error, 1)
	f.workers.Add(2)
	go func() { defer f.workers.Done(); <-start; paused <- f.pause() }()
	go func() { defer f.workers.Done(); <-start; worker <- f.manager.ProcessMessage(f.ctx, f.body) }()
	close(start)
	select {
	case err := <-paused:
		require.NoError(t, err, "a pending row is pausable: the worker can never claim it")
	case <-f.ctx.Done():
		t.Fatal("pause did not finish")
	}
	select {
	case <-worker: // refused (row still pending) or lost the claim (row paused): both buy nothing
	case <-f.ctx.Done():
		t.Fatal("purchase worker did not finish")
	}
	require.Zero(t, f.service.calls.Load())
	require.Equal(t, "paused", f.state(t).Status)
	require.NoError(t, f.resume())
	f.markNotified(t)
	f.service.unblock()
	require.NoError(t, f.manager.ProcessMessage(f.ctx, f.body))
	f.assertCompleted(t)
}
