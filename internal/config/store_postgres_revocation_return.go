package config

import (
	"context"
	"fmt"
	"time"
)

// MarkRevocationReturnPosted records that the provider accepted the Return for
// purchaseID (issue #1523, migration 000148). The revoke handler calls it right
// after Azure accepts the Return and before MarkPurchaseRevoked, so the
// finalize_revocations sweep can tell "refunded, audit write failed" (finalize)
// from "flagged, but no Return ever went out" (never finalize).
//
// The first stamp stands (COALESCE), so a retry does not move it. A revoked
// row is left alone and returns nil: there is nothing left to finalize. A
// missing row returns a not-found error.
func (s *PostgresStore) MarkRevocationReturnPosted(ctx context.Context, purchaseID string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE purchase_history
		   SET return_posted_at = COALESCE(return_posted_at, now())
		 WHERE purchase_id = $1
		   AND revoked_at IS NULL
	`, purchaseID)
	if err != nil {
		return fmt.Errorf("MarkRevocationReturnPosted: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM purchase_history WHERE purchase_id = $1)`, purchaseID).Scan(&exists); err != nil {
		return fmt.Errorf("MarkRevocationReturnPosted existence check: %w", err)
	}
	if !exists {
		return fmt.Errorf("MarkRevocationReturnPosted: purchase_id %q not found", purchaseID)
	}
	return nil
}

// ReleaseStaleRevocationsInFlight clears revocation_in_flight on unrevoked
// rows that have been flagged for longer than after with no Return recorded
// (issue #1523), and returns their purchase IDs. Such a row is one whose
// request died between the flip and the Return, or whose Return was never
// recorded as accepted. Clearing the flag lets the user revoke it again; if
// Azure did take a Return that went unrecorded, Azure refuses the second one.
//
// A flagged row with no revocation_in_flight_at (flagged before migration 000148,
// or by a binary that predates it) is stamped now and released once after has
// passed from then, so its age is never overstated.
//
// Both statements use the database clock. Each UPDATE re-checks its predicate
// on the locked row under READ COMMITTED, so a concurrent
// MarkRevocationReturnPosted or a fresh flip that commits first makes the row
// no longer match; one that commits after the release still records its
// Return, which GetPurchaseHistoryInFlight finds with or without the flag.
func (s *PostgresStore) ReleaseStaleRevocationsInFlight(ctx context.Context, after time.Duration) ([]string, error) {
	if after <= 0 {
		return nil, fmt.Errorf("ReleaseStaleRevocationsInFlight: timeout must be positive, got %s", after)
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE purchase_history
		   SET revocation_in_flight_at = now()
		 WHERE revocation_in_flight
		   AND revoked_at IS NULL
		   AND return_posted_at IS NULL
		   AND revocation_in_flight_at IS NULL
	`); err != nil {
		return nil, fmt.Errorf("ReleaseStaleRevocationsInFlight stamp: %w", err)
	}
	rows, err := s.db.Query(ctx, `
		UPDATE purchase_history
		   SET revocation_in_flight = false, revocation_in_flight_at = NULL
		 WHERE revocation_in_flight
		   AND revoked_at IS NULL
		   AND return_posted_at IS NULL
		   AND revocation_in_flight_at < now() - make_interval(secs => $1)
		RETURNING purchase_id
	`, after.Seconds())
	if err != nil {
		return nil, fmt.Errorf("ReleaseStaleRevocationsInFlight: %w", err)
	}
	defer rows.Close()
	var released []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("ReleaseStaleRevocationsInFlight scan: %w", err)
		}
		released = append(released, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ReleaseStaleRevocationsInFlight: %w", err)
	}
	return released, nil
}
