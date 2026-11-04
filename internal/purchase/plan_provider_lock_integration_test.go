//go:build integration

package purchase

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// S7a (real PostgreSQL; the resolve and attach below are test-local stand-ins,
// since nothing attaches recommendations to a plan step yet). Several plans
// target one account and provider, each wanting more than a fair share of the
// capacity the stored recommendation shows. A tick is: resolve what is still
// shown (capacity minus committed suppressions), pause at a barrier, then write
// min(wanted, shown) as a suppression. Without the per-provider lock every
// tick resolves the full capacity before anyone writes, so the sum of
// suppressions exceeds what was shown.
//
// Concurrent ticks use one pooled connection each; the test pool has 10, so
// planProviderTicks stays at MaxConnections/2 - 1.
const (
	planProviderTicks    = 4
	planProviderCapacity = 10
	planProviderWanted   = 3
)

// planProviderLockWait matches LockPlanProvidersTx's advisory lock statement.
const planProviderLockWait = "%pg_advisory_xact_lock%"

func runProviderTicks(ctx context.Context, t *testing.T, store *config.PostgresStore, pool *pgxpool.Pool, lock bool) int {
	t.Helper()
	account := uuid.NewString()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		resolved int
	)
	arrived := func() int {
		mu.Lock()
		defer mu.Unlock()
		return resolved
	}
	// The barrier waits until every tick has resolved or is observed parked
	// on the provider lock. With the lock the other ticks cannot resolve until
	// this one commits; without it they all arrive. A fixed give-up timeout
	// here let a slow unlocked tick miss the barrier, see the first tick's
	// committed suppression, and hide the over-commit the control relies on.
	arrive := func() {
		mu.Lock()
		resolved++
		mu.Unlock()
		if err := testhelpers.AwaitArrivalsOrLockWaits(ctx, pool, planProviderTicks, arrived, planProviderLockWait); err != nil {
			t.Error(err)
		}
	}
	for i := 0; i < planProviderTicks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.WithTx(ctx, func(tx pgx.Tx) error {
				if lock {
					if err := store.LockPlanProvidersTx(ctx, tx, []string{"aws"}); err != nil {
						return err
					}
				}
				var used int
				if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(suppressed_count), 0) FROM purchase_suppressions WHERE account_id = $1`, account).Scan(&used); err != nil {
					return err
				}
				shown := planProviderCapacity - used
				arrive()
				take := min(planProviderWanted, shown)
				if take <= 0 {
					return nil
				}
				return store.CreateSuppressionTx(ctx, tx, &config.PurchaseSuppression{
					ExecutionID: uuid.NewString(), AccountID: account, Provider: "aws", Service: "ec2", Region: "us-east-1",
					ResourceType: "m5.large", SuppressedCount: take, ExpiresAt: time.Now().Add(time.Hour),
				})
			})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	var total int
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(SUM(suppressed_count), 0) FROM purchase_suppressions WHERE account_id = $1`, account).Scan(&total)
	}))
	return total
}

func TestPlanProviderLock_AttachedNeverExceedsShown(t *testing.T) {
	ctx := context.Background()
	store, pool := rampStepStore(ctx, t)

	// Control: the harness really races. If this stops over-committing, the
	// assertion below proves nothing.
	assert.Greater(t, runProviderTicks(ctx, t, store, pool, false), planProviderCapacity,
		"without the lock the ticks must over-commit, otherwise the test cannot detect a missing lock")

	// Treatment: with the lock the ticks serialize and exactly fill the capacity.
	assert.Equal(t, planProviderCapacity, runProviderTicks(ctx, t, store, pool, true))
}

func TestLockPlanProvidersTx_Validation(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
		assert.Error(t, store.LockPlanProvidersTx(ctx, tx, nil))
		assert.Error(t, store.LockPlanProvidersTx(ctx, tx, []string{"aws", ""}))
		return store.LockPlanProvidersTx(ctx, tx, []string{"gcp", "aws", "aws"})
	}))
}
