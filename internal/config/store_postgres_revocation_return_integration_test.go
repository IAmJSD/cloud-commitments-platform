//go:build integration

package config

// Real-DB tests for issue #1523: the finalize_revocations sweep's queue is
// rows with a recorded Azure Return, and a flag with no recorded Return is
// released after a timeout instead of being finalized.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testStaleAfter = 2 * time.Hour

type revocationState struct {
	inFlight       bool
	inFlightAt     *time.Time
	returnPostedAt *time.Time
	revokedAt      *time.Time
}

func readRevocationState(ctx context.Context, t *testing.T, store *PostgresStore, purchaseID string) revocationState {
	t.Helper()
	var st revocationState
	require.NoError(t, store.db.QueryRow(ctx, `
		SELECT revocation_in_flight, revocation_in_flight_at, return_posted_at, revoked_at
		  FROM purchase_history WHERE purchase_id = $1`, purchaseID).
		Scan(&st.inFlight, &st.inFlightAt, &st.returnPostedAt, &st.revokedAt))
	return st
}

// backdateFlip makes a flagged row look as if it was flipped age ago.
func backdateFlip(ctx context.Context, t *testing.T, store *PostgresStore, purchaseID string, age time.Duration) {
	t.Helper()
	_, err := store.db.Exec(ctx, `
		UPDATE purchase_history
		   SET revocation_in_flight_at = now() - make_interval(secs => $2)
		 WHERE purchase_id = $1`, purchaseID, age.Seconds())
	require.NoError(t, err)
}

func finalizeQueueIDs(ctx context.Context, t *testing.T, store *PostgresStore) map[string]bool {
	t.Helper()
	rows, err := store.GetPurchaseHistoryInFlight(ctx)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.PurchaseID] = true
	}
	return ids
}

func TestGetPurchaseHistoryInFlight_RequiresRecordedReturn(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	const flaggedOnly, returned, returnedFlagCleared, revoked = "rr-1523-flagged-only", "rr-1523-returned", "rr-1523-returned-cleared", "rr-1523-revoked"
	for _, id := range []string{flaggedOnly, returned, returnedFlagCleared, revoked} {
		seedRevocationRow(ctx, t, store, id)
		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))
	}
	require.NoError(t, store.MarkRevocationReturnPosted(ctx, returned))
	// A concurrent request's failed Return cleared the flag after this one's
	// Return was accepted: the refund still happened and must be finalized.
	require.NoError(t, store.MarkRevocationReturnPosted(ctx, returnedFlagCleared))
	require.NoError(t, store.ClearRevocationInFlight(ctx, returnedFlagCleared))
	require.NoError(t, store.MarkRevocationReturnPosted(ctx, revoked))
	require.NoError(t, store.MarkPurchaseRevoked(ctx, revoked, time.Now().UTC(), "direct-api", "", nil, ""))

	rows, err := store.GetPurchaseHistoryInFlight(ctx)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, r := range rows {
		got[r.PurchaseID] = r.RevocationInFlight
	}
	assert.NotContains(t, got, flaggedOnly, "a flag with no recorded Return must never reach the finalize queue")
	assert.NotContains(t, got, revoked)
	require.Contains(t, got, returned)
	assert.True(t, got[returned], "the flag is read from the row")
	require.Contains(t, got, returnedFlagCleared)
	assert.False(t, got[returnedFlagCleared], "the flag is read from the row, not assumed")
}

