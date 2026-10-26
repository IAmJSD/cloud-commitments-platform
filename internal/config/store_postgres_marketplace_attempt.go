package config

// Store side of recovering a marketplace listing attempt from the cancel
// endpoint (issue #525). A row left pending by a crashed or concurrent
// marketplace-list request is resolved against what the cancel handler read,
// so a cancel never overwrites a listing or attempt it did not see.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrMarketplaceListingEnded is returned by UpdatePurchaseHistoryListing when
// the row already records the same listing as canceled or closed and the write
// would mark it live again. AWS never revives an ended listing, so such a
// write comes from a request that raced the cancel and lost.
var ErrMarketplaceListingEnded = errors.New("the marketplace listing is already recorded as ended")

// MarketplaceListingAttempt is the listing state of a purchase_history row as
// the cancel handler read it: the recorded listing, its state, and the client
// token of an unresolved list attempt (empty when none).
type MarketplaceListingAttempt struct {
	ListingID   string
	State       string
	ClientToken string //nolint:gosec // G117: an idempotency token for AWS, not a credential
}

// GetMarketplaceListingAttempt returns the listing id, state and unresolved
// attempt token of the row for purchaseID, or (nil, nil) when there is none.
// With duplicate rows for one purchase_id it reads the earliest.
func (s *PostgresStore) GetMarketplaceListingAttempt(ctx context.Context, purchaseID string) (*MarketplaceListingAttempt, error) {
	query := `
		SELECT COALESCE(listing_id, ''), COALESCE(listing_state, ''), COALESCE(listing_client_token, '')
		FROM purchase_history
		WHERE purchase_id = $1
		ORDER BY created_at, id
		LIMIT 1
	`
	var a MarketplaceListingAttempt
	err := s.db.QueryRow(ctx, query, purchaseID).Scan(&a.ListingID, &a.State, &a.ClientToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read marketplace listing attempt for purchase %s: %w", purchaseID, err)
	}
	return &a, nil
}

// ResolveMarketplaceListingAttempt records listingID in state on the row and
// drops any unresolved attempt token, but only while the row still matches
// seen, or already holds listingID (another request recorded the listing the
// caller just canceled). state must be an ended state (canceled or closed) or
// empty, which clears a stuck pending row that has no listing; an empty
// listingID clears the listing id too. It reports whether the row was written.
func (s *PostgresStore) ResolveMarketplaceListingAttempt(ctx context.Context, purchaseID string, seen MarketplaceListingAttempt, listingID, state string) (bool, error) {
	if state != "" && !isEndedListingState(state) {
		return false, fmt.Errorf("cannot resolve marketplace listing %s for purchase %s to live state %q", listingID, purchaseID, state)
	}
	query := `
		UPDATE purchase_history
		SET listing_id = NULLIF($2, ''), listing_state = NULLIF($3, ''),
		    listing_client_token = NULL, listing_price_schedule = NULL
		WHERE purchase_id = $1
		  AND ((lower(COALESCE(listing_state, '')) = lower($4)
		        AND COALESCE(listing_id, '') = $5
		        AND COALESCE(listing_client_token, '') = $6)
		       OR ($2 <> '' AND listing_id = $2))
	`
	tag, err := s.db.Exec(ctx, query, purchaseID, listingID, state, seen.State, seen.ListingID, seen.ClientToken)
	if err != nil {
		return false, fmt.Errorf("failed to resolve marketplace listing attempt for purchase %s: %w", purchaseID, err)
	}
	return tag.RowsAffected() > 0, nil
}

func isEndedListingState(state string) bool {
	return strings.EqualFold(state, ListingStateCancelled) || strings.EqualFold(state, ListingStateClosed)
}

// listingEndedOrMissing tells apart the two reasons a guarded
// UpdatePurchaseHistoryListing wrote nothing: the row records the listing as
// ended (ErrMarketplaceListingEnded), or there is no row.
func (s *PostgresStore) listingEndedOrMissing(ctx context.Context, purchaseID string) error {
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM purchase_history WHERE purchase_id = $1)`, purchaseID).Scan(&exists); err != nil {
		return fmt.Errorf("failed to update listing state for purchase %s: %w", purchaseID, err)
	}
	if exists {
		return fmt.Errorf("purchase %s: %w", purchaseID, ErrMarketplaceListingEnded)
	}
	return fmt.Errorf("purchase %s not found in history", purchaseID)
}
