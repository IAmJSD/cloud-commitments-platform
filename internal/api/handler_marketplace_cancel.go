package api

// handler_marketplace_cancel.go implements POST
// /api/purchases/{id}/marketplace-cancel. Besides canceling an active listing
// it recovers a row that a marketplace-list attempt left pending (issue #525):
// a crash between the AWS create and the DB write, an AWS listing recorded in
// its own pending state, or a claim from before the attempt token was
// persisted. Every write after an AWS call is fenced on what this request
// read, so it never overwrites a listing or attempt it did not see, and runs
// on a detached context so a disconnecting caller cannot lose it.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

// marketplaceListingChangedMessage is the 409 for a fenced write that found the
// row changed after this request read it.
const marketplaceListingChangedMessage = "the marketplace listing of this RI changed while this request was handling it; reload and retry"

// marketplaceCancel handles POST /api/purchases/{id}/marketplace-cancel.
func (h *Handler) marketplaceCancel(ctx context.Context, req *events.LambdaFunctionURLRequest, purchaseID string) (any, error) {
	session, action, err := h.requireSessionPurchaseAction(ctx, req, auth.ActionSellAny, auth.ActionSellOwn)
	if err != nil {
		return nil, err
	}
	err = validateUUID(purchaseID)
	if err != nil {
		return nil, err
	}
	row, err := h.config.GetPurchaseHistoryByPurchaseID(ctx, purchaseID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up purchase: %w", err)
	}
	if row == nil {
		return nil, NewClientError(404, "purchase not found")
	}
	err = h.authorizeSessionSell(ctx, session, action, row)
	if err != nil {
		return nil, err
	}

	switch {
	case strings.EqualFold(row.ListingState, config.ListingStatePending):
		return h.recoverPendingListing(ctx, purchaseID, row)
	case !strings.EqualFold(row.ListingState, config.ListingStateActive):
		return nil, NewClientError(409, "no active or pending listing found for this RI; current state: "+row.ListingState)
	}
	ec2Client, err := h.marketplaceClientFor(ctx, row)
	if err != nil {
		return nil, err
	}
	return h.cancelListing(ctx, purchaseID, nil, row.ListingID, ec2Client)
}

func (h *Handler) marketplaceClientFor(ctx context.Context, row *config.PurchaseHistoryRecord) (marketplaceEC2Client, error) {
	cfg, err := h.loadMarketplaceAWSConfig(ctx, row)
	if err != nil {
		return nil, err
	}
	return h.buildMarketplaceEC2Client(cfg), nil
}

// recoverPendingListing resolves a pending row. It reads the row's attempt
// token, which the purchase lookup does not return, and fences its writes on
// what it read.
func (h *Handler) recoverPendingListing(ctx context.Context, purchaseID string, row *config.PurchaseHistoryRecord) (any, error) {
	seen, err := h.config.GetMarketplaceListingAttempt(ctx, purchaseID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up the marketplace listing attempt: %w", err)
	}
	if seen == nil {
		return nil, NewClientError(404, "purchase not found")
	}
	if !strings.EqualFold(seen.State, config.ListingStatePending) {
		return nil, NewClientError(409, marketplaceListingChangedMessage)
	}
	if seen.ClientToken == "" && seen.ListingID == "" {
		// A claim from before the attempt token was persisted (migration
		// 000105) that never finished: nothing on AWS can be tied to it, so
		// the stuck row is cleared.
		logging.Warnf("marketplace: purchase %s: clearing a pending listing claim with no attempt token and no listing", purchaseID)
		return h.resolveListingAttempt(ctx, purchaseID, *seen, "", "")
	}
	if seen.ClientToken == "" {
		// A recorded listing in AWS's own pending state, or one recorded after
		// its rollback cancel failed: cancel it like an active one.
		ec2Client, clientErr := h.marketplaceClientFor(ctx, row)
		if clientErr != nil {
			return nil, clientErr
		}
		return h.cancelListing(ctx, purchaseID, seen, seen.ListingID, ec2Client)
	}
	return h.recoverPendingAttempt(ctx, purchaseID, row, *seen)
}

// recoverPendingAttempt resolves a pending row that holds the token of a list
// attempt that never recorded its outcome: in flight, or crashed between the
// AWS create and the DB write. AWS is asked whether the attempt created a
// listing; a live one is canceled, an ended one recorded.
func (h *Handler) recoverPendingAttempt(ctx context.Context, purchaseID string, row *config.PurchaseHistoryRecord, seen config.MarketplaceListingAttempt) (any, error) {
	cfg, err := h.loadMarketplaceAWSConfig(ctx, row)
	if err != nil {
		return nil, err
	}
	listing, found, err := h.buildMarketplaceFinder(cfg).FindMarketplaceListingByToken(ctx, purchaseID, seen.ClientToken)
	if err != nil {
		return nil, mapAWSMarketplaceError("could not look up the pending listing attempt on AWS", err)
	}
	if !found {
		return h.releaseAbandonedAttempt(ctx, purchaseID, seen)
	}
	if isDeadListingState(listing.State) {
		return h.resolveListingAttempt(ctx, purchaseID, seen, listing.ListingID, listing.State)
	}
	return h.cancelListing(ctx, purchaseID, &seen, listing.ListingID, h.buildMarketplaceEC2Client(cfg))
}

