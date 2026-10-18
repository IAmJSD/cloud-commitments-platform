//go:build integration

package config

// Real PostgreSQL tests for DeleteCloudAccount against purchase_executions
// history (issue #265). Migration 000053 made the execution FK ON DELETE
// RESTRICT, which used to block the delete for every referencing row
// whatever its status, so one failed execution made an account undeletable.

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedDeleteAccount(ctx context.Context, t *testing.T, store *PostgresStore) string {
	t.Helper()
	id := uuid.New().String()
	_, err := store.db.Exec(ctx, `
		INSERT INTO cloud_accounts (id, name, enabled, provider, external_id, aws_is_org_root, created_at, updated_at)
		VALUES ($1, 'Delete Test Account', true, 'azure', $2, false, now(), now())`, id, id)
	require.NoError(t, err, "seed cloud_accounts")
	return id
}

// liveExecutionStatuses are the schema's statuses outside
// TerminalExecutionStatuses. TestTerminalExecutionStatuses_CoverSchema keeps
// the two lists equal to the status CHECK constraint between them.
var liveExecutionStatuses = []string{"pending", "notified", "approved", "running", "scheduled", "paused"}

// seedDeleteAccountExec inserts an execution in status on accountID with an
// old updated_at, so a later bump is visible. It is a plain INSERT on purpose:
// the BEFORE UPDATE trigger would overwrite an updated_at set by an UPDATE.
func seedDeleteAccountExec(ctx context.Context, t *testing.T, store *PostgresStore, accountID, status string) string {
	t.Helper()
	var id string
	require.NoError(t, store.db.QueryRow(ctx, `
		INSERT INTO purchase_executions (status, step_number, scheduled_date, cloud_account_id, updated_at)
		VALUES ($1, 1, NOW(), $2, $3)
		RETURNING execution_id::text`, status, accountID, agedUpdatedAt).Scan(&id),
		"seed execution in status %s", status)
	return id
}

var agedUpdatedAt = time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

func storedUpdatedAt(ctx context.Context, t *testing.T, store *PostgresStore, execID string) time.Time {
	t.Helper()
	var ts time.Time
	require.NoError(t, store.db.QueryRow(ctx,
		`SELECT updated_at FROM purchase_executions WHERE execution_id = $1`, execID).Scan(&ts))
	return ts.UTC()
}

func storedExecAccount(ctx context.Context, t *testing.T, store *PostgresStore, execID string) (status string, accountID *string) {
	t.Helper()
	require.NoError(t, store.db.QueryRow(ctx,
		`SELECT status, cloud_account_id::text FROM purchase_executions WHERE execution_id = $1`, execID,
	).Scan(&status, &accountID), "execution %s must survive the account delete", execID)
	return status, accountID
}

func accountExists(ctx context.Context, t *testing.T, store *PostgresStore, id string) bool {
	t.Helper()
	var n int
	require.NoError(t, store.db.QueryRow(ctx, `SELECT COUNT(*) FROM cloud_accounts WHERE id = $1`, id).Scan(&n))
	return n == 1
}

// An account whose executions are all terminal is deletable, and the history
// rows survive with their status intact and the account reference detached.
func TestDeleteCloudAccount_TerminalExecutionsAreDetached(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)

	for _, status := range TerminalExecutionStatuses {
		t.Run(status, func(t *testing.T) {
			acct := seedDeleteAccount(ctx, t, store)
			execID := seedDeleteAccountExec(ctx, t, store, acct, status)

			require.NoError(t, store.DeleteCloudAccount(ctx, acct))

			assert.False(t, accountExists(ctx, t, store, acct), "account row must be gone")
			gotStatus, gotAcct := storedExecAccount(ctx, t, store, execID)
			assert.Equal(t, status, gotStatus, "history row status must be unchanged")
			assert.Nil(t, gotAcct, "history row must be detached from the deleted account")
			assert.True(t, storedUpdatedAt(ctx, t, store, execID).Equal(agedUpdatedAt),
				"detaching must not move updated_at (plan health and retention read it as a lifecycle clock)")
		})
	}
}

