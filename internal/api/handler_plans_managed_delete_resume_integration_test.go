//go:build integration

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func (f *createConcurrencyFixture) saveRow(ctx context.Context, t *testing.T, status string, noSubmission bool, scheduled time.Time, recs bool) string {
	t.Helper()
	row := &config.PurchaseExecution{ExecutionID: uuid.NewString(), PlanID: f.planID, Status: status, StepNumber: 3, ScheduledDate: scheduled, NoSubmission: noSubmission}
	if recs {
		row.Recommendations = []config.RecommendationRecord{{ID: "r1", Provider: "azure", Service: "compute", Selected: true}}
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, row))
	return row.ExecutionID
}

func (f *createConcurrencyFixture) deletePlan(ctx context.Context, h *Handler) error {
	_, err := h.deletePlan(ctx, managedGuardRequest(""), f.planID)
	return err
}

func requireLiveExecutionsRefusal(t *testing.T, err error) {
	t.Helper()
	var ce *clientError
	require.True(t, errors.As(err, &ce), "want a client error, got %v", err)
	require.Equal(t, http.StatusConflict, ce.code)
	require.Equal(t, codeManagedPlanLiveExecutions, ce.details["code"])
}

func TestDeleteManagedPlanByExecutionStatus(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		status       string
		noSubmission bool
		blocks       bool
	}{
		{"pending", false, true}, {"notified", false, true}, {"scheduled", false, true}, {"approved", false, true},
		{"running", false, true}, {"paused", false, true}, {"failed", false, true},
		{"completed", false, false}, {"partially_completed", false, false}, {"canceled", false, false},
		{"expired", false, false}, {"failed", true, false},
	}
	for _, tc := range cases {
		name := tc.status
		if tc.noSubmission {
			name += "_no_submission"
		}
		t.Run(name, func(t *testing.T) {
			f := newCreateConcurrencyFixture(ctx, t)
			setPlanManaged(ctx, t, f, true)
			id := f.saveRow(ctx, t, tc.status, tc.noSubmission, time.Now(), false)
			err := f.deletePlan(ctx, f.handler)
			var planID *string
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT plan_id::text FROM purchase_executions WHERE execution_id = $1`, id).Scan(&planID))
			if tc.blocks {
				requireLiveExecutionsRefusal(t, err)
				require.NotNil(t, planID, "a refused delete must not detach the root")
				require.Equal(t, 1, f.count(ctx, t, `SELECT count(*) FROM purchase_plans WHERE id = $1`))
				return
			}
			require.NoError(t, err)
			require.Nil(t, planID, "settled history detaches as before")
		})
	}
	t.Run("unmanaged plan with a live row deletes as before", func(t *testing.T) {
		f := newCreateConcurrencyFixture(ctx, t)
		f.saveRow(ctx, t, "pending", false, time.Now(), false)
		require.NoError(t, f.deletePlan(ctx, f.handler))
	})
}

func TestDeleteManagedPlanWaitsForCommandAndSeesItsRoot(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	command := f.holdPlanLock(ctx, t)
	done := make(chan error, 1)
	go func() { done <- f.deletePlan(ctx, f.handler) }()
	awaitLockWait(ctx, t, f)

	id := uuid.NewString()
	_, err := command.Exec(ctx, `INSERT INTO purchase_executions (execution_id, plan_id, status, step_number, scheduled_date)
		VALUES ($1, $2, 'pending', 1, now())`, id, f.planID)
	require.NoError(t, err)
	_, err = command.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule = ramp_schedule || '{"dates_managed":true}' WHERE id = $1`, f.planID)
	require.NoError(t, err)
	require.NoError(t, command.Commit(ctx))

	requireLiveExecutionsRefusal(t, <-done)
	require.Equal(t, 1, f.count(ctx, t, `SELECT count(*) FROM purchase_executions WHERE plan_id = $1`))
}

func TestRescheduleAwaitingExecutionOnManagedPlan(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	orig := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	id := f.saveRow(ctx, t, "pending", false, orig, true)
	moved := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	ok, err := f.store.RescheduleAwaitingExecution(ctx, id, moved)
	require.NoError(t, err)
	require.True(t, ok, "unmanaged plan reschedules as before")

	setPlanManaged(ctx, t, f, true)
	ok, err = f.store.RescheduleAwaitingExecution(ctx, id, orig)
	require.ErrorIs(t, err, config.ErrEditorReconciliationRequired)
	require.False(t, ok)
	row, err := f.store.GetExecutionByID(ctx, id)
	require.NoError(t, err)
	require.True(t, row.ScheduledDate.Equal(moved), "refused reschedule leaves the date")
}

func TestRescheduleWaitsForCommandThenRefuses(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	orig := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	id := f.saveRow(ctx, t, "pending", false, orig, true)
	command := f.holdPlanLock(ctx, t)
	done := make(chan error, 1)
	go func() {
		_, err := f.store.RescheduleAwaitingExecution(ctx, id, time.Now().Add(48*time.Hour))
		done <- err
	}()
	awaitLockWait(ctx, t, f)
	_, err := command.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule = ramp_schedule || '{"dates_managed":true}' WHERE id = $1`, f.planID)
	require.NoError(t, err)
	require.NoError(t, command.Commit(ctx))

	require.ErrorIs(t, <-done, config.ErrEditorReconciliationRequired)
	row, err := f.store.GetExecutionByID(ctx, id)
	require.NoError(t, err)
	require.True(t, row.ScheduledDate.Equal(orig))
}

func TestResumeOnManagedPlanRefusesBeforeTransition(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	setPlanManaged(ctx, t, f, true)
	id := f.saveRow(ctx, t, "paused", false, time.Now().Add(-time.Hour), true)

	_, err := f.handler.resumePlannedPurchase(ctx, managedGuardRequest(""), id)
	requireEditorRefusal(t, err)
	row, err := f.store.GetExecutionByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "paused", row.Status)
}

func TestDeleteAndResumeAuthorizationPrecedeManagedGuard(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	setPlanManaged(ctx, t, f, true)
	id := f.saveRow(ctx, t, "paused", false, time.Now().Add(-time.Hour), true)
	f.saveRow(ctx, t, "pending", false, time.Now(), false)
	view := []auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePlans}}
	admin := []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}

	forbidden := limitedHandler(f, view, nil)
	scoped := limitedHandler(f, admin, []string{"unrelated-account"})
	_, resumeErr := forbidden.resumePlannedPurchase(ctx, managedGuardRequest(""), id)
	for name, got := range map[string]error{"delete": f.deletePlan(ctx, forbidden), "resume": resumeErr} {
		var ce *clientError
		require.True(t, errors.As(got, &ce), "%s: %v", name, got)
		require.Equal(t, http.StatusForbidden, ce.code, name)
	}
	_, scopedResumeErr := scoped.resumePlannedPurchase(ctx, managedGuardRequest(""), id)
	for name, got := range map[string]error{"delete": f.deletePlan(ctx, scoped), "resume": scopedResumeErr} {
		require.True(t, IsNotFoundError(got), "scoped %s must look like not found, got %v", name, got)
	}
	row, err := f.store.GetExecutionByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "paused", row.Status)
}
