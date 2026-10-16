//go:build integration

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func (f *createConcurrencyFixture) addPurchases(ctx context.Context, h *Handler, startDate time.Time) error {
	body := fmt.Sprintf(`{"count":1,"start_date":%q}`, startDate.Format("2006-01-02"))
	_, err := h.createPlannedPurchases(ctx, managedGuardRequest(body), f.planID)
	return err
}

func (f *createConcurrencyFixture) assign(ctx context.Context, h *Handler, ids ...string) error {
	body := `{"account_ids":["` + ids[0] + `"`
	for _, id := range ids[1:] {
		body += `,"` + id + `"`
	}
	_, err := h.setPlanAccounts(ctx, managedGuardRequest(body+`]}`), f.planID)
	return err
}

func (f *createConcurrencyFixture) count(ctx context.Context, t *testing.T, query string) (n int) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(ctx, query, f.planID).Scan(&n))
	return n
}

func (f *createConcurrencyFixture) extraAccount(ctx context.Context, t *testing.T, name string) string {
	t.Helper()
	acct := &config.CloudAccount{Name: name, Provider: "azure", ExternalID: "ext-" + name, Enabled: true}
	require.NoError(t, f.store.CreateCloudAccount(ctx, acct))
	return acct.ID
}

// holdPlanLock stands in for the editor command: an open transaction holding
// the plan row FOR UPDATE, rolled back at test end unless the test commits it.
func (f *createConcurrencyFixture) holdPlanLock(ctx context.Context, t *testing.T) pgx.Tx {
	t.Helper()
	command, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = command.Rollback(ctx) })
	_, err = command.Exec(ctx, `SELECT 1 FROM purchase_plans WHERE id = $1 FOR UPDATE`, f.planID)
	require.NoError(t, err)
	return command
}

func awaitLockWait(ctx context.Context, t *testing.T, f *createConcurrencyFixture) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'`).Scan(&n))
		return n > 0
	}, 15*time.Second, 50*time.Millisecond, "writer must wait on the plan lock")
}

const (
	execCount = `SELECT count(*) FROM purchase_executions WHERE plan_id = $1`
	acctCount = `SELECT count(*) FROM plan_accounts WHERE plan_id = $1`
)

func TestAddPurchasesRefusesManagedPlanWhole(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	start := time.Now().AddDate(0, 0, 7).UTC().Truncate(24 * time.Hour)

	setPlanManaged(ctx, t, f, true)
	// Equal dates make the later plan write a no-op, so only the early guard refuses.
	_, err := f.pool.Exec(ctx, `UPDATE purchase_plans SET next_execution_date = $2,
		ramp_schedule = jsonb_set(ramp_schedule, '{start_date}', to_jsonb(to_char($2::timestamptz AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))) WHERE id = $1`, f.planID, start)
	require.NoError(t, err)
	requireEditorRefusal(t, f.addPurchases(ctx, f.handler, start))
	require.Zero(t, f.count(ctx, t, execCount))

	setPlanManaged(ctx, t, f, false)
	require.NoError(t, f.addPurchases(ctx, f.handler, start))
	require.Equal(t, 1, f.count(ctx, t, execCount))
}

func TestAssignAccountsOnManagedPlan(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	var current string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT account_id::text FROM plan_accounts WHERE plan_id = $1`, f.planID).Scan(&current))
	other := f.extraAccount(ctx, t, "acct-b")

	require.NoError(t, f.assign(ctx, f.handler, current, other), "unmanaged plan assigns normally")
	require.NoError(t, f.assign(ctx, f.handler, current))
	setPlanManaged(ctx, t, f, true)

	require.NoError(t, f.assign(ctx, f.handler, current, current), "identical set with a duplicate passes")
	require.NoError(t, f.assign(ctx, f.handler, strings.ToUpper(current)), "identical set in upper-case hex passes")
	for name, ids := range map[string][]string{"added": {current, other}, "replaced": {other}} {
		requireEditorRefusal(t, f.assign(ctx, f.handler, ids...))
		require.Equal(t, 1, f.count(ctx, t, acctCount), name)
	}
}

func TestAddAndAssignAuthorizationPrecedeManagedGuard(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	setPlanManaged(ctx, t, f, true)
	other := f.extraAccount(ctx, t, "acct-b")
	view := []auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePlans}}
	admin := []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}
	start := time.Now().AddDate(0, 0, 7)

	for name, tc := range map[string]struct {
		perms  []auth.Permission
		scoped []string
		status int
	}{
		"no permission": {view, nil, http.StatusForbidden},
		"out of scope":  {admin, []string{"unrelated-account"}, http.StatusNotFound},
	} {
		h := limitedHandler(f, tc.perms, tc.scoped)
		for op, err := range map[string]error{"add": f.addPurchases(ctx, h, start), "assign": f.assign(ctx, h, other)} {
			var ce *clientError
			if errors.As(err, &ce) {
				require.Equal(t, tc.status, ce.code, "%s %s: %s", name, op, ce.message)
			} else {
				require.True(t, IsNotFoundError(err) && tc.status == http.StatusNotFound, "%s %s: %v", name, op, err)
			}
		}
	}
}

// A writer that starts while the editor command holds the plan lock waits, then
// judges the committed marker.
func TestManagedMarkerCommittedUnderLockRefusesWaitingWriters(t *testing.T) {
	ctx := context.Background()
	other := func(f *createConcurrencyFixture) string { return f.extraAccount(ctx, t, "acct-b") }
	writers := map[string]func(f *createConcurrencyFixture, otherID string) error{
		"add": func(f *createConcurrencyFixture, _ string) error {
			return f.addPurchases(ctx, f.handler, time.Now().AddDate(0, 0, 7))
		},
		"assign": func(f *createConcurrencyFixture, o string) error { return f.assign(ctx, f.handler, o) },
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			f := newCreateConcurrencyFixture(ctx, t)
			o := other(f)
			command := f.holdPlanLock(ctx, t)
			done := make(chan error, 1)
			go func() { done <- write(f, o) }()
			awaitLockWait(ctx, t, f)

			_, err := command.Exec(ctx, `UPDATE purchase_plans SET ramp_schedule = ramp_schedule || '{"dates_managed":true}' WHERE id = $1`, f.planID)
			require.NoError(t, err)
			require.NoError(t, command.Commit(ctx))
			requireEditorRefusal(t, <-done)
			require.Zero(t, f.count(ctx, t, execCount))
			require.Equal(t, 1, f.count(ctx, t, acctCount))
		})
	}
}
