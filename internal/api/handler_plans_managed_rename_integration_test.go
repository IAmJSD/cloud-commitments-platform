//go:build integration

package api

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/require"
)

func TestUpdatePlanRenameKeepsManagedMarkers(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	_, err := f.pool.Exec(ctx, `UPDATE purchase_plans SET next_execution_date = now() + interval '3 days',
		ramp_schedule = ramp_schedule || '{"dates_managed":true,"allocation_mode":"equal_count"}' WHERE id = $1`, f.planID)
	require.NoError(t, err)
	const snapshot = `SELECT jsonb_build_object('managed', ramp_schedule->'dates_managed', 'mode', ramp_schedule->'allocation_mode',
		'step', ramp_schedule->'current_step', 'start', ramp_schedule->'start_date', 'next', next_execution_date)::text
		FROM purchase_plans WHERE id = $1`
	var before, after, name string
	require.NoError(t, f.pool.QueryRow(ctx, snapshot, f.planID).Scan(&before))

	_, err = f.handler.updatePlan(ctx, &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token", "Content-Type": "application/json"},
		Body:    `{"name":"renamed","enabled":true,"notification_days_before":2,"ramp_schedule":"weekly-25pct"}`,
	}, f.planID)
	require.NoError(t, err)

	require.NoError(t, f.pool.QueryRow(ctx, snapshot, f.planID).Scan(&after))
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT name FROM purchase_plans WHERE id = $1`, f.planID).Scan(&name))
	require.Equal(t, "renamed", name)
	require.JSONEq(t, before, after, "a rename must not touch the editor-owned markers, clock or progress")
	require.Contains(t, after, `"managed": true`)
}
