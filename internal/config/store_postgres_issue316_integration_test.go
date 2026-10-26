//go:build integration

package config

// Real-DB tests for the store-side findings of issue #316 (2026-09-02 audit).

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedRevocationRow(ctx context.Context, t *testing.T, store *PostgresStore, purchaseID string) {
	t.Helper()
	require.NoError(t, store.SavePurchaseHistory(ctx, &PurchaseHistoryRecord{
		AccountID: "acct", PurchaseID: purchaseID, Timestamp: time.Now(), Provider: "azure", Service: "compute",
		Region: "eastus", ResourceType: "Standard_D2s_v3", Count: 1, Term: 1, Payment: "All Upfront",
	}))
}

// inFlightIndexed counts the row under the predicate of the partial index
// idx_purchase_history_revocation_in_flight (migration 000072).
func inFlightIndexed(ctx context.Context, t *testing.T, store *PostgresStore, purchaseID string) int {
	t.Helper()
	var n int
	require.NoError(t, store.db.QueryRow(ctx,
		`SELECT count(*) FROM purchase_history WHERE purchase_id = $1 AND revocation_in_flight`, purchaseID).Scan(&n))
	return n
}

// TestMarkPurchaseRevoked_ClearsRevocationInFlight covers A04-013: migration
// 000072 and PurchaseHistoryRecord.RevocationInFlight document that a
// successful MarkPurchaseRevoked resets the flag, but the UPDATE left it set,
// so every revoked Azure row read back as in flight and stayed in the
// partial index for good.
func TestMarkPurchaseRevoked_ClearsRevocationInFlight(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	t.Run("flip then mark clears the flag", func(t *testing.T) {
		const id = "azure-revoke-316-a"
		seedRevocationRow(ctx, t, store, id)
		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))
		require.Equal(t, 1, inFlightIndexed(ctx, t, store, id), "precondition: the flip sets the flag")

		revokedAt := time.Now().UTC().Truncate(time.Microsecond)
		amount := 12.5
		require.NoError(t, store.MarkPurchaseRevoked(ctx, id, revokedAt, "direct-api", "", &amount, "USD"))

		got, err := store.GetPurchaseHistoryByPurchaseID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got.RevokedAt)
		assert.True(t, revokedAt.Equal(*got.RevokedAt))
		assert.False(t, got.RevocationInFlight, "a completed revocation must not read back as in flight")
		assert.Zero(t, inFlightIndexed(ctx, t, store, id), "the row must leave the in-flight partial index")

		pending, err := store.GetPurchaseHistoryInFlight(ctx)
		require.NoError(t, err)
		for _, r := range pending {
			assert.NotEqual(t, id, r.PurchaseID)
		}
	})

	t.Run("a repeat mark is still a no-op", func(t *testing.T) {
		const id = "azure-revoke-316-b"
		seedRevocationRow(ctx, t, store, id)
		first := time.Now().UTC().Truncate(time.Microsecond)
		require.NoError(t, store.MarkPurchaseRevoked(ctx, id, first, "direct-api", "", nil, ""))
		require.NoError(t, store.MarkPurchaseRevoked(ctx, id, first.Add(time.Hour), "support-case", "case-1", nil, ""))

		got, err := store.GetPurchaseHistoryByPurchaseID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got.RevokedAt)
		assert.True(t, first.Equal(*got.RevokedAt), "the first revocation's stamp must stand")
		assert.Equal(t, "direct-api", got.RevokedVia)
		assert.False(t, got.RevocationInFlight)
	})

	// The handler's retry loop and the finalize_revocations sweep can both be
	// marking the same in-flight row. The UPDATE's row lock serializes them:
	// one stamps the row, the rest re-check revoked_at IS NULL after the lock,
	// match nothing and take the idempotent path. Whatever the interleaving,
	// the flag ends cleared and exactly one stamp lands.
	t.Run("concurrent marks of one in-flight row", func(t *testing.T) {
		const id = "azure-revoke-316-c"
		seedRevocationRow(ctx, t, store, id)
		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))

		const workers = 8
		base := time.Now().UTC().Truncate(time.Microsecond)
		stamps := make([]time.Time, workers)
		errs := make([]error, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range workers {
			stamps[i] = base.Add(time.Duration(i) * time.Second)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = store.MarkPurchaseRevoked(ctx, id, stamps[i], "direct-api", "", nil, "")
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			require.NoError(t, err, "worker %d", i)
		}

		got, err := store.GetPurchaseHistoryByPurchaseID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got.RevokedAt)
		assert.Contains(t, stamps, got.RevokedAt.UTC(), "exactly one worker's stamp must land")
		assert.False(t, got.RevocationInFlight)
		assert.Zero(t, inFlightIndexed(ctx, t, store, id))
	})
}