func TestMarkRevocationReturnPosted(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	t.Run("a missing row is not found", func(t *testing.T) {
		err := store.MarkRevocationReturnPosted(ctx, "rr-1523-missing")
		require.ErrorContains(t, err, "not found")
	})

	t.Run("the first stamp stands", func(t *testing.T) {
		const id = "rr-1523-stamp-twice"
		seedRevocationRow(ctx, t, store, id)
		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))
		require.NoError(t, store.MarkRevocationReturnPosted(ctx, id))
		first := readRevocationState(ctx, t, store, id).returnPostedAt
		require.NotNil(t, first)
		time.Sleep(5 * time.Millisecond)
		require.NoError(t, store.MarkRevocationReturnPosted(ctx, id))
		again := readRevocationState(ctx, t, store, id).returnPostedAt
		require.NotNil(t, again)
		assert.True(t, first.Equal(*again))
	})

	t.Run("a revoked row is left alone", func(t *testing.T) {
		const id = "rr-1523-stamp-revoked"
		seedRevocationRow(ctx, t, store, id)
		require.NoError(t, store.MarkPurchaseRevoked(ctx, id, time.Now().UTC(), "support-case", "case-1", nil, ""))
		require.NoError(t, store.MarkRevocationReturnPosted(ctx, id))
		assert.Nil(t, readRevocationState(ctx, t, store, id).returnPostedAt)
	})
}

func TestReleaseStaleRevocationsInFlight(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	t.Run("a non-positive timeout is refused", func(t *testing.T) {
		_, err := store.ReleaseStaleRevocationsInFlight(ctx, 0)
		require.Error(t, err)
	})

	t.Run("only stale flags with no recorded Return are released", func(t *testing.T) {
		const fresh, almost, stale, staleReturned, staleRevoked = "rr-1523-fresh", "rr-1523-almost", "rr-1523-stale", "rr-1523-stale-returned", "rr-1523-stale-revoked"
		for _, id := range []string{fresh, almost, stale, staleReturned, staleRevoked} {
			seedRevocationRow(ctx, t, store, id)
			require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))
		}
		require.NotNil(t, readRevocationState(ctx, t, store, fresh).inFlightAt, "the flip stamps revocation_in_flight_at")
		require.NoError(t, store.MarkRevocationReturnPosted(ctx, staleReturned))
		_, err := store.db.Exec(ctx, `UPDATE purchase_history SET revoked_at = now() WHERE purchase_id = $1`, staleRevoked)
		require.NoError(t, err)
		for _, id := range []string{stale, staleReturned, staleRevoked} {
			backdateFlip(ctx, t, store, id, testStaleAfter+time.Minute)
		}
		backdateFlip(ctx, t, store, almost, testStaleAfter-time.Minute)

		released, err := store.ReleaseStaleRevocationsInFlight(ctx, testStaleAfter)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{stale}, released)

		st := readRevocationState(ctx, t, store, stale)
		assert.False(t, st.inFlight, "the stale flag is cleared")
		assert.Nil(t, st.inFlightAt)
		assert.Nil(t, st.revokedAt, "a released row is never marked revoked")
		assert.Nil(t, st.returnPostedAt)
		assert.True(t, readRevocationState(ctx, t, store, fresh).inFlight, "a flag younger than the timeout stays")
		assert.True(t, readRevocationState(ctx, t, store, almost).inFlight, "a flag just under the timeout stays")
		assert.True(t, readRevocationState(ctx, t, store, staleReturned).inFlight, "a recorded Return is finalized, not released")
		assert.True(t, readRevocationState(ctx, t, store, staleRevoked).inFlight, "revoked rows are not the release's business")

		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, stale), "a released row can be revoked again")
	})

	// A row flagged before migration 000148 (or by a binary without it) has no
	// flip timestamp. Its age is unknown, so it is stamped on first sight and
	// released only once the timeout has passed from then.
	t.Run("a legacy flag is aged from first sight", func(t *testing.T) {
		const id = "rr-1523-legacy"
		seedRevocationRow(ctx, t, store, id)
		_, err := store.db.Exec(ctx, `UPDATE purchase_history SET revocation_in_flight = true WHERE purchase_id = $1`, id)
		require.NoError(t, err)
		require.Nil(t, readRevocationState(ctx, t, store, id).inFlightAt)

		released, err := store.ReleaseStaleRevocationsInFlight(ctx, testStaleAfter)
		require.NoError(t, err)
		assert.NotContains(t, released, id)
		st := readRevocationState(ctx, t, store, id)
		assert.True(t, st.inFlight)
		require.NotNil(t, st.inFlightAt, "first sight stamps the row")
		assert.NotContains(t, finalizeQueueIDs(ctx, t, store), id, "a legacy flag is never finalized")

		backdateFlip(ctx, t, store, id, testStaleAfter+time.Minute)
		released, err = store.ReleaseStaleRevocationsInFlight(ctx, testStaleAfter)
		require.NoError(t, err)
		assert.Contains(t, released, id)
		assert.False(t, readRevocationState(ctx, t, store, id).inFlight)
	})
}

