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
// listing the poller must read: every active listing, plus closed or canceled
// listings the poller has seen (listed_at set) whose proceeds are not recorded
// yet. The second set covers a cancel recorded by the cancel handler before the
// poller saw the listing end, and a terminal listing whose sale price could
// not be determined on an earlier poll. Only the fields the poller uses are
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
		       OR (lower(COALESCE(listing_state, '')) IN ($2, $3)
		           AND listed_at IS NOT NULL
		           AND listing_proceeds_received IS NULL))
		ORDER BY purchase_id, listing_id, created_at, id
	`
	rows, err := s.db.Query(ctx, query, ListingStateActive, ListingStateClosed, ListingStateCancelled)
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

// RecordMarketplaceListingActive stamps listed_at on a row whose listing AWS
// still reports active. It writes only while the row holds listingID in the
// active state, and never writes listing_state, so a cancel or relist recorded
// after the poller read AWS is kept. Any settlement still on the row belongs to
// an earlier listing of the same RI and is cleared. It reports whether the row
// changed.
func (s *PostgresStore) RecordMarketplaceListingActive(ctx context.Context, purchaseID, listingID string, listedAt time.Time) (bool, error) {
	query := `
		UPDATE purchase_history
		SET listed_at = $3, listing_proceeds_received = NULL, listing_fee_paid = NULL
		WHERE purchase_id = $1 AND listing_id = $2
		  AND lower(COALESCE(listing_state, '')) = $4
		  AND (listed_at IS DISTINCT FROM $3
		       OR listing_proceeds_received IS NOT NULL
		       OR listing_fee_paid IS NOT NULL)
	`
	tag, err := s.db.Exec(ctx, query, purchaseID, listingID, listedAt.UTC(), ListingStateActive)
	if err != nil {
		return false, fmt.Errorf("failed to record active marketplace listing %s for purchase %s: %w", listingID, purchaseID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// SettleMarketplaceListing records a listing that ended on AWS: its terminal
// state, listed_at and, when known, the proceeds and AWS fee. It applies only
// while the row still holds the listing, has no proceeds recorded, and is
// active or already in the same terminal state. So proceeds are recorded once
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
		  AND lower(COALESCE(listing_state, '')) IN ($7, $3)
	`
	tag, err := s.db.Exec(ctx, query, st.PurchaseID, st.ListingID, state, listedAt,
		st.ProceedsReceived, st.FeePaid, ListingStateActive)
	if err != nil {
		return false, fmt.Errorf("failed to settle marketplace listing %s for purchase %s: %w", st.ListingID, st.PurchaseID, err)
	}
	return tag.RowsAffected() > 0, nil
}
