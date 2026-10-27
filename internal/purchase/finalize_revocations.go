package purchase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
)

// FinalizeResult summarizes one sweep of FinalizeInFlightRevocations.
// Returned for the scheduled-task handler to log and surface in CloudWatch.
type FinalizeResult struct {
	// Found is the number of unrevoked purchase_history rows with a recorded
	// Return (return_posted_at) that the sweep saw.
	Found int
	// Finalized is the number of rows successfully stamped by MarkPurchaseRevoked.
	Finalized int
	// Errored is the number of rows the MarkPurchaseRevoked call failed for.
	Errored int
	// Released is the number of rows whose revocation_in_flight flag was
	// cleared because no Return was recorded within
	// staleRevocationInFlightAfter (issue #1523). They are not revoked.
	Released int
}

// staleRevocationInFlightAfter is how long a row may stay flagged with no
// recorded Return before the sweep clears the flag so the purchase can be
// revoked again (issue #1523). It only has to outlast a revoke still between
// its flip and its recorded Return, which every deployment bounds: the app's
// own HTTP server caps each request at 30s on every container platform
// (handleHTTPRequest in internal/server/http.go), Lambda ends an invocation at
// its function timeout, and past the flip the revoke handler runs the Return
// and the writes recording it detached under their own budgets (3m and 30s,
// issue #1527). 2h is a wide margin over all of these. Correctness does not
// depend on it (issue #1525): a Return recorded after the release is still
// finalized, flag or not, and if the user revokes again while the first
// Return is in flight, Azure refuses a second Return of the same reservation,
// so there is no double refund.
const staleRevocationInFlightAfter = 2 * time.Hour

// finalizeRevocationBackoffs are the sleep durations between consecutive
// MarkPurchaseRevoked retry attempts in FinalizeInFlightRevocations.
var finalizeRevocationBackoffs = []time.Duration{
	2 * time.Second,
	6 * time.Second,
}

// FinalizeInFlightRevocations sweeps unrevoked purchase_history rows whose
// Azure Return was recorded as accepted (return_posted_at) and retries
// MarkPurchaseRevoked for each. These rows represent cases where the Azure
// Return API call succeeded but the subsequent DB write failed — the
// finalize_revocations scheduled tick ensures the audit record is eventually
// consistent without requiring the user to retry (which would be rejected by
// Azure as "already returned").
//
// A row that is only flagged (revocation_in_flight) with no recorded Return is
// never marked revoked (issue #1523): its request may have died before the
// Return was posted, so the purchase may still be active and billed. Once it
// has been flagged for staleRevocationInFlightAfter, the sweep clears the flag
// instead, so the user can revoke it again, and logs it for an operator. A
// failure to release is logged, does not stop the finalize pass, and is
// returned with the result.
//
// The sweep uses a fixed timestamp of time.Now() at the START of the sweep so
// all rows finalized in one pass share the same revokedAt wall-clock value,
// which makes log correlation easier.
//
// Per-row error handling: rows that fail MarkPurchaseRevoked after retries
// are logged and counted in FinalizeResult.Errored but do not abort the
// sweep — the sweep continues to the next row so a single stuck row does not
// block finalization of all in-flight rows.
//
// Cancellation: the backoff between retries honors ctx. When ctx is done the
// row being retried is counted as Errored, the counts so far are logged, and
// the sweep returns them with an error wrapping ctx.Err(), so a Lambda near
// its deadline stops instead of sleeping past it (issue #243).
func (m *Manager) FinalizeInFlightRevocations(ctx context.Context) (*FinalizeResult, error) {
	released, releaseErr := m.releaseStaleRevocations(ctx)

	rows, err := m.config.GetPurchaseHistoryInFlight(ctx)
	if err != nil {
		return nil, errors.Join(releaseErr, err)
	}

	result := &FinalizeResult{Found: len(rows), Released: released}
	now := time.Now().UTC()

	for _, record := range rows {
		markErr, ctxErr := m.markRevokedWithRetry(ctx, record.PurchaseID, now)
		if ctxErr != nil {
			result.Errored++
			logging.Errorf("finalize_revocations: sweep stopped at purchase_id=%s: %v (found=%d finalized=%d errored=%d)",
				record.PurchaseID, ctxErr, result.Found, result.Finalized, result.Errored)
			return result, errors.Join(releaseErr, fmt.Errorf("finalize_revocations: sweep canceled: %w", ctxErr))
		}
		if markErr != nil {
			logging.Errorf("finalize_revocations: MarkPurchaseRevoked for %s failed after %d attempts: %v",
				record.PurchaseID, len(finalizeRevocationBackoffs)+1, markErr)
			result.Errored++
		} else {
			logging.Infof("finalize_revocations: finalized in-flight revocation for purchase_id=%s", record.PurchaseID)
			result.Finalized++
		}
	}

	return result, releaseErr
}

// releaseStaleRevocations clears the flag on rows flagged for longer than
// staleRevocationInFlightAfter with no recorded Return, and logs each one: if
// Azure did take a Return that was never recorded, the row now reads as
// active and an operator has to reconcile it by hand.
func (m *Manager) releaseStaleRevocations(ctx context.Context) (int, error) {
	ids, err := m.config.ReleaseStaleRevocationsInFlight(ctx, staleRevocationInFlightAfter)
	if err != nil {
		logging.Errorf("finalize_revocations: releasing stale in-flight revocations failed: %v", err)
		return 0, fmt.Errorf("finalize_revocations: release stale: %w", err)
	}
	for _, id := range ids {
		logging.Errorf("finalize_revocations: purchase_id=%s was flagged in flight for over %s with no recorded Azure Return; "+
			"cleared the flag without marking it revoked so it can be revoked again. If Azure did accept a Return, reconcile the row by hand",
			id, staleRevocationInFlightAfter)
	}
	return len(ids), nil
}

// markRevokedWithRetry calls MarkPurchaseRevoked, retrying after each
// finalizeRevocationBackoffs delay. It returns the last MarkPurchaseRevoked
// error, or a non-nil ctxErr when ctx was done during a backoff.
func (m *Manager) markRevokedWithRetry(ctx context.Context, purchaseID string, now time.Time) (markErr, ctxErr error) {
	markErr = m.config.MarkPurchaseRevoked(ctx, purchaseID, now, "direct-api", "", nil, "")
	for attempt, backoff := range finalizeRevocationBackoffs {
		if markErr == nil {
			return nil, nil
		}
		logging.Warnf("finalize_revocations: MarkPurchaseRevoked attempt %d for %s failed: %v (retrying in %s)",
			attempt+1, purchaseID, markErr, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return markErr, ctx.Err()
		case <-timer.C:
		}
		markErr = m.config.MarkPurchaseRevoked(ctx, purchaseID, now, "direct-api", "", nil, "")
	}
	return markErr, nil
}