// A live execution still blocks the delete at the FK, and the transaction
// rolls back whole: the account survives and its terminal history stays
// attached to it.
func TestDeleteCloudAccount_LiveExecutionBlocks(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)

	for _, status := range liveExecutionStatuses {
		t.Run(status, func(t *testing.T) {
			acct := seedDeleteAccount(ctx, t, store)
			liveID := seedDeleteAccountExec(ctx, t, store, acct, status)
			historyID := seedDeleteAccountExec(ctx, t, store, acct, StatusFailed)

			n, err := store.CountLiveExecutionsForAccount(ctx, acct)
			require.NoError(t, err)
			assert.Equal(t, 1, n, "only the %s row is live", status)

			err = store.DeleteCloudAccount(ctx, acct)
			require.Error(t, err)
			var pgErr *pgconn.PgError
			require.True(t, errors.As(err, &pgErr), "want a *pgconn.PgError, got %v", err)
			assert.Equal(t, "23503", pgErr.Code, "the RESTRICT FK must still refuse a live row")

			assert.True(t, accountExists(ctx, t, store, acct), "account must survive a refused delete")
			for _, id := range []string{liveID, historyID} {
				_, gotAcct := storedExecAccount(ctx, t, store, id)
				require.NotNil(t, gotAcct, "a refused delete must leave %s attached", id)
				assert.Equal(t, acct, *gotAcct)
			}
		})
	}
}

// Detaching is scoped to the deleted account: another account's terminal
// execution keeps its reference.
func TestDeleteCloudAccount_DetachIsScopedToAccount(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	gone, kept := seedDeleteAccount(ctx, t, store), seedDeleteAccount(ctx, t, store)
	seedDeleteAccountExec(ctx, t, store, gone, StatusCompleted)
	keptID := seedDeleteAccountExec(ctx, t, store, kept, StatusCompleted)

	require.NoError(t, store.DeleteCloudAccount(ctx, gone))

	_, gotAcct := storedExecAccount(ctx, t, store, keptID)
	require.NotNil(t, gotAcct)
	assert.Equal(t, kept, *gotAcct)
}

// CountLiveExecutionsForAccount counts every non-terminal row of the account
// and nothing else; CountPendingExecutionsForAccount keeps counting only the
// cancelable pending/notified subset.
func TestCountLiveExecutionsForAccount(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	acct, other := seedDeleteAccount(ctx, t, store), seedDeleteAccount(ctx, t, store)
	for _, status := range liveExecutionStatuses {
		seedDeleteAccountExec(ctx, t, store, acct, status)
	}
	for _, status := range TerminalExecutionStatuses {
		seedDeleteAccountExec(ctx, t, store, acct, status)
	}
	seedDeleteAccountExec(ctx, t, store, other, "running")

	live, err := store.CountLiveExecutionsForAccount(ctx, acct)
	require.NoError(t, err)
	assert.Equal(t, len(liveExecutionStatuses), live)

	pending, err := store.CountPendingExecutionsForAccount(ctx, acct)
	require.NoError(t, err)
	assert.Equal(t, 2, pending, "pending + notified")
}

