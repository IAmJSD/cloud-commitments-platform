//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

type b1s1StoredState struct {
	row, history, accounts, ledger, baseline string
}

func b1s1State(t *testing.T, f *createConcurrencyFixture) b1s1StoredState {
	t.Helper()
	ctx := context.Background()
	state := b1s1StoredState{row: b1aRow(t, f)}
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY execution_id),'[]')::text FROM purchase_executions e WHERE plan_id=$1`, f.planID).Scan(&state.history))
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('association',to_jsonb(a),'account',to_jsonb(c)) ORDER BY account_id),'[]')::text FROM plan_accounts a JOIN cloud_accounts c ON c.id=a.account_id WHERE plan_id=$1`, f.planID).Scan(&state.accounts))
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT ramp_ledger_state, COALESCE(ramp_baseline::text,'null') FROM purchase_plans WHERE id=$1`, f.planID).Scan(&state.ledger, &state.baseline))
	return state
}

func b1s1Fixture(t *testing.T, service config.ServiceConfig) *createConcurrencyFixture {
	t.Helper()
	ctx := context.Background()
	f := b1aFixture(t)
	accounts, err := f.store.GetPlanAccounts(ctx, f.planID)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	next := time.Now().AddDate(0, 0, 7).UTC().Truncate(time.Microsecond)
	plan := &config.PurchasePlan{Name: "alias plan", Enabled: true, NotificationDaysBefore: 2,
		Services: map[string]config.ServiceConfig{"original-key": service}, RampSchedule: config.PresetRampSchedules["weekly-25pct"], NextExecutionDate: &next}
	plan.RampSchedule.StartDate = time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, f.store.CreatePurchasePlan(ctx, plan))
	f.planID = plan.ID
	require.NoError(t, f.store.SetPlanAccounts(ctx, f.planID, []string{accounts[0].ID}))
	stored := b1aRead(t, f)
	require.NotNil(t, stored.NextExecutionDate)
	require.True(t, next.Equal(*stored.NextExecutionDate))
	require.Equal(t, 0, stored.RampSchedule.CurrentStep)
	require.Equal(t, config.PresetRampSchedules["weekly-25pct"].TotalSteps, stored.RampSchedule.TotalSteps)
	state := b1s1State(t, f)
	require.Equal(t, "new", state.ledger)
	require.Equal(t, "null", state.baseline)
	require.Equal(t, "[]", state.history)
	return f
}

func b1s1AssertStoredEdit(t *testing.T, f *createConcurrencyFixture, before *config.PurchasePlan, state b1s1StoredState, ledger string, clearBaseline bool, response string) {
	t.Helper()
	after := b1aRead(t, f)
	returned, err := json.Marshal(after)
	require.NoError(t, err)
	require.JSONEq(t, string(returned), response, "complete PUT response agrees with authoritative store and GET")
	wantService := before.Services["original-key"]
	wantService.Service, wantService.Coverage = "compute", 85
	before.Name, before.Services, before.UpdatedAt = "renamed", map[string]config.ServiceConfig{"original-key": wantService}, after.UpdatedAt
	require.Equal(t, before, after, "all stored fields, dates, progress and service settings survive")
	actual := b1s1State(t, f)
	require.Equal(t, state.history, actual.history, "full execution rows survive")
	require.Equal(t, state.accounts, actual.accounts, "full account associations survive")
	require.Equal(t, ledger, actual.ledger)
	baseline := state.baseline
	if clearBaseline {
		baseline = "null"
	}
	require.Equal(t, baseline, actual.baseline, "baseline bytes follow the existing ledger identity rule")
	var oldRow, newRow map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(state.row), &oldRow))
	require.NoError(t, json.Unmarshal([]byte(actual.row), &newRow))
	oldRow["name"] = json.RawMessage(`"renamed"`)
	services, err := json.Marshal(after.Services)
	require.NoError(t, err)
	oldRow["services"], oldRow["updated_at"] = services, newRow["updated_at"]
	oldRow["ramp_ledger_state"], err = json.Marshal(ledger)
	require.NoError(t, err)
	oldRow["ramp_baseline"] = json.RawMessage(baseline)
	expected, err := json.Marshal(oldRow)
	require.NoError(t, err)
	require.JSONEq(t, string(expected), actual.row, "no other SQL column changes")
}

func b1s1PutBody(before *config.PurchasePlan, extra string) string {
	fields := fmt.Sprintf(`,"provider":"azure","service":"compute","term":1,"payment":"monthly","target_coverage":85,"expected_updated_at":%q%s`, before.UpdatedAt.Format(time.RFC3339Nano), extra)
	return fmt.Sprintf(b1aUpdate, fields)
}

func TestLegacyServiceB1S1RoutedLedger(t *testing.T) {
	for _, tc := range []struct {
		name, service, ledger string
		enabled, trusted      bool
	}{
		{"enabled alias demotes trusted", "vm", "legacy", true, true},
		{"canonical identity keeps trusted", "compute", "trusted", true, true},
		{"fresh enabled alias stays new", "vm", "new", true, false},
		{"disabled alias keeps trusted", "vm", "trusted", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := b1s1Fixture(t, b1s1Service("azure", tc.service, tc.enabled))
			if tc.trusted {
				accounts, err := f.store.GetPlanAccounts(ctx, f.planID)
				require.NoError(t, err)
				rec := config.RecommendationRecord{ID: uuid.NewString(), Provider: "azure", Service: tc.service, Region: "west", ResourceType: "large", Term: 1, Payment: "monthly", Count: 5, Selected: true, CloudAccountID: &accounts[0].ID}
				identity, err := config.RecIdentityKey(&rec)
				require.NoError(t, err)
				require.NotEmpty(t, identity)
				var wrote bool
				require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
					if _, lockErr := f.store.LockPurchasePlanTx(ctx, tx, f.planID); lockErr != nil {
						return lockErr
					}
					var txErr error
					wrote, txErr = f.store.SetPlanRampBaselineTx(ctx, tx, f.planID, config.PlanRampBaseline{identity: {Count: 100}})
					return txErr
				}))
				require.True(t, wrote)
				trusted := b1s1State(t, f)
				require.Equal(t, "trusted", trusted.ledger)
				require.Contains(t, trusted.baseline, `"count": 100`)
				baseline, err := json.Marshal(config.PlanRampBaseline{identity: {Count: 100}})
				require.NoError(t, err)
				require.JSONEq(t, string(baseline), trusted.baseline)
				completed := time.Now().UTC().Truncate(time.Microsecond)
				execution := &config.PurchaseExecution{ExecutionID: uuid.NewString(), PlanID: f.planID, Status: config.StatusCompleted, StepNumber: 1, ScheduledDate: completed, CompletedAt: &completed, Recommendations: []config.RecommendationRecord{rec}, CloudAccountID: &accounts[0].ID}
				require.NoError(t, f.store.SavePurchaseExecution(ctx, execution))
				var storedID string
				var count int
				var selected bool
				require.NoError(t, f.pool.QueryRow(ctx, `SELECT recommendations->0->>'id', (recommendations->0->>'count')::int, (recommendations->0->>'selected')::bool FROM purchase_executions WHERE execution_id=$1 AND status='completed'`, execution.ExecutionID).Scan(&storedID, &count, &selected))
				require.Equal(t, rec.ID, storedID)
				require.Equal(t, 5, count)
				require.True(t, selected)
			}
			before, state := b1aRead(t, f), b1s1State(t, f)
			accounts, err := f.store.GetPlanAccounts(ctx, f.planID)
			require.NoError(t, err)
			response := b1aRoute(t, f.handler, "PUT", "/api/plans/"+f.planID, b1s1PutBody(before, ""))
			require.Equal(t, 200, response.StatusCode, response.Body)
			b1s1AssertStoredEdit(t, f, before, state, tc.ledger, tc.ledger == "legacy", response.Body)
			afterAccounts, err := f.store.GetPlanAccounts(ctx, f.planID)
			require.NoError(t, err)
			require.Equal(t, accounts, afterAccounts)
		})
	}
}

func TestLegacyServiceB1S1RoutedRefusals(t *testing.T) {
	for _, managed := range []bool{true, false} {
		t.Run(map[bool]string{true: "managed sensitive alias", false: "missing arming grant"}[managed], func(t *testing.T) {
			f := b1s1Fixture(t, b1s1Service("azure", "vm", true))
			handler, status, extra := f.handler, 409, ""
			if managed {
				setPlanManaged(context.Background(), t, f, true)
			} else {
				handler = limitedHandler(f, []auth.Permission{{Action: auth.ActionUpdate, Resource: auth.ResourcePlans}}, nil)
				status, extra = 403, `,"auto_purchase":true,"max_step_commitment":1000`
			}
			before, state := b1aRead(t, f), b1s1State(t, f)
			response := b1aRoute(t, handler, "PUT", "/api/plans/"+f.planID, b1s1PutBody(before, extra))
			require.Equal(t, status, response.StatusCode, response.Body)
			if managed {
				require.Contains(t, response.Body, "editor_reconciliation_required")
			}
			require.Equal(t, before, b1aRead(t, f))
			require.Equal(t, state, b1s1State(t, f), "refusal preserves every row, account, ledger and execution field")
		})
	}
}

func TestLegacyServiceB1S1Migration127Replay(t *testing.T) {
	ctx := context.Background()
	f := b1s1Fixture(t, b1s1Service("azure", "vm", true))
	before := b1aRead(t, f)
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE purchase_plans DISABLE TRIGGER guard_ramp_ledger_state`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE purchase_plans SET ramp_ledger_state='legacy' WHERE id=$1`, f.planID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE purchase_plans ENABLE TRIGGER guard_ramp_ledger_state`)
		return err
	}))
	legacy := b1s1State(t, f)
	require.Equal(t, "legacy", legacy.ledger)
	require.Equal(t, "null", legacy.baseline)
	require.Equal(t, "[]", legacy.history)
	var protected, guardLive bool
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT ramp_ledger_protected(ramp_schedule) AND (ramp_schedule->>'current_step')::int=0 AND ramp_baseline IS NULL FROM purchase_plans WHERE id=$1`, f.planID).Scan(&protected))
	require.True(t, protected)
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT tgenabled='O' FROM pg_trigger WHERE tgname='guard_ramp_ledger_state' AND tgrelid='purchase_plans'::regclass`).Scan(&guardLive))
	require.True(t, guardLive)
	sql, err := os.ReadFile(filepath.Join(getMigrationsPath(), "000127_never_started_legacy_ramps_to_new.up.sql"))
	require.NoError(t, err)
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, string(sql)); return err }))
	promoted := b1s1State(t, f)
	require.Equal(t, "new", promoted.ledger, "actual migration 127 SQL REPLAY promotes eligible legacy state")
	require.Equal(t, legacy.baseline, promoted.baseline)
	require.Equal(t, legacy.history, promoted.history)
	require.Equal(t, legacy.accounts, promoted.accounts)
	after := b1aRead(t, f)
	before.UpdatedAt = after.UpdatedAt
	require.Equal(t, before, after, "SQL REPLAY retains services, dates and progress")
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT tgenabled='O' FROM pg_trigger WHERE tgname='guard_ramp_ledger_state' AND tgrelid='purchase_plans'::regclass`).Scan(&guardLive))
	require.True(t, guardLive)
	response := b1aRoute(t, f.handler, "PUT", "/api/plans/"+f.planID, b1s1PutBody(after, ""))
	require.Equal(t, 200, response.StatusCode, response.Body)
	b1s1AssertStoredEdit(t, f, after, promoted, "new", false, response.Body)
}
