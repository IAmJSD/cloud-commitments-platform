//go:build integration

package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRampRetention_OldCleanupWritersPreserveHeldRows(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 0, 0, 4)
	cases := []struct {
		status      string
		unsubmitted bool
		payload     string
	}{
		{StatusCompleted, false, `[]`},
		{StatusPartiallyCompleted, false, `{}`},
		{"notified", false, `null`},
		{"paused", false, `[]`},
		{StatusFailed, false, `[]`},
		{StatusFailed, true, `[]`},
		{StatusCanceled, false, `[]`},
		{LegacyStatusCanceled, false, `[]`},
		{StatusExpired, false, `[]`},
	}
	for _, tc := range cases {
		var held bool
		require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_holds($1,$2)`, tc.status, tc.unsubmitted).Scan(&held))
		require.Equal(t, holdsCapacity(&PurchaseExecution{Status: tc.status, NoSubmission: tc.unsubmitted}), held)
		_, err := f.store.db.Exec(ctx, `INSERT INTO purchase_executions
			(plan_id, execution_id, status, scheduled_date, expires_at, updated_at, no_submission, recommendations)
			VALUES ($1, $2, $3, NOW()-INTERVAL '31 days', NOW()-INTERVAL '31 days', NOW()-INTERVAL '31 days', $4, $5)`,
			f.planID, uuid.NewString(), tc.status, tc.unsubmitted, tc.payload)
		require.NoError(t, err)
	}
	var future bool
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_holds('future-status',false)`).Scan(&future))
	require.Equal(t, holdsCapacity(&PurchaseExecution{Status: "future-status"}), future)
	deleted, err := f.store.CleanupOldExecutions(ctx, 30)
	require.NoError(t, err)
	require.EqualValues(t, 3, deleted, "only canceled spellings and expired are eligible and unheld")
	// A raw expires_at DELETE, the shape the deleted cmd/cleanup-lambda ran
	// (#357) and an operator could still run by hand: the ramp ledger trigger,
	// not the caller's predicate, must keep held rows.
	tag, err := f.store.db.Exec(ctx, `DELETE FROM purchase_executions WHERE expires_at < NOW()`)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "failed/no-submission is unheld but excluded by ordinary health retention")
	var remaining int
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT COUNT(*) FROM purchase_executions WHERE plan_id=$1`, f.planID).Scan(&remaining))
	require.Equal(t, 5, remaining)
	require.NoError(t, f.store.DeletePurchasePlan(ctx, f.planID))
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT COUNT(*) FROM purchase_executions WHERE plan_id IS NULL`).Scan(&remaining))
	require.Equal(t, 5, remaining, "parent deletion detaches audit rows")
	tag, err = f.store.db.Exec(ctx, `DELETE FROM purchase_executions WHERE expires_at < NOW()`)
	require.NoError(t, err)
	require.EqualValues(t, 5, tag.RowsAffected(), "detached history resumes ordinary cleanup")
}

