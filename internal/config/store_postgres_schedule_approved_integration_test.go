//go:build integration

package config

// Real PostgreSQL tests for ScheduleApprovedExecution (#348 F6): the status
// change and the scheduled_execution_at stamp commit or roll back together.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var scheduleFrom = []string{"pending", "notified"}

func scheduleFields(ctx context.Context, t *testing.T, store *PostgresStore, id string) (status string, at *time.Time, approvedBy, transitionedBy *string) {
	t.Helper()
	require.NoError(t, store.db.QueryRow(ctx, `SELECT status, scheduled_execution_at, approved_by, transitioned_by::text
		FROM purchase_executions WHERE execution_id = $1`, id).Scan(&status, &at, &approvedBy, &transitionedBy))
	return status, at, approvedBy, transitionedBy
}

func TestScheduleApprovedExecution_StampsWithTheTransition(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "notified")
	at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Microsecond)
	by := "approver@example.com"

	got, err := store.ScheduleApprovedExecution(ctx, exec.ExecutionID, scheduleFrom, nil, "", at, &by)
	require.NoError(t, err)
	assert.Equal(t, "scheduled", got.Status)
	require.NotNil(t, got.ScheduledExecutionAt)
	assert.True(t, at.Equal(*got.ScheduledExecutionAt))

	status, storedAt, approvedBy, _ := scheduleFields(ctx, t, store, exec.ExecutionID)
	assert.Equal(t, "scheduled", status)
	require.NotNil(t, storedAt, "a scheduled row must carry its execution time")
	assert.True(t, at.Equal(*storedAt))
	require.NotNil(t, approvedBy)
	assert.Equal(t, by, *approvedBy)
}

func TestScheduleApprovedExecution_Refusals(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	at := time.Now().Add(time.Hour)

	pinned := newNoSubmissionExec(ctx, t, store, "notified")
	_, err := store.db.Exec(ctx, `UPDATE purchase_executions SET approval_token_hash = $2, approval_token_expires_at = $3
		WHERE execution_id = $1`, pinned.ExecutionID, HashApprovalToken("current-token"), time.Now().Add(time.Hour))
	require.NoError(t, err)
	_, err = store.ScheduleApprovedExecution(ctx, pinned.ExecutionID, scheduleFrom, nil, "rotated-away-token", at, nil)
	require.ErrorIs(t, err, ErrApprovalTokenChanged)
	status, storedAt, _, _ := scheduleFields(ctx, t, store, pinned.ExecutionID)
	assert.Equal(t, "notified", status)
	assert.Nil(t, storedAt)

	_, err = store.ScheduleApprovedExecution(ctx, pinned.ExecutionID, scheduleFrom, nil, "current-token", at, nil)
	require.NoError(t, err, "the current token schedules the row")

	running := newNoSubmissionExec(ctx, t, store, "running")
	_, err = store.ScheduleApprovedExecution(ctx, running.ExecutionID, scheduleFrom, nil, "", at, nil)
	require.ErrorIs(t, err, ErrExecutionNotInExpectedStatus)
}

// The real failure: the stamp's write fails after the status CAS. Before the
// fix the row was left "scheduled" with no execution time, which no scheduler
// run selects, so the approved purchase never fired.
func TestScheduleApprovedExecution_FailedStampRollsBackTheTransition(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	exec := newNoSubmissionExec(ctx, t, store, "notified")
	failScheduleStamp(ctx, t, store)

	_, err := store.ScheduleApprovedExecution(ctx, exec.ExecutionID, scheduleFrom, nil, "", time.Now().Add(time.Hour), nil)
	require.Error(t, err)
	status, storedAt, _, transitionedBy := scheduleFields(ctx, t, store, exec.ExecutionID)
	assert.Equal(t, "notified", status, "the status change must roll back with the stamp")
	assert.Nil(t, storedAt)
	assert.Nil(t, transitionedBy)
}

// failScheduleStamp makes any UPDATE that sets scheduled_execution_at fail, in
// this test's database only. The status CAS leaves the column alone, so only
// the stamp is refused.
func failScheduleStamp(ctx context.Context, t *testing.T, store *PostgresStore) {
	t.Helper()
	_, err := store.db.Exec(ctx, `
		CREATE FUNCTION test_fail_schedule_stamp() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'test: scheduled_execution_at write refused'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_schedule_stamp BEFORE UPDATE ON purchase_executions
			FOR EACH ROW WHEN (NEW.scheduled_execution_at IS DISTINCT FROM OLD.scheduled_execution_at)
			EXECUTE FUNCTION test_fail_schedule_stamp();`)
	require.NoError(t, err)
}
