//go:build integration
// +build integration

package config

// Real-DB tests for the cancel endpoint's recovery writes (issue #525): the
// guard that keeps a canceled listing from being recorded live again, the
// fenced ResolveMarketplaceListingAttempt, and the race between a list
// request recording its listing and a cancel recording that listing canceled.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarketplaceListingAttempt_PostgresSemantics(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath())
	store := NewPostgresStore(container.DB)
	pool := container.DB.Pool()

	t.Run("ended listing is never recorded live again", func(t *testing.T) { checkListingEndedGuard(ctx, t, store) })
	t.Run("attempt read", func(t *testing.T) { checkGetMarketplaceListingAttempt(ctx, t, store) })
	t.Run("resolve is fenced", func(t *testing.T) { checkResolveIsFenced(ctx, t, store) })
	t.Run("cancel committed first, list record waits", func(t *testing.T) { checkLateListRecordLoses(ctx, t, store, pool) })
	t.Run("list record committed first, cancel waits", func(t *testing.T) { checkLateCancelStillWins(ctx, t, store, pool) })
	t.Run("free-running race", func(t *testing.T) { checkCancelListRace(ctx, t, store) })
}

func listingOf(ctx context.Context, t *testing.T, store *PostgresStore, id string) MarketplaceListingAttempt {
	t.Helper()
	a, err := store.GetMarketplaceListingAttempt(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, a)
	return *a
}

// claimedRow seeds a row and claims it with token, as a list request does
// before its AWS create.
func claimedRow(ctx context.Context, t *testing.T, store *PostgresStore, id, token string) {
	t.Helper()
	seedClaimRow(ctx, t, store, id)
	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, token, []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim)
}

func checkListingEndedGuard(ctx context.Context, t *testing.T, store *PostgresStore) {
	const id = "ri-ended-525"
	seedClaimRow(ctx, t, store, id)
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-1", ListingStateActive))
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-1", ListingStateCancelled))

	for _, live := range []string{ListingStateActive, ListingStatePending, "ACTIVE"} {
		err := store.UpdatePurchaseHistoryListing(ctx, id, "ril-1", live)
		require.ErrorIs(t, err, ErrMarketplaceListingEnded, live)
	}
	got := listingOf(ctx, t, store, id)
	assert.Equal(t, ListingStateCancelled, got.State, "the canceled listing must stay canceled")

	// Ended to ended is allowed (a sale recorded after a cancel attempt), and
	// so is a new listing.
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-1", ListingStateClosed))
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-2", ListingStateActive))
	assert.Equal(t, ListingStateActive, listingOf(ctx, t, store, id).State)

	err := store.UpdatePurchaseHistoryListing(ctx, "no-such-ri-525", "ril-1", ListingStateActive)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrMarketplaceListingEnded, "a missing row is not an ended listing")
}

func checkGetMarketplaceListingAttempt(ctx context.Context, t *testing.T, store *PostgresStore) {
	const id = "ri-read-525"
	claimedRow(ctx, t, store, id, "tok-read")
	assert.Equal(t, MarketplaceListingAttempt{State: ListingStatePending, ClientToken: "tok-read"}, listingOf(ctx, t, store, id))

	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-r", ListingStateActive))
	assert.Equal(t, MarketplaceListingAttempt{ListingID: "ril-r", State: ListingStateActive}, listingOf(ctx, t, store, id))

	missing, err := store.GetMarketplaceListingAttempt(ctx, "no-such-ri-525")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func checkResolveIsFenced(ctx context.Context, t *testing.T, store *PostgresStore) {
	const id = "ri-resolve-525"
	claimedRow(ctx, t, store, id, "tok-r")
	seen := listingOf(ctx, t, store, id)

	_, err := store.ResolveMarketplaceListingAttempt(ctx, id, seen, "ril-x", ListingStateActive)
	require.Error(t, err, "resolve never records a live state")

	stale := seen
	stale.ClientToken = "tok-other"
	applied, err := store.ResolveMarketplaceListingAttempt(ctx, id, stale, "ril-x", ListingStateCancelled)
	require.NoError(t, err)
	assert.False(t, applied, "a stale read must not write")
	assert.Equal(t, seen, listingOf(ctx, t, store, id))

	applied, err = store.ResolveMarketplaceListingAttempt(ctx, id, seen, "ril-x", ListingStateCancelled)
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Equal(t, MarketplaceListingAttempt{ListingID: "ril-x", State: ListingStateCancelled}, listingOf(ctx, t, store, id))
	claim, err := store.ClaimMarketplaceListingSlot(ctx, id, "tok-next", []byte(claimTestSchedule))
	require.NoError(t, err)
	require.NotNil(t, claim)
	assert.False(t, claim.Resumed, "the resolved attempt's token is gone")

	// Stale read, but the row holds the listing the caller canceled: written.
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-y", ListingStateActive))
	applied, err = store.ResolveMarketplaceListingAttempt(ctx, id, stale, "ril-y", ListingStateCancelled)
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Equal(t, ListingStateCancelled, listingOf(ctx, t, store, id).State)

	// Stale read and another listing on the row: not written.
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, id, "ril-z", ListingStateActive))
	applied, err = store.ResolveMarketplaceListingAttempt(ctx, id, stale, "ril-y", ListingStateCancelled)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, MarketplaceListingAttempt{ListingID: "ril-z", State: ListingStateActive}, listingOf(ctx, t, store, id))

	// A pending row with neither token nor listing is cleared to no state.
	const legacy = "ri-legacy-525"
	seedClaimRow(ctx, t, store, legacy)
	require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, legacy, "", ListingStatePending))
	applied, err = store.ResolveMarketplaceListingAttempt(ctx, legacy, listingOf(ctx, t, store, legacy), "", "")
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Equal(t, MarketplaceListingAttempt{}, listingOf(ctx, t, store, legacy))
}

