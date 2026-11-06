//go:build integration

package api

// Issue #835 and #348 F6 on real PostgreSQL: the delayed approve's transition
// to "scheduled" and its scheduled_execution_at stamp are one store
// transaction. Real store and handler; the revoke is the exact store work
// revokeScheduledExecution does (CancelScheduledExecutionAtomic plus the
// suppression release, in one transaction). Nothing here reaches a cloud API.

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// revokeAfterScheduleStore runs revoke once, right after the real
// ScheduleApprovedExecution commits. The CAS-to-stamp window the old two-step
// handler had no longer exists, so the earliest a revoke can land is after the
// scheduled, stamped row is committed.
type revokeAfterScheduleStore struct {
	*config.PostgresStore
	revoke func()
}

func (s *revokeAfterScheduleStore) ScheduleApprovedExecution(ctx context.Context, executionID string, fromStatuses []string, transitionedBy *string, rawToken string, scheduledAt time.Time, approvedBy *string) (*config.PurchaseExecution, error) {
	updated, err := s.PostgresStore.ScheduleApprovedExecution(ctx, executionID, fromStatuses, transitionedBy, rawToken, scheduledAt, approvedBy)
	if err == nil {
		s.revoke()
	}
	return updated, err
}

// A committed delayed approval can still be revoked for free (#835): the
// revoke cancels the stamped row and the scheduler never sees it as due. The
// CAS-to-stamp window #835 guarded no longer exists (one transaction), so this
// is a smoke test of the revoke path; the store test pins the atomicity.
func TestApproveWithDelay_Postgres_RevokeAfterScheduleStaysCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	f := newCreateConcurrencyFixture(ctx, t)

	row := &config.PurchaseExecution{
		ExecutionID: uuid.NewString(), PlanID: f.planID, Status: "notified", StepNumber: 3, ScheduledDate: time.Now(),
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, row))
	revokedBy := "revoker@example.com"
	var canceled bool
	f.handler.config = &revokeAfterScheduleStore{PostgresStore: f.store, revoke: func() {
		require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			if canceled, _, err = f.store.CancelScheduledExecutionAtomic(ctx, tx, row.ExecutionID, &revokedBy); err != nil {
				return err
			}
			return f.store.DeleteSuppressionsByExecutionTx(ctx, tx, row.ExecutionID)
		}))
	}}

	// A delay already in the past: had the revoke not canceled the row, the
	// scheduler's due query would return it on its next tick.
	_, err := f.handler.approveWithDelay(ctx, row, -time.Minute, "approver@example.com", nil, "")

	require.NoError(t, err, "the approve committed before the revoke, so it succeeds")
	require.True(t, canceled, "the revoke must have won its CAS on the scheduled row")
	got, getErr := f.store.GetExecutionByID(ctx, row.ExecutionID)
	require.NoError(t, getErr)
	assert.Equal(t, config.StatusCanceled, got.Status, "the revoke must survive")
	assert.NotNil(t, got.ScheduledExecutionAt, "the stamp committed with the transition")
	due, dueErr := f.store.GetScheduledExecutionsDue(ctx)
	require.NoError(t, dueErr)
	for i := range due {
		assert.NotEqual(t, row.ExecutionID, due[i].ExecutionID, "a revoked row must never be due for the scheduler")
	}
}

// #348 F6: when the scheduled_execution_at write fails after the status
// transition, the old handler left the row "scheduled" with a NULL time: no
// revoke button, not cancelable, not picked up by the scheduler, and a managed
// plan stalled behind it. The trigger fails any UPDATE that changes
// scheduled_execution_at, which is the real failing scenario; the whole
// schedule must roll back and leave the row notified.
func TestApproveWithDelay_Postgres_StampFailureLeavesRowNotified(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	f := newCreateConcurrencyFixture(ctx, t)

	row := &config.PurchaseExecution{
		ExecutionID: uuid.NewString(), PlanID: f.planID, Status: "notified", StepNumber: 3, ScheduledDate: time.Now(),
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, row))

	_, err := f.pool.Exec(ctx, `
		CREATE FUNCTION fail_schedule_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected stamp failure'; END $$`)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `
		CREATE TRIGGER fail_schedule_stamp BEFORE UPDATE ON purchase_executions
		FOR EACH ROW WHEN (NEW.scheduled_execution_at IS DISTINCT FROM OLD.scheduled_execution_at)
		EXECUTE FUNCTION fail_schedule_stamp()`)
	require.NoError(t, err)

	_, err = f.handler.approveWithDelay(ctx, row, time.Hour, "approver@example.com", nil, "")

	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 500, ce.code, "a failed stamp is an internal error, not a 2xx or a client refusal")
	got, getErr := f.store.GetExecutionByID(ctx, row.ExecutionID)
	require.NoError(t, getErr)
	assert.Equal(t, "notified", got.Status, "the status transition must roll back with the failed stamp")
	assert.Nil(t, got.ScheduledExecutionAt)
}
