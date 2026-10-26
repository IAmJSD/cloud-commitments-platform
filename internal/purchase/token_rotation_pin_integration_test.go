//go:build integration

package purchase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL, mock email sender and mock provider factory (labeled
// fixtures; nothing here reaches a cloud API). Issue #1190: the immediate
// email-link approve and the email-link cancel validated the approval token
// against a snapshot and then moved the row with a plain status CAS, so a
// re-notification rotating the token between the two let the old, revoked
// token still approve (and buy) or cancel.

const rotOldToken, rotNewToken = "rot-old-raw-token", "rot-new-raw-token"

// rotatingStore rotates the row's approval token, as a re-notification does,
// right after the first read of the row returns: the caller validates the
// old token against that snapshot and only then reaches its status CAS.
type rotatingStore struct {
	*config.PostgresStore
	t    *testing.T
	once sync.Once
}

func (r *rotatingStore) GetExecutionByID(ctx context.Context, id string) (*config.PurchaseExecution, error) {
	snapshot, err := r.PostgresStore.GetExecutionByID(ctx, id)
	r.once.Do(func() {
		rotated, rotErr := r.RotatePendingApprovalToken(ctx, id, config.HashApprovalToken(rotNewToken), time.Now().Add(time.Hour))
		require.NoError(r.t, rotErr)
		require.True(r.t, rotated, "the fixture row must be rotatable")
	})
	return snapshot, err
}

func tokenRotationRow(ctx context.Context, t *testing.T, store *config.PostgresStore, planID string) *config.PurchaseExecution {
	t.Helper()
	expires := time.Now().Add(time.Hour)
	row := &config.PurchaseExecution{
		PlanID: planID, ExecutionID: uuid.New().String(), Status: "notified", StepNumber: 1,
		ScheduledDate: time.Now().Add(time.Hour), Recommendations: []config.RecommendationRecord{upfrontRec(10)},
		ApprovalToken: config.HashApprovalToken(rotOldToken), ApprovalTokenExpiresAt: &expires,
	}
	require.NoError(t, store.SavePurchaseExecution(ctx, row))
	return row
}

// stubUnreachableProvider makes the factory's provider call fail; the tests
// assert it is never called on a refused row.
func stubUnreachableProvider(factory *MockProviderFactory) {
	factory.On("CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("fixture: no cloud provider in this test")).Maybe()
}

// quietSender accepts the outcome emails a run that reached execution sends.
func quietSender() *MockEmailSender {
	sender := new(MockEmailSender)
	sender.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil).Maybe()
	sender.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).Return(nil).Maybe()
	return sender
}

func TestApproveExecution_TokenRotatedBeforeClaimIsRefused(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store)

	t.Run("old token after a rotation", func(t *testing.T) {
		row := tokenRotationRow(ctx, t, store, plan.ID)
		factory := new(MockProviderFactory)
		stubUnreachableProvider(factory)
		manager := &Manager{config: &rotatingStore{PostgresStore: store, t: t}, email: quietSender(), providerFactory: factory}

		revocationToken, err := manager.ApproveExecution(ctx, row.ExecutionID, rotOldToken, "approver@example.invalid", nil)

		require.ErrorIs(t, err, config.ErrApprovalTokenChanged, "the revoked token must lose the claim")
		assert.Empty(t, revocationToken)
		after := statusInDB(ctx, t, store, row.ExecutionID)
		assert.Equal(t, "notified", after.Status, "the row is left for the current token")
		assert.True(t, config.ApprovalTokenMatches(after.ApprovalToken, rotNewToken), "the rotated token is kept")
		assert.Nil(t, after.ApprovedBy)
		factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("positive control: the same token with no rotation wins the claim", func(t *testing.T) {
		row := tokenRotationRow(ctx, t, store, plan.ID)
		factory := new(MockProviderFactory)
		stubUnreachableProvider(factory)
		manager := &Manager{config: store, email: quietSender(), providerFactory: factory}

		_, err := manager.ApproveExecution(ctx, row.ExecutionID, rotOldToken, "approver@example.invalid", nil)

		require.NotErrorIs(t, err, config.ErrApprovalTokenChanged)
		assert.NotEqual(t, "notified", statusInDB(ctx, t, store, row.ExecutionID).Status, "the claim was won and the run started")
	})
}

func TestCancelExecution_TokenRotatedBeforeCancelIsRefused(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store)
	row := tokenRotationRow(ctx, t, store, plan.ID)
	manager := &Manager{config: &rotatingStore{PostgresStore: store, t: t}}

	err := manager.CancelExecution(ctx, row.ExecutionID, rotOldToken, "a@example.invalid")

	require.ErrorIs(t, err, config.ErrApprovalTokenChanged, "the revoked token must not cancel")
	after := statusInDB(ctx, t, store, row.ExecutionID)
	assert.Equal(t, "notified", after.Status)
	assert.True(t, config.ApprovalTokenMatches(after.ApprovalToken, rotNewToken))

	// The current token still cancels.
	require.NoError(t, manager.CancelExecution(ctx, row.ExecutionID, rotNewToken, "a@example.invalid"))
	assert.Equal(t, config.StatusCanceled, statusInDB(ctx, t, store, row.ExecutionID).Status)
}

// The full email-link cancel (snapshot check, then pinned cancel) racing a
// re-notification, many times: exactly one of them may take effect.
func TestCancelExecution_RacesRotation(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store)
	manager := &Manager{config: store}
	const rounds = 40
	cancelWins, rotationWins := 0, 0
	for i := 0; i < rounds; i++ {
		row := tokenRotationRow(ctx, t, store, plan.ID)
		lag := time.Duration(i%20) * 100 * time.Microsecond
		cancelLag, rotateLag := lag, time.Duration(0)
		if i%2 == 1 {
			cancelLag, rotateLag = 0, lag
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		var cancelErr, rotateErr error
		var rotated bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(cancelLag)
			cancelErr = manager.CancelExecution(ctx, row.ExecutionID, rotOldToken, "a@example.invalid")
		}()
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(rotateLag)
			rotated, rotateErr = store.RotatePendingApprovalToken(ctx, row.ExecutionID, config.HashApprovalToken(rotNewToken), time.Now().Add(time.Hour))
		}()
		close(start)
		wg.Wait()
		require.NoError(t, rotateErr)
		canceled := cancelErr == nil
		require.True(t, canceled != rotated, "round %d: exactly one of the cancel (err=%v) and the rotation (%v) may win", i, cancelErr, rotated)
		after := statusInDB(ctx, t, store, row.ExecutionID)
		if canceled {
			cancelWins++
			assert.Equal(t, config.StatusCanceled, after.Status)
			continue
		}
		rotationWins++
		// The snapshot check may already have seen the new token (invalid) or
		// the pinned cancel may have lost to it (changed); both refuse.
		assert.True(t, errors.Is(cancelErr, ErrInvalidApprovalToken) || errors.Is(cancelErr, config.ErrApprovalTokenChanged), "round %d: %v", i, cancelErr)
		assert.Equal(t, "notified", after.Status)
		assert.True(t, config.ApprovalTokenMatches(after.ApprovalToken, rotNewToken))
	}
	t.Logf("cancel won %d, rotation won %d of %d rounds", cancelWins, rotationWins, rounds)
	assert.Positive(t, cancelWins, "the race never let the cancel go first")
	assert.Positive(t, rotationWins, "the race never let the rotation go first")
}