// TerminalExecutionStatuses plus liveExecutionStatuses must be exactly the
// statuses the schema accepts. A status added to the CHECK constraint fails
// this test until it is classified: as terminal (detached on account delete)
// or live (blocks the delete, the default the allowlist gives it).
func TestTerminalExecutionStatuses_CoverSchema(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)

	var def string
	require.NoError(t, store.db.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conname = 'purchase_executions_status_check'`).Scan(&def))
	matches := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(def, -1)
	schema := make([]string, 0, len(matches))
	for _, m := range matches {
		schema = append(schema, m[1])
	}
	require.NotEmpty(t, schema, "could not parse %q", def)

	classified := append(append([]string{}, TerminalExecutionStatuses...), liveExecutionStatuses...)
	sort.Strings(schema)
	sort.Strings(classified)
	assert.Equal(t, schema, classified)
}

// seedDeletePlan inserts a plan in the given enabled state targeting accounts.
func seedDeletePlan(ctx context.Context, t *testing.T, store *PostgresStore, enabled bool, accounts ...string) string {
	t.Helper()
	var id string
	require.NoError(t, store.db.QueryRow(ctx,
		`INSERT INTO purchase_plans (name, enabled, auto_purchase) VALUES ('delete-264', $1, true) RETURNING id::text`,
		enabled).Scan(&id))
	for _, acct := range accounts {
		_, err := store.db.Exec(ctx, `INSERT INTO plan_accounts (plan_id, account_id) VALUES ($1, $2)`, id, acct)
		require.NoError(t, err)
	}
	return id
}

func storedPlanState(ctx context.Context, t *testing.T, store *PostgresStore, planID string) (enabled bool, accounts int, updatedAt time.Time) {
	t.Helper()
	require.NoError(t, store.db.QueryRow(ctx, `
		SELECT p.enabled, (SELECT COUNT(*) FROM plan_accounts pa WHERE pa.plan_id = p.id), p.updated_at
		  FROM purchase_plans p WHERE p.id = $1`, planID).Scan(&enabled, &accounts, &updatedAt))
	return enabled, accounts, updatedAt
}

// Issue #264: a plan whose only account is deleted must not stay enabled with
// zero plan_accounts rows (the unscoped shape migration 000060 removed). Plans
// that keep another account, plans already disabled and plans on other
// accounts are left as they were.
func TestDeleteCloudAccount_DisablesPlansLeftWithoutAccounts(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	gone, kept := seedDeleteAccount(ctx, t, store), seedDeleteAccount(ctx, t, store)

	soleGone := seedDeletePlan(ctx, t, store, true, gone)
	shared := seedDeletePlan(ctx, t, store, true, gone, kept)
	alreadyOff := seedDeletePlan(ctx, t, store, false, gone)
	other := seedDeletePlan(ctx, t, store, true, kept)
	// A plan with no accounts yet (createPlan writes the plan, then its
	// accounts) is not one this delete orphaned.
	unassigned := seedDeletePlan(ctx, t, store, true)
	_, _, offBefore := storedPlanState(ctx, t, store, alreadyOff)
	_, _, otherBefore := storedPlanState(ctx, t, store, other)
	_, _, sharedBefore := storedPlanState(ctx, t, store, shared)

	require.NoError(t, store.DeleteCloudAccount(ctx, gone))

	enabled, n, _ := storedPlanState(ctx, t, store, soleGone)
	assert.Zero(t, n, "the cascade removed the plan's only account")
	assert.False(t, enabled, "a plan left with no accounts must be disabled")

	enabled, n, at := storedPlanState(ctx, t, store, shared)
	assert.Equal(t, 1, n)
	assert.True(t, enabled, "a plan that keeps another account stays enabled")
	assert.True(t, at.Equal(sharedBefore), "a plan that keeps another account must not be rewritten")

	enabled, _, at = storedPlanState(ctx, t, store, alreadyOff)
	assert.False(t, enabled)
	assert.True(t, at.Equal(offBefore), "an already disabled plan must not be rewritten")

	enabled, n, at = storedPlanState(ctx, t, store, other)
	assert.Equal(t, 1, n)
	assert.True(t, enabled, "a plan on another account is untouched")
	assert.True(t, at.Equal(otherBefore), "a plan on another account must not be rewritten")

	enabled, _, _ = storedPlanState(ctx, t, store, unassigned)
	assert.True(t, enabled, "a plan that never targeted the deleted account is untouched")
}

// A refused delete rolls the plan disable back with it.
func TestDeleteCloudAccount_RefusedDeleteKeepsPlanEnabled(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	acct := seedDeleteAccount(ctx, t, store)
	plan := seedDeletePlan(ctx, t, store, true, acct)
	seedDeleteAccountExec(ctx, t, store, acct, "running")

	require.Error(t, store.DeleteCloudAccount(ctx, acct))

	enabled, n, _ := storedPlanState(ctx, t, store, plan)
	assert.True(t, enabled, "the disable must roll back with the refused delete")
	assert.Equal(t, 1, n)
}

// A plan that gains the account in a transaction still open when the delete
// starts is disabled too: the delete locks the account row before it picks
// the plans, so it waits for that writer and sees its committed rows instead
// of cascading them away afterwards and leaving the plan enabled.
func TestDeleteCloudAccount_DisablesPlanThatGainedAccountConcurrently(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	gone, kept := seedDeleteAccount(ctx, t, store), seedDeleteAccount(ctx, t, store)
	plan := seedDeletePlan(ctx, t, store, true, kept)

	writer, err := store.db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback(context.Background()) })
	_, err = writer.Exec(ctx, `DELETE FROM plan_accounts WHERE plan_id = $1`, plan)
	require.NoError(t, err)
	_, err = writer.Exec(ctx, `INSERT INTO plan_accounts (plan_id, account_id) VALUES ($1, $2)`, plan, gone)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- store.DeleteCloudAccount(ctx, gone) }()
	testhelpers.WaitForLockWait(ctx, t, store.db, "%cloud_accounts%")
	require.NoError(t, writer.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))

	enabled, n, _ := storedPlanState(ctx, t, store, plan)
	assert.Zero(t, n)
	assert.False(t, enabled, "the plan's only account is gone, so it must be disabled")
}

// waitForLockWaiters blocks until at least n backends wait on a lock.
func waitForLockWaiters(ctx context.Context, t *testing.T, store *PostgresStore, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var got int
		err := store.db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got)
		return err == nil && got >= n
	}, 15*time.Second, 20*time.Millisecond, "want %d lock waiters", n)
}

// Two deletes of the two accounts of one plan, running at once, must leave the
// plan disabled. Each delete locks only its own account row, so without the
// plan lock both decide while the other's plan_accounts row is still
// committed, neither disables the plan, and both cascades commit. A gate
// holding the plan's plan_accounts rows stops each delete at its cascade,
// after it has decided, so the race is forced on every run.
func TestDeleteCloudAccount_ConcurrentDeletesOfOnePlansAccounts(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	a, b := seedDeleteAccount(ctx, t, store), seedDeleteAccount(ctx, t, store)
	plan := seedDeletePlan(ctx, t, store, true, a, b)

	gate, err := store.db.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	_, err = gate.Exec(ctx, `SELECT 1 FROM plan_accounts WHERE plan_id = $1 FOR UPDATE`, plan)
	require.NoError(t, err)

	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() { doneA <- store.DeleteCloudAccount(ctx, a) }()
	go func() { doneB <- store.DeleteCloudAccount(ctx, b) }()
	waitForLockWaiters(ctx, t, store, 2)
	require.NoError(t, gate.Rollback(ctx))
	require.NoError(t, testhelpers.Await(t, doneA))
	require.NoError(t, testhelpers.Await(t, doneB))

	enabled, n, _ := storedPlanState(ctx, t, store, plan)
	assert.Zero(t, n)
	assert.False(t, enabled, "both accounts are gone, so the plan must be disabled")
}

// SetPlanAccounts narrowing {A, B} to {A} while A is deleted: whichever
// commits first, the plan never ends up enabled with no accounts, and the two
// never deadlock (both lock the plan row before the account row).
func TestDeleteCloudAccount_ConcurrentSetPlanAccounts(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	for i := 0; i < 20; i++ {
		a, b := seedDeleteAccount(ctx, t, store), seedDeleteAccount(ctx, t, store)
		plan := seedDeletePlan(ctx, t, store, true, a, b)

		start := make(chan struct{})
		delDone, setDone := make(chan error, 1), make(chan error, 1)
		go func() { <-start; delDone <- store.DeleteCloudAccount(ctx, a) }()
		go func() { <-start; setDone <- store.SetPlanAccounts(ctx, plan, []string{a}) }()
		close(start)
		delErr, setErr := testhelpers.Await(t, delDone), testhelpers.Await(t, setDone)

		for _, e := range []error{delErr, setErr} {
			var pgErr *pgconn.PgError
			if errors.As(e, &pgErr) {
				require.NotEqual(t, "40P01", pgErr.Code, "iteration %d: deadlock: %v", i, e)
			}
		}
		require.NoError(t, delErr, "iteration %d", i)
		enabled, n, _ := storedPlanState(ctx, t, store, plan)
		if enabled {
			require.NotZero(t, n, "iteration %d: plan enabled with no accounts (set err: %v)", i, setErr)
		}
	}
}

// An editor-managed ramp plan (dates_managed, #631) is disabled like any
// other, and only its enabled flag changes: the schedule, baseline, ledger
// state, services and next execution date are left as they were.
func TestDeleteCloudAccount_DisablesManagedRampPlanKeepingSchedule(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	acct := seedDeleteAccount(ctx, t, store)
	var plan string
	require.NoError(t, store.db.QueryRow(ctx, `
		INSERT INTO purchase_plans (name, enabled, auto_purchase, services, ramp_schedule, next_execution_date)
		VALUES ('managed-264', true, false,
		        '{"azure:vm":{"provider":"azure","service":"vm","enabled":true,"term":1,"payment":"upfront"}}',
		        '{"type":"linear","total_steps":3,"current_step":1,"step_interval_days":30,"percent_per_step":33,"dates_managed":true}',
		        NOW() + INTERVAL '10 days')
		RETURNING id::text`).Scan(&plan))
	_, err := store.db.Exec(ctx, `INSERT INTO plan_accounts (plan_id, account_id) VALUES ($1, $2)`, plan, acct)
	require.NoError(t, err)

	const scheduleCols = `SELECT ramp_schedule::text, COALESCE(ramp_baseline::text, ''), ramp_ledger_state,
		services::text, next_execution_date::text FROM purchase_plans WHERE id = $1`
	before := make([]string, 5)
	require.NoError(t, store.db.QueryRow(ctx, scheduleCols, plan).Scan(&before[0], &before[1], &before[2], &before[3], &before[4]))

	require.NoError(t, store.DeleteCloudAccount(ctx, acct))

	enabled, _, _ := storedPlanState(ctx, t, store, plan)
	assert.False(t, enabled, "a managed plan left with no accounts must be disabled")
	after := make([]string, 5)
	require.NoError(t, store.db.QueryRow(ctx, scheduleCols, plan).Scan(&after[0], &after[1], &after[2], &after[3], &after[4]))
	assert.Equal(t, before, after, "only enabled may change on a managed plan")
	assert.Contains(t, after[0], `"dates_managed": true`)
}
