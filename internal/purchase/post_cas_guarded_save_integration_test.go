//go:build integration

package purchase

// Issue #835 on real PostgreSQL: each post-CAS save replayed against a
// concurrent status change landing between the CAS (or the caller's read) and
// the save. Real store, real Manager, counting fake provider (labeled fixture;
// nothing here reaches a cloud API). The concurrent writers are the production
// store calls that make them (the late-recovery write, the session revoke's
// CAS, the reaper's CAS) except where noted as simulated.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// interleaveStore runs before exactly once, ahead of the first execution save
// of any kind (guarded or not, own transaction or caller's), so the concurrent
// write lands after the caller's CAS or read and before its save. Hooking every
// save variant keeps the replay identical before and after the fix.
type interleaveStore struct {
	*config.PostgresStore
	once   sync.Once
	before func()
}

func (s *interleaveStore) fire() { s.once.Do(s.before) }

func (s *interleaveStore) SavePurchaseExecution(ctx context.Context, e *config.PurchaseExecution) error {
	s.fire()
	return s.PostgresStore.SavePurchaseExecution(ctx, e)
}

func (s *interleaveStore) SavePurchaseExecutionTx(ctx context.Context, tx pgx.Tx, e *config.PurchaseExecution) error {
	s.fire()
	return s.PostgresStore.SavePurchaseExecutionTx(ctx, tx, e)
}

func (s *interleaveStore) SaveExecutionIfStatus(ctx context.Context, e *config.PurchaseExecution, expected []string) error {
	s.fire()
	return s.PostgresStore.SaveExecutionIfStatus(ctx, e, expected)
}

func (s *interleaveStore) SaveExecutionIfStatusTx(ctx context.Context, tx pgx.Tx, e *config.PurchaseExecution, expected []string) error {
	s.fire()
	return s.PostgresStore.SaveExecutionIfStatusTx(ctx, tx, e, expected)
}

// interleave points the manager at a store that runs before ahead of its next
// execution save, and restores the plain store when the subtest ends.
func (f *rampStepFixture) interleave(t *testing.T, before func()) {
	t.Helper()
	f.manager.config = &interleaveStore{PostgresStore: f.store, before: before}
	t.Cleanup(func() { f.manager.config = f.store })
}

// mustTransition is the concurrent writer: one production status CAS.
func mustTransition(ctx context.Context, t *testing.T, f *rampStepFixture, id string, from []string, to string) {
	t.Helper()
	_, err := f.store.TransitionExecutionStatus(ctx, id, from, to, nil)
	require.NoError(t, err)
}

// Writers that exist today land between the CAS and the save, and the save
// used to put the old status back.
func TestPostCASSave_ConcurrentStatusChangeSurvives(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)

	// safeFail flips a stranded approved row to failed; the interrupted
	// original run then finishes late and its purchased outcome is recovered
	// over the failed row (RecoverReapedExecution, the D1 path). The stamp must
	// not turn the purchased row back into "failed", which invites a Retry
	// that buys a second commitment.
	t.Run("safeFail stamp after a late recovery", func(t *testing.T) {
		row := claimRaceRow(ctx, t, f, "approved")
		f.interleave(t, func() {
			late := rowStatusCopy(row)
			late.Status = config.StatusCompleted
			now := time.Now()
			late.CompletedAt = &now
			late.Recommendations[0].Purchased = true
			require.NoError(t, f.store.RecoverReapedExecution(ctx, late))
		})

		// The late recovery completes the row before the guarded stamp, so the
		// failed outcome is stale and no failure email may go out.
		counted, err := f.manager.safeFail(ctx, rowStatusCopy(row))

		require.NoError(t, err)
		assert.True(t, counted, "the flip to failed landed, so the sweep still counts it")
		f.email.AssertNumberOfCalls(t, "SendPurchaseFailedNotification", 0)
		got := rowStatus(ctx, t, f, row.ExecutionID)
		assert.Equal(t, config.StatusCompleted, got.Status, "the recovered purchase must not be reverted to failed")
		assert.NotContains(t, got.Error, "failed by the recovery sweep")
		require.Len(t, got.Recommendations, 1)
		assert.True(t, got.Recommendations[0].Purchased)
	})

	// Without the interleaved recovery the same flip stays failed and is
	// announced exactly once (#1195).
	t.Run("safeFail flip without a recovery notifies once", func(t *testing.T) {
		row := claimRaceRow(ctx, t, f, "approved")
		f.email.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).Return(nil).Once()

		counted, err := f.manager.safeFail(ctx, rowStatusCopy(row))

		require.NoError(t, err)
		assert.True(t, counted)
		assert.Equal(t, config.StatusFailed, rowStatus(ctx, t, f, row.ExecutionID).Status)
		f.email.AssertNumberOfCalls(t, "SendPurchaseFailedNotification", 1)
	})

	// A session revoke (revokeViaSession's CAS) lands right after the terminal
	// save; the revocation-token rotation and the ramp-refusal note then write
	// the caller's in-memory "completed" row. Neither may erase the request.
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, exec *config.PurchaseExecution)
	}{
		{"revocation token rotation", func(t *testing.T, exec *config.PurchaseExecution) {
			_, err := f.manager.rotateApprovalToken(ctx, exec, config.RevocationWindow)
			assert.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
		}},
		{"ramp-advance refusal note", func(_ *testing.T, exec *config.PurchaseExecution) {
			f.manager.recordRampAdvanceRefusal(ctx, exec, errors.New("plan row changed"))
		}},
	} {
		t.Run(tc.name+" after a revoke", func(t *testing.T) {
			row := claimRaceRow(ctx, t, f, config.StatusCompleted)
			mustTransition(ctx, t, f, row.ExecutionID, []string{config.StatusCompleted, config.StatusPartiallyCompleted}, config.StatusRevocationRequested)

			tc.write(t, rowStatusCopy(row))

			got := rowStatus(ctx, t, f, row.ExecutionID)
			assert.Equal(t, config.StatusRevocationRequested, got.Status, "the revocation request must survive")
			assert.NotContains(t, got.Error, "ramp not advanced")
		})
	}
}