// The handler's MarkRevocationReturnPosted can race the sweep's release of the
// same stale row. Whichever commits first, a recorded Return must end in the
// finalize queue: the release re-checks return_posted_at IS NULL on the locked
// row, and the queue does not require the flag.
func TestReleaseStaleRevocationsInFlight_RacesReturnRecord(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	releasedRounds := 0
	for round := range 30 {
		id := fmt.Sprintf("rr-1523-race-return-%d", round)
		seedRevocationRow(ctx, t, store, id)
		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))
		backdateFlip(ctx, t, store, id, testStaleAfter+time.Minute)

		var wg sync.WaitGroup
		var stampErr, releaseErr error
		var released []string
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; stampErr = store.MarkRevocationReturnPosted(ctx, id) }()
		go func() {
			defer wg.Done()
			<-start
			released, releaseErr = store.ReleaseStaleRevocationsInFlight(ctx, testStaleAfter)
		}()
		close(start)
		wg.Wait()
		require.NoError(t, stampErr)
		require.NoError(t, releaseErr)

		st := readRevocationState(ctx, t, store, id)
		require.NotNil(t, st.returnPostedAt, "round %d", round)
		assert.Nil(t, st.revokedAt, "round %d: the release never revokes", round)
		if len(released) > 0 {
			releasedRounds++
			assert.False(t, st.inFlight, "round %d", round)
		} else {
			assert.True(t, st.inFlight, "round %d", round)
		}
		require.True(t, finalizeQueueIDs(ctx, t, store)[id], "round %d: a recorded Return must reach the finalize queue (released=%v)", round, released)
		require.NoError(t, store.MarkPurchaseRevoked(ctx, id, time.Now().UTC(), "direct-api", "", nil, ""))
	}
	t.Logf("release won %d of 30 rounds", releasedRounds)
}

// A new flip can race the release of a stale flag on the same row. A flip that
// succeeds must leave the row flagged with a fresh stamp whichever commits
// first: the release re-checks the stamp's age on the locked row.
func TestReleaseStaleRevocationsInFlight_RacesFlip(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)

	for round := range 30 {
		id := fmt.Sprintf("rr-1523-race-flip-%d", round)
		seedRevocationRow(ctx, t, store, id)
		require.NoError(t, store.FlipPurchaseRevocationInFlight(ctx, id))
		backdateFlip(ctx, t, store, id, testStaleAfter+time.Minute)

		var wg sync.WaitGroup
		var flipErr, releaseErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; flipErr = store.FlipPurchaseRevocationInFlight(ctx, id) }()
		go func() {
			defer wg.Done()
			<-start
			_, releaseErr = store.ReleaseStaleRevocationsInFlight(ctx, testStaleAfter)
		}()
		close(start)
		wg.Wait()
		require.NoError(t, releaseErr)
		if flipErr != nil {
			continue // a flip that refused the row (an in-flight claim) posts no Return
		}
		st := readRevocationState(ctx, t, store, id)
		assert.True(t, st.inFlight, "round %d: a successful flip must not be released", round)
		require.NotNil(t, st.inFlightAt, "round %d", round)
		assert.WithinDuration(t, time.Now(), *st.inFlightAt, time.Minute, "round %d: the flip's stamp is fresh", round)
	}
}