// attemptCancelWriteSQL is the row change the cancel endpoint makes when it
// records the attempt's listing canceled, run inside a held transaction.
const attemptCancelWriteSQL = `UPDATE purchase_history SET listing_id = $2, listing_state = $3,
	listing_client_token = NULL, listing_price_schedule = NULL WHERE purchase_id = $1`

// attemptListWriteSQL is the row change of the list request recording its
// listing.
const attemptListWriteSQL = `UPDATE purchase_history SET listing_id = $2, listing_state = $3,
	listing_client_token = NULL, listing_price_schedule = NULL WHERE purchase_id = $1`

// The cancel's write commits while the list request's record is queued on
// the row lock. Postgres re-checks the record's guard on the committed row,
// so the record is refused and the listing stays canceled.
func checkLateListRecordLoses(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "ri-late-list-525"
	claimedRow(ctx, t, store, id, "tok-L")
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, attemptCancelWriteSQL, id, "ril-L", ListingStateCancelled)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- store.UpdatePurchaseHistoryListing(ctx, id, "ril-L", ListingStateActive) }()
	testhelpers.WaitForLockWait(ctx, t, pool, "%UPDATE purchase_history%listing_id = $1%")
	require.NoError(t, tx.Commit(ctx))

	require.ErrorIs(t, testhelpers.Await(t, done), ErrMarketplaceListingEnded)
	assert.Equal(t, MarketplaceListingAttempt{ListingID: "ril-L", State: ListingStateCancelled}, listingOf(ctx, t, store, id))
}

// The list request's record commits while the cancel's fenced write is queued.
// The row no longer matches what the cancel read, but it holds the listing the
// cancel canceled on AWS, so the cancel's write applies.
func checkLateCancelStillWins(ctx context.Context, t *testing.T, store *PostgresStore, pool *pgxpool.Pool) {
	const id = "ri-late-cancel-525"
	claimedRow(ctx, t, store, id, "tok-C")
	seen := listingOf(ctx, t, store, id)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, attemptListWriteSQL, id, "ril-C", ListingStateActive)
	require.NoError(t, err)

	type result struct {
		applied bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		applied, resolveErr := store.ResolveMarketplaceListingAttempt(ctx, id, seen, "ril-C", ListingStateCancelled)
		done <- result{applied, resolveErr}
	}()
	testhelpers.WaitForLockWait(ctx, t, pool, "%UPDATE purchase_history%NULLIF%")
	require.NoError(t, tx.Commit(ctx))

	got := testhelpers.Await(t, done)
	require.NoError(t, got.err)
	assert.True(t, got.applied)
	assert.Equal(t, MarketplaceListingAttempt{ListingID: "ril-C", State: ListingStateCancelled}, listingOf(ctx, t, store, id))
}

// Both writes fire at once on many rows. Whatever the order, the listing ends
// recorded as canceled, and the list request learns it lost exactly when it
// ran second.
func checkCancelListRace(ctx context.Context, t *testing.T, store *PostgresStore) {
	for i := range 25 {
		id := fmt.Sprintf("ri-race-cancel-525-%d", i)
		listing := fmt.Sprintf("ril-race-%d", i)
		claimedRow(ctx, t, store, id, "tok-race")
		seen := listingOf(ctx, t, store, id)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var listErr, resolveErr error
		var applied bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			listErr = store.UpdatePurchaseHistoryListing(ctx, id, listing, ListingStateActive)
		}()
		go func() {
			defer wg.Done()
			<-start
			applied, resolveErr = store.ResolveMarketplaceListingAttempt(ctx, id, seen, listing, ListingStateCancelled)
		}()
		close(start)
		wg.Wait()

		require.NoError(t, resolveErr)
		assert.True(t, applied, "row %s: the cancel's write applies in either order", id)
		if listErr != nil {
			require.True(t, errors.Is(listErr, ErrMarketplaceListingEnded), "row %s: %v", id, listErr)
		}
		assert.Equal(t, MarketplaceListingAttempt{ListingID: listing, State: ListingStateCancelled}, listingOf(ctx, t, store, id), "row %s", id)
	}
}