func TestRampRetention_MonotonicTransitions(t *testing.T) {
	cases := []struct{ name, before, change, want string }{
		{"same metadata", "", `name=name,ramp_baseline=ramp_baseline`, "trusted"},
		{"cannot rearm trusted", "", `ramp_ledger_state='new'`, "trusted"},
		{"baseline replacement", "", `ramp_baseline='{"fixture":{"count":101}}'`, "legacy"},
		{"progress rewind", `ramp_schedule=jsonb_set(ramp_schedule,'{current_step}','1')`, `ramp_schedule=jsonb_set(ramp_schedule,'{current_step}','0')`, "legacy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRetentionSQLFixture(ctx, t)
			_, err := f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_baseline='{"fixture":{"count":100}}',ramp_ledger_state='trusted' WHERE id=$1`, f.planID)
			require.NoError(t, err)
			if tc.before != "" {
				_, err = f.store.db.Exec(ctx, `UPDATE purchase_plans SET `+tc.before+` WHERE id=$1`, f.planID)
				require.NoError(t, err)
			}
			_, err = f.store.db.Exec(ctx, `UPDATE purchase_plans SET `+tc.change+` WHERE id=$1`, f.planID)
			require.NoError(t, err)
			var state string
			require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, f.planID).Scan(&state))
			require.Equal(t, tc.want, state)
		})
	}
}

func TestRampRetention_SQLShapeMatchesBaselineInvalidation(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)
	services := `{"a":{"provider":"aws","service":"ec2","enabled":true,"term":1,"payment":"no-upfront","coverage":80}}`
	ramp := `{"type":"weekly","percent_per_step":25,"total_steps":4,"step_interval_days":7,"current_step":0}`
	cases := []struct{ name, services, ramp string }{
		{"coverage", strings.Replace(services, `80`, `90`, 1), ramp},
		{"provider case", strings.Replace(services, `aws`, `AWS`, 1), ramp},
		{"map key", strings.Replace(services, `"a"`, `"renamed"`, 1), ramp},
		{"payment", strings.Replace(services, `no-upfront`, `upfront`, 1), ramp},
		{"term", strings.Replace(services, `"term":1`, `"term":3`, 1), ramp},
		{"progress", services, strings.Replace(ramp, `"current_step":0`, `"current_step":1`, 1)},
		{"shape", services, strings.Replace(ramp, `25`, `50`, 1)},
		{"interval", services, strings.Replace(ramp, `7`, `30`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var old, next PurchasePlan
			require.NoError(t, json.Unmarshal([]byte(services), &old.Services))
			require.NoError(t, json.Unmarshal([]byte(ramp), &old.RampSchedule))
			require.NoError(t, json.Unmarshal([]byte(tc.services), &next.Services))
			require.NoError(t, json.Unmarshal([]byte(tc.ramp), &next.RampSchedule))
			var changed bool
			require.NoError(t, store.db.QueryRow(ctx, `SELECT ramp_ledger_shape($1,$2) IS DISTINCT FROM ramp_ledger_shape($3,$4)`, services, ramp, tc.services, tc.ramp).Scan(&changed))
			require.Equal(t, rampBaselineStale(&old, &next), changed)
		})
	}
}

func TestRampRetention_ImmediateAndFinishedHistoryStillExpires(t *testing.T) {
	for name, shape := range map[string][2]int{"immediate": {0, 1}, "finished": {4, 4}} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newRampFixture(ctx, t, 0, shape[0], shape[1])
			_, err := f.store.db.Exec(ctx, `INSERT INTO purchase_executions
				(plan_id, execution_id, status, scheduled_date) VALUES ($1,$2,'completed',NOW()-INTERVAL '31 days')`, f.planID, uuid.NewString())
			require.NoError(t, err)
			deleted, err := f.store.CleanupOldExecutions(ctx, 30)
			require.NoError(t, err)
			require.EqualValues(t, 1, deleted)
		})
	}
}

func TestRampRetention_DeleteOverlapsProtectedExitWithoutParentLock(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 0, 0, 4)
	e := f.row(ctx, t, 1, nil, StatusCompleted)
	_, err := f.store.db.Exec(ctx, `UPDATE purchase_executions SET scheduled_date=NOW()-INTERVAL '31 days' WHERE execution_id=$1`, e.ExecutionID)
	require.NoError(t, err)
	tx, err := f.store.db.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule=jsonb_set(ramp_schedule,'{total_steps}','1') WHERE id=$1`, f.planID)
	require.NoError(t, err)
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tag, err := f.store.db.Exec(bounded, `DELETE FROM purchase_executions WHERE execution_id=$1`, e.ExecutionID)
	require.NoError(t, err, "DELETE must not wait for the parent row lock")
	require.Zero(t, tag.RowsAffected(), "the committed parent shape remains protected")
	require.NoError(t, tx.Commit(ctx))
	deleted, err := f.store.CleanupOldExecutions(ctx, 30)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "cleanup sees the newly committed unprotected shape")
	_, err = f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule=jsonb_set(ramp_schedule,'{total_steps}','4') WHERE id=$1`, f.planID)
	require.NoError(t, err)
	var state string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, f.planID).Scan(&state))
	require.Equal(t, "legacy", state, "leaving and reopening protection never rearms trust")
}

func TestRampRetention_DownUpKeepsExistingLineageUntrusted(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 0, 0, 4)
	for _, direction := range []string{"down", "up"} {
		raw, err := os.ReadFile(filepath.Join(getTestMigrationsPath(), "000124_ramp_ledger_retention."+direction+".sql"))
		require.NoError(t, err)
		_, err = f.store.db.Exec(ctx, string(raw))
		require.NoError(t, err)
	}
	var state string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, f.planID).Scan(&state))
	require.Equal(t, "legacy", state)
}

func newRetentionSQLFixture(ctx context.Context, t *testing.T) *rampFixture {
	t.Helper()
	store := setupRampStepStore(ctx, t)
	id := uuid.NewString()
	_, err := store.db.Exec(ctx, `INSERT INTO purchase_plans(id,name,services,ramp_schedule,ramp_ledger_state)
		VALUES ($1,'synthetic SQL lineage','{}','{"type":"weekly","percent_per_step":25,"total_steps":4,"step_interval_days":7,"current_step":0}','new')`, id)
	require.NoError(t, err)
	return &rampFixture{store: store, planID: id}
}

func TestRampRetention_CreationLineageAndLegacyWriters(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 0, 0, 4)
	var state string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, f.planID).Scan(&state))
	require.Equal(t, "new", state)
	legacyID := uuid.NewString()
	_, err := f.store.db.Exec(ctx, `INSERT INTO purchase_plans(id,name,ramp_schedule) SELECT $1,'old writer',ramp_schedule FROM purchase_plans WHERE id=$2`, legacyID, f.planID)
	require.NoError(t, err)
	_, err = f.store.db.Exec(ctx, `UPDATE purchase_plans SET ramp_ledger_state='trusted',ramp_baseline='{"fixture":{"count":100}}' WHERE id=$1`, legacyID)
	require.NoError(t, err)
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, legacyID).Scan(&state))
	require.Equal(t, "legacy", state)
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE name='old writer'`).Scan(&state))
	require.Equal(t, "legacy", state)
}

func TestRampRetention_NullServicesShapeParity(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)
	var equal bool
	require.NoError(t, store.db.QueryRow(ctx, `SELECT ramp_ledger_shape('null','{}') IS NOT DISTINCT FROM ramp_ledger_shape('{}','{}')`).Scan(&equal))
	old, next := &PurchasePlan{}, &PurchasePlan{Services: map[string]ServiceConfig{}}
	require.Equal(t, !rampBaselineStale(old, next), equal)
}