// The executors' post-claim audit stamp finds the claim gone (here the
// reaper's own CAS, running -> failed, lands right after the claim; the reaper
// waits 10 minutes today, so this ordering is not reachable in production yet).
// Nothing may be bought. Before the fix the stamp failure was logged and the
// purchase went ahead.
func TestPostCASSave_ClaimLostBeforeStampBuysNothing(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	reap := func(t *testing.T, row *config.PurchaseExecution) {
		f.interleave(t, func() { mustTransition(ctx, t, f, row.ExecutionID, []string{"running"}, config.StatusFailed) })
	}

	t.Run("approve", func(t *testing.T) {
		row := claimRaceRow(ctx, t, f, "notified")
		before := f.purchaseCount()
		reap(t, row)

		got, _, err := f.manager.ApproveAndExecute(ctx, row.ExecutionID, "approver@example.invalid", nil, nil)

		assert.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
		assert.Nil(t, got)
		assert.Equal(t, 0, f.purchaseCount()-before, "a run that lost its claim must not reach the provider")
		assert.Equal(t, config.StatusFailed, rowStatus(ctx, t, f, row.ExecutionID).Status)
	})

	t.Run("scheduled fire", func(t *testing.T) {
		row := claimRaceRow(ctx, t, f, "scheduled")
		before := f.purchaseCount()
		reap(t, row)

		fired, raceLost := f.manager.fireOneDue(ctx, rowStatusCopy(row))

		assert.False(t, fired)
		assert.True(t, raceLost)
		assert.Equal(t, 0, f.purchaseCount()-before, "a run that lost its claim must not reach the provider")
		assert.Equal(t, config.StatusFailed, rowStatus(ctx, t, f, row.ExecutionID).Status)
	})
}

// No production writer moves a canceled row today, so the concurrent write
// here is simulated (a status CAS to expired). The guard is defense in depth:
// the full-row save must not resurrect the status its own CAS set once another
// writer has moved the row on. failExistingPlanStep is not replayed here: its
// CAS, save and release share one transaction (issue #1052), so no other
// writer can land between them; plan_step_fail_atomic_integration_test.go
// covers its concurrent writers.
func TestPostCASSave_SimulatedWriterSurvives(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)

	t.Run("clearApprovalToken", func(t *testing.T) {
		row := claimRaceRow(ctx, t, f, config.StatusCanceled)
		f.interleave(t, func() {
			mustTransition(ctx, t, f, row.ExecutionID, []string{config.StatusCanceled}, config.StatusExpired)
		})

		err := f.manager.clearApprovalToken(ctx, row.ExecutionID)

		assert.Equal(t, config.StatusExpired, rowStatus(ctx, t, f, row.ExecutionID).Status)
		assert.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
	})
}
