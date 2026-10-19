package purchase

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil/lambdatimeout"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestLambdaTimeoutCoversPurchaseBudget keeps every shipped AWS Lambda timeout
// above the detached purchase budget (execution, then the terminal save).
// Lambda ends the invocation at its timeout whatever the context deadline
// says, so a shorter timeout cuts a purchase off mid-flight (#913).
func TestLambdaTimeoutCoversPurchaseBudget(t *testing.T) {
	lambdatimeout.AssertCovers(t, "../..", purchaseExecutionTimeout+terminalSaveTimeout)
}

// detachFixture wires a direct single-account AWS purchase (issue #706).
type detachFixture struct {
	manager          *Manager
	exec             *config.PurchaseExecution
	svc              *MockServiceClient
	saved            []string
	history          int
	saveCtx          context.Context
	saveRemaining    time.Duration
	histCtx          context.Context
	historyRemaining time.Duration
}

func newDetachFixture(t *testing.T) *detachFixture {
	t.Helper()
	f := &detachFixture{svc: new(MockServiceClient)}
	store := new(MockConfigStore)
	factory := new(MockProviderFactory)
	prov := new(MockProvider)
	stsMock := new(MockSTSClient)
	f.exec = &config.PurchaseExecution{
		ExecutionID: "exec-detach", StepNumber: 1, Status: "running",
		Recommendations: []config.RecommendationRecord{
			{Provider: "aws", Service: "ec2", ResourceType: "m5.large", Region: "us-east-1", Count: 1, UpfrontCost: 500, Selected: true},
		},
	}
	store.SavePurchaseExecutionFn = func(ctx context.Context, e *config.PurchaseExecution) error {
		f.saveCtx = ctx
		if deadline, ok := ctx.Deadline(); ok {
			f.saveRemaining = time.Until(deadline)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f.saved = append(f.saved, e.Status)
		return nil
	}
	store.On("SavePurchaseHistory", mock.Anything, mock.Anything).Run(func(a mock.Arguments) {
		f.history++
		f.histCtx = a.Get(0).(context.Context)
		if deadline, ok := f.histCtx.Deadline(); ok {
			f.historyRemaining = time.Until(deadline)
		}
	}).Return(nil)
	email := new(MockEmailSender)
	email.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil)
	stsMock.On("GetCallerIdentity", mock.Anything, mock.Anything).Return(&stsOut, nil)
	factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(prov, nil)
	prov.On("GetServiceClient", mock.Anything, common.ServiceEC2, "us-east-1").Return(f.svc, nil)
	f.manager = &Manager{config: store, email: email, stsClient: stsMock, providerFactory: factory, dashboardURL: "https://d.example.com"}
	return f
}

func TestExecuteAndFinalize_SlowRunGetsFreshTerminalSaveBudget(t *testing.T) {
	t.Setenv("CUDLY_MAX_ACCOUNT_PARALLELISM", "1")
	f := newDetachFixture(t)
	f.exec.Recommendations = append(f.exec.Recommendations, f.exec.Recommendations[0])
	f.exec.Recommendations[1].ResourceType = "m5.xlarge"
	var active atomic.Int32
	var overlapped atomic.Bool
	var providerCanceled atomic.Bool
	f.svc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			if active.Add(1) != 1 {
				overlapped.Store(true)
			}
			defer active.Add(-1)
			ctx := args.Get(0).(context.Context)
			timer := time.NewTimer(16 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				providerCanceled.Store(true)
			}
		}).Return(common.PurchaseResult{Success: true, CommitmentID: "ri-slow"}, nil).Twice()
	started := time.Now()
	err := f.manager.executeAndFinalize(context.Background(), f.exec)
	assert.Greater(t, time.Since(started), 30*time.Second)
	assert.False(t, overlapped.Load())
	assert.False(t, providerCanceled.Load(), "provider call must fit its own budget")
	f.svc.AssertNumberOfCalls(t, "PurchaseCommitment", 2)
	assert.Equal(t, 2, f.history)
	require.NoError(t, err)
	assert.Equal(t, []string{"completed"}, f.saved)
	assert.Greater(t, f.saveRemaining, 29*time.Second)
	assert.LessOrEqual(t, f.saveRemaining, 30*time.Second)
}

var stsOut = sts.GetCallerIdentityOutput{Account: aws.String("123456789012")}

// A committed purchase must remain auditable after request cancellation.
func TestExecuteAndFinalize_ClientDisconnectMidPurchase_StillRecorded(t *testing.T) {
	f := newDetachFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.svc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { cancel() }).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-1"}, nil).Once()

	err := f.manager.executeAndFinalize(ctx, f.exec)

	require.NoError(t, err)
	f.svc.AssertNumberOfCalls(t, "PurchaseCommitment", 1)
	assert.Equal(t, []string{"completed"}, f.saved)
	assert.Equal(t, 1, f.history)
}

// Test B: a caller ctx whose deadline already passed (30s request timeout)
// must not reach the provider or the save; both get their own bounded budget.
func TestExecuteAndFinalize_ExpiredCallerDeadline_UsesOwnBudgets(t *testing.T) {
	f := newDetachFixture(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	var provDeadline time.Time
	var provContextErr error
	f.svc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Run(func(a mock.Arguments) {
			c := a.Get(0).(context.Context)
			provContextErr = c.Err()
			provDeadline, _ = c.Deadline()
		}).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-2"}, nil).Once()

	require.NoError(t, f.manager.executeAndFinalize(ctx, f.exec))
	require.NoError(t, provContextErr)

	// per-rec cap (30s) still bounds the provider call inside the 4m run budget.
	assert.WithinDuration(t, time.Now(), provDeadline, 30*time.Second)
	_, ok := f.saveCtx.Deadline()
	require.True(t, ok, "terminal save needs a deadline")
	// Literal durations on purpose: comparing against the constants would
	// pass whatever their values are.
	assert.LessOrEqual(t, f.saveRemaining, 30*time.Second)
	assert.Greater(t, f.saveRemaining, 29*time.Second)
	// SavePurchaseHistory runs inside executePurchase, so it sees the run ctx.
	_, ok = f.histCtx.Deadline()
	require.True(t, ok, "run ctx needs a deadline")
	assert.LessOrEqual(t, f.historyRemaining, 4*time.Minute)
	assert.Greater(t, f.historyRemaining, 3*time.Minute+59*time.Second)
	assert.Equal(t, []string{"completed"}, f.saved)
}

// Test C: a failed purchase with a canceled caller still persists "failed".
func TestExecuteAndFinalize_ProviderErrorWithCanceledCaller_PersistsFailed(t *testing.T) {
	f := newDetachFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.svc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { cancel() }).
		Return(common.PurchaseResult{}, errors.New("provider rejected")).Once()

	err := f.manager.executeAndFinalize(ctx, f.exec)

	require.Error(t, err)
	assert.NotErrorIs(t, err, config.ErrAuditLoss)
	assert.Equal(t, []string{"failed"}, f.saved)
}