// releaseAbandonedAttempt frees a pending row whose attempt created nothing on
// AWS. The token is kept: a list request still in flight with it records its
// listing as usual, and the next list request resumes the same attempt, so
// AWS still deduplicates on the token and no second listing can appear.
func (h *Handler) releaseAbandonedAttempt(ctx context.Context, purchaseID string, seen config.MarketplaceListingAttempt) (any, error) {
	if err := h.config.ReleaseMarketplaceListingClaim(ctx, purchaseID, seen.ClientToken, "", true); err != nil {
		return nil, fmt.Errorf("failed to release the pending marketplace listing claim: %w", err)
	}
	logging.Warnf("marketplace: purchase %s: no AWS listing for the pending attempt; released the claim", purchaseID)
	return map[string]string{
		"listing_id":    "",
		"listing_state": "",
		"note":          "AWS has no listing for the pending attempt, so nothing was canceled and the RI can be listed again",
	}, nil
}

// cancelListing cancels listingID on AWS and records the outcome. seen is the
// attempt a pending row was read with, nil for an active row. A cancel that
// fails because the listing already ended (a retry after the DB write of an
// earlier cancel failed) records the ended state instead.
func (h *Handler) cancelListing(ctx context.Context, purchaseID string, seen *config.MarketplaceListingAttempt, listingID string, ec2Client marketplaceEC2Client) (any, error) {
	result, err := ec2Client.CancelMarketplaceListing(ctx, listingID)
	if err != nil {
		ended, ok := endedMarketplaceListing(ctx, ec2Client, listingID, err)
		if !ok {
			logging.Warnf("marketplace: CancelReservedInstancesListing for listing %s failed: %v", listingID, err)
			return nil, mapAWSMarketplaceError("AWS cancel listing failed", err)
		}
		result = ended
	}
	state := result.State
	if state == "" {
		// The cancel call succeeded but AWS reported no status: never record
		// an empty state, which would read as "not listed".
		state = config.ListingStateCancelled
	}

	// AWS has canceled the listing; there is no rollback, so record it on a
	// detached context the caller disconnecting cannot abort.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), marketplaceCompensationTimeout)
	defer cancel()
	if seen != nil {
		return h.resolveListingAttempt(recCtx, purchaseID, *seen, listingID, state)
	}
	if dbErr := h.config.UpdatePurchaseHistoryListing(recCtx, purchaseID, listingID, state); dbErr != nil {
		logging.Errorf("marketplace: listing canceled in AWS (%s) but DB update failed: %v; state is out of sync", listingID, dbErr)
		return nil, fmt.Errorf("listing canceled in AWS but could not be persisted; retry the cancel to record it: %w", dbErr)
	}
	return map[string]string{"listing_id": listingID, "listing_state": state}, nil
}

// endedMarketplaceListing reports whether listingID has already ended on AWS,
// read after its cancel failed. A role-assumption failure would fail the read
// the same way, so it is not retried.
func endedMarketplaceListing(ctx context.Context, ec2Client marketplaceEC2Client, listingID string, cancelErr error) (ec2svc.MarketplaceListingResult, bool) {
	if isRoleAssumptionFailure(cancelErr) {
		return ec2svc.MarketplaceListingResult{}, false
	}
	listing, err := ec2Client.DescribeMarketplaceListing(ctx, listingID)
	if err != nil || !isDeadListingState(listing.State) {
		return ec2svc.MarketplaceListingResult{}, false
	}
	logging.Warnf("marketplace: listing %s is already %s on AWS (cancel said: %v); recording it", listingID, listing.State, cancelErr)
	return ec2svc.MarketplaceListingResult{ListingID: listingID, State: listing.State}, true
}

// resolveListingAttempt records listingID in state on a pending row, only
// while the row is still as seen or already holds listingID.
func (h *Handler) resolveListingAttempt(ctx context.Context, purchaseID string, seen config.MarketplaceListingAttempt, listingID, state string) (any, error) {
	applied, err := h.config.ResolveMarketplaceListingAttempt(ctx, purchaseID, seen, listingID, state)
	if err != nil {
		logging.Errorf("marketplace: failed to record listing %s (%s) for purchase %s: %v", listingID, state, purchaseID, err)
		return nil, fmt.Errorf("failed to record the marketplace listing outcome; retry the cancel to record it: %w", err)
	}
	if !applied {
		return nil, NewClientError(409, marketplaceListingChangedMessage)
	}
	return map[string]string{"listing_id": listingID, "listing_state": state}, nil
}

// isListingEndedConflict reports a list-path write refused because another
// request already recorded the listing as canceled or closed.
func isListingEndedConflict(err error) bool {
	return errors.Is(err, config.ErrMarketplaceListingEnded)
}
