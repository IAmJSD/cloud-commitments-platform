//go:build integration

package purchase

// Real PostgreSQL, no cloud calls. Issue #1523: the finalize_revocations sweep
// marked every flagged, unrevoked row revoked, including rows whose request
// died between the flip and the Azure Return, so a purchase that was never
// refunded read back as revoked while it was still active and billed.

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedAzurePurchase(ctx context.Context, t *testing.T, store *config.PostgresStore, purchaseID string) {
	t.Helper()
	require.NoError(t, store.SavePurchaseHistory(ctx, &config.PurchaseHistoryRecord{
		AccountID: "acct", PurchaseID: purchaseID, Timestamp: time.Now(), Provider: "azure", Service: "compute",
		Region: "eastus", ResourceType: "Standard_D2s_v3", Count: 1, Term: 1, Payment: "All Upfront",
	}))
}

func loadPurchase(ctx context.Context, t *testing.T, store *config.PostgresStore, purchaseID string) *config.PurchaseHistoryRecord {
	t.Helper()
	got, err := store.GetPurchaseHistoryByPurchaseID(ctx, purchaseID)
	require.NoError(t, err)
	require.NotNil(t, got)
	return got
}

// Acceptance criterion 1: a row flagged without a recorded Return is not
// marked revoked by the sweep. Both shapes are covered: a request that died
// after the flip (the flip stamped the row), and a row flagged before
// migration 000148 (no stamp at all).
func TestFinalizeInFlightRevocations_FlagWithoutReturnIsNotRevoked(t *testing.T) {
	ctx := context.Background()
	store, pool := rampStepStore(ctx, t)
	m := &Manager{config: store}

	const crashed, legacy = "azure-1523-crashed-before-return", "azure-1523-legacy-flag"
	seedAzurePurchase(ctx, t, store, crashed)
	require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, crashed))
	seedAzurePurchase(ctx, t, store, legacy)
	_, err := pool.Exec(ctx, `UPDATE purchase_history SET revocation_in_flight = true WHERE purchase_id = $1`, legacy)
	require.NoError(t, err)

	res, err := m.FinalizeInFlightRevocations(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Finalized, "no Return was recorded, so nothing may be finalized")

	for _, id := range []string{crashed, legacy} {
		got := loadPurchase(ctx, t, store, id)
		assert.Nil(t, got.RevokedAt, "%s: a purchase with no recorded Return must not read back as revoked", id)
		assert.True(t, got.RevocationInFlight, "%s: a fresh flag stays until the timeout", id)
	}
}

// Acceptance criterion 2: a row whose Return was recorded is still finalized,
// as before. And once a flag with no recorded Return has outlived the
// timeout, the sweep clears it (still unrevoked) so the purchase can be
// revoked again.
func TestFinalizeInFlightRevocations_RecordedReturnFinalizedStaleFlagReleased(t *testing.T) {
	ctx := context.Background()
	store, pool := rampStepStore(ctx, t)
	m := &Manager{config: store}

	const returned, stale = "azure-1523-returned", "azure-1523-stale-flag"
	seedAzurePurchase(ctx, t, store, returned)
	require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, returned))
	require.NoError(t, store.MarkRevocationReturnPosted(ctx, returned))
	seedAzurePurchase(ctx, t, store, stale)
	require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, stale))
	_, err := pool.Exec(ctx, `
		UPDATE purchase_history
		   SET revocation_in_flight_at = now() - make_interval(secs => $2)
		 WHERE purchase_id = $1`, stale, (staleRevocationInFlightAfter + time.Minute).Seconds())
	require.NoError(t, err)

	res, err := m.FinalizeInFlightRevocations(ctx)
	require.NoError(t, err)
	assert.Equal(t, FinalizeResult{Found: 1, Finalized: 1, Released: 1}, *res)

	got := loadPurchase(ctx, t, store, returned)
	require.NotNil(t, got.RevokedAt, "a recorded Return is finalized")
	assert.Equal(t, "direct-api", got.RevokedVia)
	assert.False(t, got.RevocationInFlight)

	got = loadPurchase(ctx, t, store, stale)
	assert.Nil(t, got.RevokedAt, "a released row is not revoked")
	assert.False(t, got.RevocationInFlight, "the stale flag is cleared so the user can retry")

	res, err = m.FinalizeInFlightRevocations(ctx)
	require.NoError(t, err)
	assert.Equal(t, FinalizeResult{}, *res, "a second tick finds nothing left to do")
}
