//go:build integration

package config

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

// Audited writers (W16, W21, W12) against the editor command's lock footprint.
// None of them gains a managed-plan refusal: each either queues behind the
// command and is accepted, or rechecks its own predicate on the committed row.

func inBackground(f func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	return done
}

// W16: a fan-out child row after a real purchase must never be refused.
func TestFanOutChildQueuesBehindCommandAndIsAccepted(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 1, 3)
	command := testhelpers.HoldCommandFootprint(ctx, t, f.store.db, f.planID)
	child := &PurchaseExecution{ExecutionID: "00000000-0000-0000-0000-0000000000c1", IdempotencyKey: "root:child", PlanID: f.planID,
		CloudAccountID: &f.accounts[0], Status: StatusCompleted, StepNumber: 2}
	done := inBackground(func() error { return f.store.SavePurchaseExecution(ctx, child) })
	testhelpers.WaitForLockWait(ctx, t, f.store.db, "%INSERT INTO purchase_executions%")
	command.SetManaged(ctx, t, f.planID)
	require.NoError(t, command.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done), "audit rows after money moved are never refused")

	require.Contains(t, testhelpers.HoldCommandFootprint(ctx, t, f.store.db, f.planID).Executions, child.ExecutionID)
}

// W21: the prune DELETE rechecks the committed row after the wait. The
// fixture ramp is finished (3 of 3): on an unfinished ramp the retention
// trigger of migration 000124 already keeps every capacity-holding row, so
// only the SQL predicate would be unexercised.
func TestPruneRechecksRowAfterCommandCancelsIt(t *testing.T) {
	for _, commandWins := range []bool{true, false} {
		name := map[bool]string{true: "command first", false: "prune first"}[commandWins]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newRampFixture(ctx, t, 0, 3, 3)
			row := f.row(ctx, t, 2, nil, "pending")
			_, err := f.store.db.Exec(ctx, `UPDATE purchase_executions SET expires_at = NOW() - INTERVAL '40 days' WHERE execution_id = $1`, row.ExecutionID)
			require.NoError(t, err)
			prune := func() error { _, pErr := f.store.CleanupOldExecutions(ctx, 30); return pErr }

			if !commandWins {
				deleted, pErr := f.store.CleanupOldExecutions(ctx, 30)
				require.NoError(t, pErr)
				require.EqualValues(t, 1, deleted)
				require.NotContains(t, testhelpers.HoldCommandFootprint(ctx, t, f.store.db, f.planID).Executions, row.ExecutionID)
				return
			}
			command := testhelpers.HoldCommandFootprint(ctx, t, f.store.db, f.planID)
			done := inBackground(prune)
			testhelpers.WaitForLockWait(ctx, t, f.store.db, "%DELETE FROM purchase_executions%")
			_, err = command.Exec(ctx, `UPDATE purchase_executions SET status = 'canceled', updated_at = NOW() WHERE execution_id = $1`, row.ExecutionID)
			require.NoError(t, err)
			require.NoError(t, command.Commit(ctx))
			require.NoError(t, testhelpers.Await(t, done))
			require.Equal(t, 1, f.countRows(ctx, t, row.ExecutionID), "the canceled row is no longer prunable and survives")
		})
	}
}

func (f *rampFixture) countRows(ctx context.Context, t *testing.T, execID string) (n int) {
	t.Helper()
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT count(*) FROM purchase_executions WHERE execution_id = $1`, execID).Scan(&n))
	return n
}

// W12: deleting an account queues behind the command's account FOR SHARE, so
// the command's captured account set cannot shrink mid-flight.
func TestAccountDeleteWaitsForCommandAccountShare(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 1, 3)
	command := testhelpers.HoldCommandFootprint(ctx, t, f.store.db, f.planID)
	done := inBackground(func() error { return f.store.DeleteCloudAccount(ctx, f.accounts[0]) })
	testhelpers.WaitForLockWait(ctx, t, f.store.db, "%FOR UPDATE OF p%")
	var n int
	require.NoError(t, command.QueryRow(ctx, `SELECT count(*) FROM plan_accounts WHERE plan_id = $1`, f.planID).Scan(&n))
	require.Equal(t, 1, n, "the command still sees its account while the delete waits")
	require.NoError(t, command.Commit(ctx))
	require.NoError(t, testhelpers.Await(t, done))

	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT count(*) FROM plan_accounts WHERE plan_id = $1`, f.planID).Scan(&n))
	require.Zero(t, n, "the cascade runs after the command committed (plan_accounts only; ladder_configs, the recommendations cache and credentials also cascade, none is editor-managed plan data; purchase_executions.cloud_account_id is RESTRICT)")
}
