package config

// Store side of the RI Marketplace status poller (issue #27). The poller reads
// each listing from AWS and records what it finds with the status-guarded
// writes below. The guards, not the poller's copy of the row, decide whether
// a write applies, so two poller runs, or a poll racing the cancel handler or a
// relist, never record proceeds twice, revive a canceled listing, or write to a
// listing the row no longer holds.
//
// Every listing's settlement is also kept in its own marketplace_listing_settlements
// row (migration 000153, issue #1494). purchase_history only describes an RI's
// latest listing, so a listing the RI was relisted from is polled and settled
// from that table instead of being lost.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// marketplaceListingsToPollSQL selects the listings the poller must read, one
// row per (purchase_id, listing_id). $1..$4 are ListingStateActive,
// ListingStateClosed, ListingStateCancelled and ListingStatePending.
//
// The first branch is the RI's current listing on purchase_history. The
// second is a listing the RI has since moved on from (marketplace_listing_settlements,
// issue #1494), selected by the same rules; its account, region and cloud
// account come from the purchase_history row it was made from. A listing a
// purchase_history row holds as a claim in flight (pending with a token) is
// left out of both branches until the claim resolves or is released, as
// #1508 leaves such a row alone. When both branches name a listing the
// purchase_history one is returned (src 0), so the poller sees the row's live
// state.
const marketplaceListingsToPollSQL = `
	SELECT DISTINCT ON (purchase_id, listing_id)
	       account_id, purchase_id, provider, region, cloud_account_id::text,
	       listing_id, listing_state
	FROM (
		SELECT 0 AS src, created_at, id, account_id, purchase_id, provider, region,
		       cloud_account_id, listing_id, listing_state
		FROM purchase_history
		WHERE provider = 'aws'
		  AND COALESCE(listing_id, '') <> ''
		  AND (lower(COALESCE(listing_state, '')) = $1
		       OR (lower(COALESCE(listing_state, '')) = $4
		           AND listing_client_token IS NULL)
		       OR (lower(COALESCE(listing_state, '')) IN ($2, $3)
		           AND listed_at IS NOT NULL
		           AND listing_proceeds_received IS NULL))
		UNION ALL
		SELECT 1, p.created_at, p.id, p.account_id, s.purchase_id, p.provider, p.region,
		       p.cloud_account_id, s.listing_id, s.status
		FROM marketplace_listing_settlements s
		JOIN purchase_history p ON p.id = s.purchase_history_id
		WHERE p.provider = 'aws'
		  AND s.proceeds_received IS NULL
		  AND (lower(s.status) IN ($1, $4)
		       OR (lower(s.status) IN ($2, $3) AND s.listed_at IS NOT NULL))
		  AND NOT EXISTS (
			SELECT 1 FROM purchase_history c
			WHERE c.purchase_id = s.purchase_id AND c.listing_id = s.listing_id
			  AND lower(COALESCE(c.listing_state, '')) = $4
			  AND c.listing_client_token IS NOT NULL)
	) due
	ORDER BY purchase_id, listing_id, src, created_at, id
`

