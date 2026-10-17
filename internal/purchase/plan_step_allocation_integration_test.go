//go:build integration

package purchase

import (
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Native PostgreSQL, actual Manager attachment; provider completion simulated.
func testEqualCountAttach(t *testing.T, count int, want []int) {
	f := newCumFixture(t, 97)
	percent := 100.0 / float64(count)
	if count == 3 {
		percent = 33
	}
	_, err := f.pool.Exec(f.ctx, `UPDATE purchase_plans SET ramp_schedule = ramp_schedule || jsonb_build_object('allocation_mode','equal_count','total_steps',$2::int,'percent_per_step',$3::float8), services = jsonb_set(services,'{ec2,coverage}','100') WHERE id=$1`, f.planID, count, percent)
	require.NoError(t, err)
	var ledger string
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT ramp_ledger_state FROM purchase_plans WHERE id=$1`, f.planID).Scan(&ledger))
	require.Equal(t, "new", ledger)
	f.shownSuppressed(97)
	for i, n := range want {
		exec, tickErr := f.tick()
		require.NoError(t, tickErr, "exact equal-count attachment")
		assert.Equal(t, n, totalCount(exec), "exact equal-count allocation step %d", i+1)
		f.buy(i + 1)
	}
	var b config.PlanRampBaseline
	if f.plan().RampSchedule.IsComplete() {
		require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT ramp_baseline FROM purchase_plans WHERE id=$1`, f.planID).Scan(&b))
		require.ErrorIs(t, f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
			_, readErr := f.store.GetPlanRampBaselineTx(f.ctx, tx, f.planID)
			return readErr
		}), config.ErrRampLedgerUntrusted)
	} else {
		b, err = f.baseline()
		require.NoError(t, err)
	}
	for _, q := range b {
		assert.Equal(t, 97, q.Count)
	}
	var held map[string]config.PlanRampQuantity
	require.NoError(t, f.store.WithTx(f.ctx, func(tx pgx.Tx) error {
		var err error
		held, err = f.store.ListPlanCommittedTx(f.ctx, tx, f.planID)
		return err
	}))
	require.Len(t, b, 1)
	require.Len(t, held, 1)
	total := 0
	for _, n := range want {
		total += n
	}
	for _, q := range held {
		assert.Equal(t, total, q.Count)
	}
}

func TestEqualCountAttachThree(t *testing.T)       { testEqualCountAttach(t, 3, []int{32, 32, 33}) }
func TestEqualCountAttachNinetySeven(t *testing.T) { testEqualCountAttach(t, 97, []int{1}) }
