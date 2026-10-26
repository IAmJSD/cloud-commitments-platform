package config

// Store side of the RI Marketplace status poller (issue #27). The poller reads
// each listing from AWS and records what it finds with the two status-guarded
// UPDATEs below. The guards, not the poller's copy of the row, decide whether
// a write applies, so two poller runs, or a poll racing the cancel handler or a
// relist, never record proceeds twice, revive a canceled listing, or write to a
// listing the row no longer holds.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ListMarketplaceListingsToPoll returns the AWS purchase_history rows whose
// listing the poller must read: every active listing; every listing recorded
// as pending, i.e. one AWS gave no status for when it was created, or whose
// rollback cancel failed (issue #1494); and closed or canceled listings the
// poller has seen (listed_at set) whose proceeds are not recorded yet. A
// pending row that holds a list attempt's token is a claim in flight, whose
// listing_id is the RI's previous listing, so it is left out. The last set
// covers a cancel recorded by the cancel handler before the poller saw the
// listing end, and a terminal listing whose sale price could not be
// determined on an earlier poll. Only the fields the poller uses are
// populated: AccountID, PurchaseID, Provider, Region, CloudAccountID,
// ListingID and ListingState. One row is returned per (purchase_id,
// listing_id), the earliest, so legacy duplicate rows cost one AWS call.
func (s *PostgresStore) ListMarketplaceListingsToPoll(ctx context.Context) ([]PurchaseHistoryRecord, error) {
	query := `
		SELECT DISTINCT ON (purchase_id, listing_id)
		       account_id, purchase_id, provider, region, cloud_account_id::text,
		       listing_id, listing_state
		FROM purchase_history
		WHERE provider = 'aws'
		  AND COALESCE(listing_id, '') <> ''
		  AND (lower(COALESCE(listing_state, '')) = $1
		       OR (lower(COALESCE(listing_state, '')) = $4
		           AND listing_client_token IS NULL)
		       OR (lower(COALESCE(listing_state, '')) IN ($2, $3)
		           AND listed_at IS NOT NULL
		           AND listing_proceeds_received IS NULL))
		ORDER BY purchase_id, listing_id, created_at, id
	`
	rows, err := s.db.Query(ctx, query, ListingStateActive, ListingStateClosed, ListingStateCancelled, ListingStatePending)
	if err != nil {
		return nil, fmt.Errorf("failed to list marketplace listings to poll: %w", err)
	}
	defer rows.Close()

	var out []PurchaseHistoryRecord
	for rows.Next() {
		var r PurchaseHistoryRecord
		var cloudAccountID *string
		if err := rows.Scan(&r.AccountID, &r.PurchaseID, &r.Provider, &r.Region, &cloudAccountID,
			&r.ListingID, &r.ListingState); err != nil {
			return nil, fmt.Errorf("failed to scan marketplace listing to poll: %w", err)
		}
		r.CloudAccountID = cloudAccountID
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to list marketplace listings to poll: %w", err)
	}
	return out, nil
}

// RecordMarketplaceListingActive records that AWS reports the row's listing
// active. On a row that holds listingID as active it stamps listed_at and never
// writes listing_state, so a cancel or relist recorded after the poller read
// AWS is kept. On a row that holds listingID as pending with no list attempt in
// flight (no client token) it also moves the state to active (issue #1494); a
// pending row with a token is a claim whose listing_id is the RI's previous
// listing and is left alone. A zero listedAt (AWS reported no CreateDate)
// keeps the stored listed_at. Any settlement still on the row belongs to an
// earlier listing of the same RI and is cleared. It reports whether the row
// changed.
func (s *PostgresStore) RecordMarketplaceListingActive(ctx context.Context, purchaseID, listingID string, listedAt time.Time) (bool, error) {
	var at *time.Time
	if !listedAt.IsZero() {
		utc := listedAt.UTC()
		at = &utc
	}
	query := `
		UPDATE purchase_history
		SET listing_state = CASE WHEN lower(COALESCE(listing_state, '')) = $5 THEN $4 ELSE listing_state END,
		    listed_at = COALESCE($3::timestamptz, listed_at),
		    listing_proceeds_received = NULL, listing_fee_paid = NULL
		WHERE purchase_id = $1 AND listing_id = $2
		  AND ((lower(COALESCE(listing_state, '')) = $5 AND listing_client_token IS NULL)
		       OR (lower(COALESCE(listing_state, '')) = $4
		           AND (listed_at IS DISTINCT FROM COALESCE($3::timestamptz, listed_at)
		                OR listing_proceeds_received IS NOT NULL
		                OR listing_fee_paid IS NOT NULL)))
	`
	tag, err := s.db.Exec(ctx, query, purchaseID, listingID, at, ListingStateActive, ListingStatePending)
	if err != nil {
		return false, fmt.Errorf("failed to record active marketplace listing %s for purchase %s: %w", listingID, purchaseID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// SettleMarketplaceListing records a listing that ended on AWS: its terminal
// state, listed_at and, when known, the proceeds and AWS fee. It applies only
// while the row still holds the listing, has no proceeds recorded, and is
// active, pending with no list attempt in flight (issue #1494), or already in
// the same terminal state. So proceeds are recorded once
// however many polls race, a recorded cancel is never turned into a sale (or
// the reverse), and a row that was relisted is left alone. The list paths
// (UpdatePurchaseHistoryListing, ResolveMarketplaceListingAttempt) clear the
// previous listing's settlement when they write a new listing id, so a relist
// settles here even if it ends before any poll sees it active. It reports
// whether the row was written.
func (s *PostgresStore) SettleMarketplaceListing(ctx context.Context, st MarketplaceListingSettlement) (bool, error) {
	state := strings.ToLower(st.State)
	if state != ListingStateClosed && state != ListingStateCancelled {
		return false, fmt.Errorf("cannot settle marketplace listing %s in state %q", st.ListingID, st.State)
	}
	if (st.ProceedsReceived == nil) != (st.FeePaid == nil) {
		return false, fmt.Errorf("marketplace listing %s: proceeds and fee must be recorded together", st.ListingID)
	}
	var listedAt *time.Time
	if st.ListedAt != nil {
		utc := st.ListedAt.UTC()
		listedAt = &utc
	}
	query := `
		UPDATE purchase_history
		SET listing_state = $3,
		    listed_at = COALESCE($4::timestamptz, listed_at),
		    listing_proceeds_received = $5,
		    listing_fee_paid = $6
		WHERE purchase_id = $1 AND listing_id = $2
		  AND listing_proceeds_received IS NULL
		  AND (lower(COALESCE(listing_state, '')) IN ($7, $3)
		       OR (lower(COALESCE(listing_state, '')) = $8 AND listing_client_token IS NULL))
	`
	tag, err := s.db.Exec(ctx, query, st.PurchaseID, st.ListingID, state, listedAt,
		st.ProceedsReceived, st.FeePaid, ListingStateActive, ListingStatePending)
	if err != nil {
		return false, fmt.Errorf("failed to settle marketplace listing %s for purchase %s: %w", st.ListingID, st.PurchaseID, err)
	}
	return tag.RowsAffected() > 0, nil
}