// ListMarketplaceListingsToPoll returns the AWS listings the poller must read:
// every active listing; every listing recorded as pending, i.e. one AWS gave
// no status for when it was created, or whose rollback cancel failed (issue
// #1494); and closed or canceled listings the poller has seen (listed_at set)
// whose proceeds are not recorded yet. It reads both the RI's current listing
// on purchase_history and the listings the RI was relisted from, which
// marketplace_listing_settlements keeps (issue #1494). A pending row that
// holds a list attempt's token is a claim in flight, whose listing_id is the
// RI's previous listing; that listing is not polled until the claim resolves
// (it is then polled from the settlement table) or is released. The
// terminal set covers a cancel recorded by the cancel handler before the
// poller saw the listing end, and a terminal listing whose sale price could
// not be determined on an earlier poll. Only the fields the poller uses are
// populated: AccountID, PurchaseID, Provider, Region, CloudAccountID,
// ListingID and ListingState. One row is returned per (purchase_id,
// listing_id), the earliest, so legacy duplicate rows cost one AWS call.
func (s *PostgresStore) ListMarketplaceListingsToPoll(ctx context.Context) ([]PurchaseHistoryRecord, error) {
	rows, err := s.db.Query(ctx, marketplaceListingsToPollSQL, ListingStateActive, ListingStateClosed, ListingStateCancelled, ListingStatePending)
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

// settleCurrentListingSQL records a terminal listing on the purchase_history
// row(s) that still hold it. It applies only while the row has no proceeds
// recorded and is active, pending with no list attempt in flight (issue
// #1494), or already in the same terminal state. Arguments are those of
// settleListingArgs.
const settleCurrentListingSQL = `
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

// upsertListingSettlementSQL records the same settlement in the listing's own
// row, creating it from the purchase_history row settleCurrentListingSQL just
// wrote (the earliest, for legacy duplicates). An existing row is only filled
// while its amounts are unset and its status is active, pending or the same
// terminal state, and only for the same RI, so a listing's proceeds are
// recorded once.
const upsertListingSettlementSQL = `
	INSERT INTO marketplace_listing_settlements AS s (
	    listing_id, purchase_history_id, purchase_id, status, listed_at,
	    proceeds_received, fee_paid, settled_at)
	SELECT p.listing_id, p.id, p.purchase_id, $3, COALESCE($4::timestamptz, p.listed_at),
	       $5, $6, CASE WHEN $5::numeric IS NOT NULL THEN now() END
	FROM purchase_history p
	WHERE p.purchase_id = $1 AND p.listing_id = $2
	ORDER BY p.created_at, p.id
	LIMIT 1
	ON CONFLICT (listing_id) DO UPDATE
	SET status = EXCLUDED.status,
	    listed_at = COALESCE(EXCLUDED.listed_at, s.listed_at),
	    proceeds_received = EXCLUDED.proceeds_received,
	    fee_paid = EXCLUDED.fee_paid,
	    settled_at = EXCLUDED.settled_at,
	    updated_at = now()
	WHERE s.purchase_id = EXCLUDED.purchase_id
	  AND s.proceeds_received IS NULL
	  AND lower(s.status) IN ($7, $8, $3)
`

// listingHeldByClaimSQL share-locks the purchase_history rows that hold the
// listing and reports whether any of them is a list claim in flight (pending
// with a token). The lock makes a claim that starts now wait for this
// transaction, so no settlement is written while a row is mid-claim.
const listingHeldByClaimSQL = `
	SELECT COALESCE(bool_or(lower(COALESCE(listing_state, '')) = $3
	                        AND listing_client_token IS NOT NULL), false)
	FROM (SELECT listing_state, listing_client_token
	      FROM purchase_history
	      WHERE purchase_id = $1 AND listing_id = $2
	      FOR SHARE) held
`

// settleSupersededListingSQL records the settlement of a listing the RI no
// longer holds on purchase_history, in the row the relist kept for it, under
// the same guards as upsertListingSettlementSQL.
const settleSupersededListingSQL = `
	UPDATE marketplace_listing_settlements AS s
	SET status = $3,
	    listed_at = COALESCE($4::timestamptz, s.listed_at),
	    proceeds_received = $5,
	    fee_paid = $6,
	    settled_at = CASE WHEN $5::numeric IS NOT NULL THEN now() END,
	    updated_at = now()
	WHERE s.listing_id = $2 AND s.purchase_id = $1
	  AND s.proceeds_received IS NULL
	  AND lower(s.status) IN ($7, $8, $3)
`

// SettleMarketplaceListing records a listing that ended on AWS: its terminal
// state, listed_at and, when known, the proceeds and AWS fee. In one
// transaction it writes the purchase_history row while the row still holds the
// listing (the RI's latest listing settlement, as before) and the listing's
// marketplace_listing_settlements row (issue #1494), which is what per-RI
// totals read. Each write applies only while that row has no proceeds
// recorded and is active, pending with no list attempt in flight, or already
// in the same terminal state, and never while a purchase_history row holds the
// listing as a list claim in flight. So proceeds are recorded once however
// many polls race, a recorded cancel is never turned into a sale (or the
// reverse), a mid-claim row is left alone (#1508), and a listing the RI was
// relisted from is still settled, in its own row. The
// purchase_history row is written first, so a poll and a relist of the same
// RI take their locks in the same order. It reports whether either row was
// written.
func (s *PostgresStore) SettleMarketplaceListing(ctx context.Context, st MarketplaceListingSettlement) (bool, error) {
	args, err := settleListingArgs(st)
	if err != nil {
		return false, err
	}
	var applied bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		applied, txErr = settleMarketplaceListingTx(ctx, tx, args)
		return txErr
	})
	if err != nil {
		return false, fmt.Errorf("failed to settle marketplace listing %s for purchase %s: %w", st.ListingID, st.PurchaseID, err)
	}
	return applied, nil
}

// settleListingArgs validates st and returns the arguments the settle
// statements share: purchase id, listing id, lowercase terminal state,
// listed_at (UTC, nil to keep), proceeds, fee, then ListingStateActive and
// ListingStatePending.
func settleListingArgs(st MarketplaceListingSettlement) ([]any, error) {
	state := strings.ToLower(st.State)
	if state != ListingStateClosed && state != ListingStateCancelled {
		return nil, fmt.Errorf("cannot settle marketplace listing %s in state %q", st.ListingID, st.State)
	}
	if (st.ProceedsReceived == nil) != (st.FeePaid == nil) {
		return nil, fmt.Errorf("marketplace listing %s: proceeds and fee must be recorded together", st.ListingID)
	}
	var listedAt *time.Time
	if st.ListedAt != nil {
		utc := st.ListedAt.UTC()
		listedAt = &utc
	}
	return []any{st.PurchaseID, st.ListingID, state, listedAt, st.ProceedsReceived, st.FeePaid,
		ListingStateActive, ListingStatePending}, nil
}

// settleMarketplaceListingTx runs SettleMarketplaceListing's writes in tx. A
// listing the purchase_history row still holds gets its settlement row
// created or filled; one the row no longer holds can only be filled in the
// settlement row its relist kept. Neither is written while a purchase_history
// row holds the listing as a claim in flight (#1508): the listing is settled
// once the claim resolves or is released. purchase_history is locked before
// the settlement table, the order a relist and its trigger take them in.
func settleMarketplaceListingTx(ctx context.Context, tx pgx.Tx, args []any) (bool, error) {
	tag, err := tx.Exec(ctx, settleCurrentListingSQL, args...)
	if err != nil {
		return false, err
	}
	rowApplied := tag.RowsAffected() > 0
	midClaim, err := listingHeldByClaim(ctx, tx, args[0], args[1])
	if err != nil {
		return false, err
	}
	if midClaim {
		return rowApplied, nil
	}
	listingSQL := settleSupersededListingSQL
	if rowApplied {
		listingSQL = upsertListingSettlementSQL
	}
	tag, err = tx.Exec(ctx, listingSQL, args...)
	if err != nil {
		return false, err
	}
	return rowApplied || tag.RowsAffected() > 0, nil
}

// listingHeldByClaim runs listingHeldByClaimSQL in tx for the purchase and
// listing ids.
func listingHeldByClaim(ctx context.Context, tx pgx.Tx, purchaseID, listingID any) (bool, error) {
	var midClaim bool
	err := tx.QueryRow(ctx, listingHeldByClaimSQL, purchaseID, listingID, ListingStatePending).Scan(&midClaim)
	return midClaim, err
}

// GetMarketplaceListingProceedsTotals returns, for each of purchaseIDs that
// has any marketplace listing recorded, the net proceeds and AWS fees summed
// over all of its settled listings, from marketplace_listing_settlements only
// (issue #1494). Each listing has one row there, so a sale is counted once
// however often the RI was relisted; purchase_history's settlement columns
// (the latest listing only) are never added in. One total is returned per
// purchase and currency, ordered by purchase id then currency.
func (s *PostgresStore) GetMarketplaceListingProceedsTotals(ctx context.Context, purchaseIDs []string) ([]MarketplaceListingProceedsTotal, error) {
	if len(purchaseIDs) == 0 {
		return nil, nil
	}
	query := `
		SELECT purchase_id, currency,
		       COALESCE(SUM(proceeds_received), 0)::float8,
		       COALESCE(SUM(fee_paid), 0)::float8,
		       count(proceeds_received),
		       count(*) - count(proceeds_received)
		FROM marketplace_listing_settlements
		WHERE purchase_id = ANY($1)
		GROUP BY purchase_id, currency
		ORDER BY purchase_id, currency
	`
	rows, err := s.db.Query(ctx, query, purchaseIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to read marketplace listing proceeds totals: %w", err)
	}
	defer rows.Close()

	var out []MarketplaceListingProceedsTotal
	for rows.Next() {
		var t MarketplaceListingProceedsTotal
		if err := rows.Scan(&t.PurchaseID, &t.Currency, &t.ProceedsReceived, &t.FeePaid,
			&t.SettledListings, &t.UnsettledListings); err != nil {
			return nil, fmt.Errorf("failed to scan marketplace listing proceeds total: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read marketplace listing proceeds totals: %w", err)
	}
	return out, nil
}