// TestFlipPurchaseRevocationInFlight_CompareAndSet covers the follow-up to
// A04-013 tracked on issue #316: the flip was an unconditional UPDATE, so a
// stale revoke request reaching it after another request's
// MarkPurchaseRevoked set the flag again on a revoked row, and
// MarkPurchaseRevoked (guarded on revoked_at IS NULL) never cleared it.
func TestFlipPurchaseRevocationInFlight_CompareAndSet(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	t.Run("a revoked row is refused and keeps the flag clear", func(t *testing.T) {
		const id = "azure-flip-316-a"
		seedRevocationRow(ctx, t, store, id)
		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))
		require.NoError(t, store.MarkPurchaseRevoked(ctx, id, time.Now().UTC(), "direct-api", "", nil, ""))

		err := store.FlipPurchaseRevocationInFlight(ctx, id)
		require.ErrorIs(t, err, ErrPurchaseAlreadyRevoked)
		assert.NotContains(t, err.Error(), "not found")

		got, err := store.GetPurchaseHistoryByPurchaseID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got.RevokedAt)
		assert.False(t, got.RevocationInFlight, "a revoked row must not be flagged in flight again")
		assert.Zero(t, inFlightIndexed(ctx, t, store, id))
	})

	t.Run("an unrevoked row still flips, idempotently", func(t *testing.T) {
		const id = "azure-flip-316-b"
		seedRevocationRow(ctx, t, store, id)
		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))
		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id), "a second flip of an in-flight row is a no-op")
		assert.Equal(t, 1, inFlightIndexed(ctx, t, store, id))
	})

	t.Run("a missing row is still not found", func(t *testing.T) {
		err := store.FlipPurchaseRevocationInFlight(ctx, "azure-flip-316-missing")
		require.ErrorContains(t, err, "not found")
		assert.NotErrorIs(t, err, ErrPurchaseAlreadyRevoked)
	})

	// The race the guard closes: one request's MarkPurchaseRevoked against
	// stale requests' flips on the same in-flight row. The row lock
	// serializes them; a flip that waits on the mark re-checks
	// revoked_at IS NULL on the committed row and matches nothing. Whatever
	// the interleaving, the row ends revoked with the flag clear, and every
	// flip either landed before the mark or reports the row revoked.
	t.Run("stale flips racing a mark", func(t *testing.T) {
		const rounds, flips = 20, 4
		for round := range rounds {
			id := fmt.Sprintf("azure-flip-316-race-%d", round)
			seedRevocationRow(ctx, t, store, id)
			require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))

			errs := make([]error, flips+1)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range flips + 1 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if i == 0 {
						errs[i] = store.MarkPurchaseRevoked(ctx, id, time.Now().UTC(), "direct-api", "", nil, "")
						return
					}
					errs[i] = store.FlipPurchaseRevocationInFlight(ctx, id)
				}()
			}
			close(start)
			wg.Wait()

			require.NoError(t, errs[0], "round %d: the mark", round)
			for i, err := range errs[1:] {
				if err != nil {
					require.ErrorIs(t, err, ErrPurchaseAlreadyRevoked, "round %d: flip %d", round, i)
				}
			}
			got, err := store.GetPurchaseHistoryByPurchaseID(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, got.RevokedAt, "round %d", round)
			require.False(t, got.RevocationInFlight, "round %d: a flip that lost to the mark must not re-flag the revoked row", round)
		}
	})
}

// TestSetPlanAccounts_ProviderCheckFailsClosed covers the store-side guard of
// A04-012. DerivePlanProviders skipped "provider:service" keys (the form
// migration 000040 wrote) and legacy keys with no provider prefix, and an
// empty provider set meant "accept an account of any provider".
func TestSetPlanAccounts_ProviderCheckFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	azure := &CloudAccount{Name: "prod-azure-316", Provider: "azure", ExternalID: uuid.NewString()}
	require.NoError(t, store.CreateCloudAccount(ctx, azure))

	for _, tc := range []struct {
		name     string
		services map[string]ServiceConfig
		wantErr  string
	}{
		{
			name: "colon-keyed AWS plan rejects an Azure account",
			services: map[string]ServiceConfig{
				"aws:savings-plans-compute":     {Provider: "aws", Service: "savings-plans", Enabled: true},
				"aws:savings-plans-ec2instance": {Provider: "aws", Service: "savings-plans", Enabled: true},
			},
			wantErr: "plan provider mismatch:",
		},
		{
			name:     "legacy-keyed AWS plan rejects an Azure account by its provider field",
			services: map[string]ServiceConfig{"original-key": {Provider: "aws", Service: "ec2", Enabled: true}},
			wantErr:  "plan provider mismatch:",
		},
		{
			name:     "plan with no identifiable provider rejects every account",
			services: map[string]ServiceConfig{"ec2": {Service: "ec2", Enabled: true}},
			wantErr:  "identifies a provider",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &PurchasePlan{Name: "plan " + tc.name, Services: tc.services}
			require.NoError(t, store.CreatePurchasePlan(ctx, plan))

			err := store.SetPlanAccounts(ctx, plan.ID, []string{azure.ID})
			require.ErrorContains(t, err, tc.wantErr)

			accounts, err := store.GetPlanAccounts(ctx, plan.ID)
			require.NoError(t, err)
			assert.Empty(t, accounts, "the Azure account must not be attached")
		})
	}

	// A legacy alias kept under its original key (#1123's singleton-alias
	// edit path) is a supported plan shape: its provider field identifies it,
	// so a matching account is still accepted.
	t.Run("legacy-keyed Azure plan accepts an Azure account", func(t *testing.T) {
		plan := &PurchasePlan{Name: "legacy alias plan", Services: map[string]ServiceConfig{
			"original-key": {Provider: "azure", Service: "vm", Enabled: true},
		}}
		require.NoError(t, store.CreatePurchasePlan(ctx, plan))
		require.NoError(t, store.SetPlanAccounts(ctx, plan.ID, []string{azure.ID}))

		accounts, err := store.GetPlanAccounts(ctx, plan.ID)
		require.NoError(t, err)
		require.Len(t, accounts, 1)
		assert.Equal(t, azure.ID, accounts[0].ID)
	})
}
