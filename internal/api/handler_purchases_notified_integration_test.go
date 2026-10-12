//go:build integration

package api

import (
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL through the handlers. The rec-bearing row is a hand-built
// fixture (nothing attaches recommendations to a plan step yet).

func (f *pauseClaimFixture) setRow(t *testing.T, status string, date time.Time) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET status=$2, scheduled_date=$3 WHERE execution_id=$1`, f.execution.ExecutionID, status, date)
	require.NoError(t, err)
}

func (f *pauseClaimFixture) scheduledDate(t *testing.T) time.Time {
	t.Helper()
	row, err := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
	require.NoError(t, err)
	return row.ScheduledDate
}

func (f *pauseClaimFixture) suppressionCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT count(*) FROM purchase_suppressions WHERE execution_id=$1`, f.execution.ExecutionID).Scan(&n))
	return n
}

func TestNotifiedRow_PauseResumeHandlersMoveDatePastWindow(t *testing.T) {
	f := newPauseClaimFixture(t)
	f.setRow(t, "notified", time.Now().Add(-48*time.Hour))
	require.NoError(t, f.pause())
	require.Equal(t, "paused", f.state(t).Status)

	before := time.Now()
	require.NoError(t, f.resume())
	assert.Equal(t, "pending", f.state(t).Status)
	plan, err := f.store.GetPurchasePlan(f.ctx, f.execution.PlanID)
	require.NoError(t, err)
	assert.WithinDuration(t, before.AddDate(0, 0, plan.NotificationDaysBefore), f.scheduledDate(t), time.Minute)
}

// F1: a cancel landing between the paused->pending CAS and the reschedule is
// not overwritten by the reschedule (replayed with the stale resume snapshot).
func TestNotifiedRow_RescheduleDoesNotOverwriteConcurrentCancel(t *testing.T) {
	f := newPauseClaimFixture(t)
	past := time.Now().Add(-48 * time.Hour)
	f.setRow(t, "paused", past)
	resumed, err := f.store.TransitionExecutionStatus(f.ctx, f.execution.ExecutionID, []string{"paused"}, "pending", nil)
	require.NoError(t, err)
	_, err = f.store.TransitionExecutionStatus(f.ctx, f.execution.ExecutionID, []string{"pending", "notified", "paused"}, "canceled", nil)
	require.NoError(t, err)
	dateBefore := f.scheduledDate(t)

	require.NoError(t, f.handler.pushPastScheduledDate(f.ctx, resumed, time.Now()))

	assert.Equal(t, "canceled", f.state(t).Status)
	assert.WithinDuration(t, dateBefore, f.scheduledDate(t), time.Second)
}

// F2: canceling or deleting a notified rec-bearing row releases its suppressions.
func TestNotifiedRow_DeleteReleasesSuppressions(t *testing.T) {
	f := newPauseClaimFixture(t)
	f.setRow(t, "notified", time.Now().Add(time.Hour))
	global, err := f.store.GetGlobalConfig(f.ctx)
	require.NoError(t, err)
	sups := config.BuildSuppressions(f.execution.Recommendations, f.execution.ExecutionID, global, time.Now())
	require.NotEmpty(t, sups)
	for i := range sups {
		require.NoError(t, f.store.CreateSuppression(f.ctx, &sups[i]))
	}
	require.Positive(t, f.suppressionCount(t))

	_, err = f.handler.deletePlannedPurchase(f.ctx, f.request, f.execution.ExecutionID)
	require.NoError(t, err)

	assert.Equal(t, "canceled", f.state(t).Status)
	assert.Zero(t, f.suppressionCount(t))
}
