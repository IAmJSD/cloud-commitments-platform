package api

// handler_marketplace_attempt.go holds the create-listing attempt of the
// marketplace-list flow (issue #525). The attempt's ClientToken and price
// schedule are persisted in the statement that claims the listing slot, before
// the AWS call. A retry after an ambiguous AWS error or a crash resends the
// stored token and schedule, so AWS returns the existing listing instead of
// creating a second one, and first asks AWS whether the earlier attempt
// already created the listing (reconcile) and records it if so.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	sdkec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/google/uuid"
)

// marketplaceListingFinder looks up a listing created by an earlier attempt
// through DescribeReservedInstancesListings. The ec2 service wrapper has no
// lookup by ClientToken, so this is a separate narrow interface. The status
// poller reads a listing's sale details through it too (issue #27), because
// the wrapper's DescribeMarketplaceListing returns only the state.
type marketplaceListingFinder interface {
	FindMarketplaceListingByToken(ctx context.Context, reservedInstancesID, clientToken string) (ec2svc.MarketplaceListingResult, bool, error)
	DescribeMarketplaceListingStatus(ctx context.Context, listingID string) (marketplaceListingStatus, error)
}

type describeListingsAPI interface {
	DescribeReservedInstancesListings(ctx context.Context, in *sdkec2.DescribeReservedInstancesListingsInput, opts ...func(*sdkec2.Options)) (*sdkec2.DescribeReservedInstancesListingsOutput, error)
}

type awsMarketplaceListingFinder struct{ api describeListingsAPI }

func (f awsMarketplaceListingFinder) FindMarketplaceListingByToken(ctx context.Context, reservedInstancesID, clientToken string) (ec2svc.MarketplaceListingResult, bool, error) {
	out, err := f.api.DescribeReservedInstancesListings(ctx, &sdkec2.DescribeReservedInstancesListingsInput{
		ReservedInstancesId: aws.String(reservedInstancesID),
	})
	if err != nil {
		return ec2svc.MarketplaceListingResult{}, false, fmt.Errorf("DescribeReservedInstancesListings failed: %w", err)
	}
	for i := range out.ReservedInstancesListings {
		l := &out.ReservedInstancesListings[i]
		if aws.ToString(l.ClientToken) == clientToken {
			return ec2svc.MarketplaceListingResult{ListingID: aws.ToString(l.ReservedInstancesListingId), State: string(l.Status)}, true, nil
		}
	}
	return ec2svc.MarketplaceListingResult{}, false, nil
}

func (h *Handler) buildMarketplaceFinder(cfg aws.Config) marketplaceListingFinder {
	if h.marketplaceFinderFactory != nil {
		return h.marketplaceFinderFactory(cfg)
	}
	return awsMarketplaceListingFinder{api: sdkec2.NewFromConfig(cfg)}
}

// marketplaceInstanceCount returns how many RIs of the row to list. A row of N
// Standard RIs must list all N, not a single unit (issue #292 multi-count fix).
// It floors at 1 for legacy rows that recorded a non-positive count so a valid
// Standard RI still lists, and rejects an implausibly large count rather than
// silently truncating it into int32 (which could list the wrong number of RIs
// on the money path).
func marketplaceInstanceCount(row *config.PurchaseHistoryRecord) (int32, error) {
	switch {
	case row.Count > math.MaxInt32:
		return 0, NewClientError(500, fmt.Sprintf("purchase count %d exceeds the marketplace listing limit", row.Count))
	case row.Count > 1:
		return int32(row.Count), nil
	}
	return 1, nil
}

func toAWSPriceSchedule(schedule []MarketplacePriceTier) []ec2svc.MarketplacePriceTier {
	out := make([]ec2svc.MarketplacePriceTier, 0, len(schedule))
	for _, t := range schedule {
		out = append(out, ec2svc.MarketplacePriceTier{Term: t.TermMonths, Price: t.Price})
	}
	return out
}

// reserveAndCreateListing atomically claims the marketplace-listing slot for the
// row (persisting the attempt's token and schedule), reconciles an earlier
// unresolved attempt, creates the AWS listing, and persists it, releasing the
// claim on every failure path that leaves no live listing so a failed attempt
// does not leave the row stuck in the transient pending state. Returns the
// persisted listing and the schedule it was created with, which on a retry is
// the stored one, not the one passed in.
func (h *Handler) reserveAndCreateListing(ctx context.Context, purchaseID string, row *config.PurchaseHistoryRecord, cfg aws.Config, ec2Client marketplaceEC2Client, schedule []MarketplacePriceTier) (ec2svc.MarketplaceListingResult, []MarketplacePriceTier, error) {
	instanceCount, err := marketplaceInstanceCount(row)
	if err != nil {
		return ec2svc.MarketplaceListingResult{}, nil, err
	}
	claim, used, err := h.claimListingSlot(ctx, purchaseID, schedule)
	if err != nil {
		return ec2svc.MarketplaceListingResult{}, nil, err
	}
	if claim.Resumed {
		result, found, recErr := h.reconcileAttempt(ctx, purchaseID, claim, h.buildMarketplaceFinder(cfg))
		if recErr != nil || found {
			return result, used, recErr
		}
	}
	result, err := h.createAndPersistListing(ctx, purchaseID, claim, ec2Client, instanceCount, toAWSPriceSchedule(used))
	return result, used, err
}

// claimListingSlot claims the row with a fresh random token. The token is
// persisted by the claim itself and only the persisted copy is used, so it
// never depends on a row read before the claim or on the clock.
func (h *Handler) claimListingSlot(ctx context.Context, purchaseID string, schedule []MarketplacePriceTier) (*config.MarketplaceListingClaim, []MarketplacePriceTier, error) {
	encoded, err := json.Marshal(schedule)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode marketplace price schedule: %w", err)
	}
	claim, err := h.config.ClaimMarketplaceListingSlot(ctx, purchaseID, uuid.NewString(), encoded)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to reserve marketplace listing slot: %w", err)
	}
	if claim == nil {
		return nil, nil, NewClientError(409, "a marketplace listing is already active or in progress for this RI; cancel it first")
	}
	if !claim.Resumed {
		return claim, schedule, nil
	}
	var stored []MarketplacePriceTier
	if err = json.Unmarshal(claim.PriceSchedule, &stored); err != nil {
		h.releaseMarketplaceClaim(ctx, purchaseID, claim, true)
		return nil, nil, fmt.Errorf("stored marketplace price schedule for purchase %s is unusable: %w", purchaseID, err)
	}
	if len(stored) == 0 {
		h.releaseMarketplaceClaim(ctx, purchaseID, claim, true)
		return nil, nil, fmt.Errorf("stored marketplace price schedule for purchase %s is empty", purchaseID)
	}
	return claim, stored, nil
}

// reconcileAttempt asks AWS whether the earlier attempt that persisted
// claim.ClientToken created its listing. found reports that the attempt is
// resolved: result holds the recorded listing, or err why it ended in a dead
// one. Not found means AWS has nothing for the token, so the caller creates
// the listing with that same token.
func (h *Handler) reconcileAttempt(ctx context.Context, purchaseID string, claim *config.MarketplaceListingClaim, finder marketplaceListingFinder) (ec2svc.MarketplaceListingResult, bool, error) {
	listing, found, err := finder.FindMarketplaceListingByToken(ctx, purchaseID, claim.ClientToken)
	if err != nil {
		h.releaseMarketplaceClaim(ctx, purchaseID, claim, true)
		return ec2svc.MarketplaceListingResult{}, false, mapAWSMarketplaceError("could not reconcile the earlier listing attempt on AWS", err)
	}
	if !found {
		return ec2svc.MarketplaceListingResult{}, false, nil
	}
	logging.Warnf("marketplace: purchase %s: earlier attempt already created listing %s (%s); recording it", purchaseID, listing.ListingID, listing.State)
	if isDeadListingState(listing.State) {
		return ec2svc.MarketplaceListingResult{}, true, h.recordDeadListing(ctx, purchaseID, claim, listing)
	}
	if err = h.config.UpdatePurchaseHistoryListing(ctx, purchaseID, listing.ListingID, listing.State); err != nil {
		h.releaseMarketplaceClaim(ctx, purchaseID, claim, true)
		return ec2svc.MarketplaceListingResult{}, true, fmt.Errorf("listing %s exists on AWS but could not be recorded: %w", listing.ListingID, err)
	}
	return listing, true, nil
}

func (h *Handler) createAndPersistListing(ctx context.Context, purchaseID string, claim *config.MarketplaceListingClaim, ec2Client marketplaceEC2Client, instanceCount int32, awsSchedule []ec2svc.MarketplacePriceTier) (ec2svc.MarketplaceListingResult, error) {
	result, err := ec2Client.CreateMarketplaceListing(ctx, ec2svc.MarketplaceListingRequest{
		ReservedInstancesID: purchaseID,
		ClientToken:         claim.ClientToken,
		PriceSchedule:       awsSchedule,
		InstanceCount:       instanceCount,
	})
	if err != nil {
		mapped := mapAWSMarketplaceError("AWS marketplace listing failed", err)
		// Release the claim so a retry is not blocked. AWS may still have created
		// the listing unless it rejected the request as a client fault, so only
		// then is the token dropped; otherwise the retry resends it.
		h.releaseMarketplaceClaim(ctx, purchaseID, claim, !isClientFaultError(mapped))
		logging.Warnf("marketplace: CreateReservedInstancesListing for purchase %s failed: %v", purchaseID, err)
		return ec2svc.MarketplaceListingResult{}, mapped
	}

	if isDeadListingState(result.State) {
		return ec2svc.MarketplaceListingResult{}, h.recordDeadListing(ctx, purchaseID, claim, result)
	}

	// Persist the listing ID and state. On DB failure, attempt a compensating
	// rollback (cancel the just-created listing) to avoid a desync where the
	// user sees success but the listing is invisible in subsequent renders, then
	// release the claim so the row does not stay stuck in the pending state.
	if dbErr := h.config.UpdatePurchaseHistoryListing(ctx, purchaseID, result.ListingID, result.State); dbErr != nil {
		logging.Errorf("marketplace: listing created (%s / %s) but DB update failed: %v; attempting rollback", result.ListingID, result.State, dbErr)
		compCtx, cancelComp := context.WithTimeout(context.WithoutCancel(ctx), marketplaceCompensationTimeout)
		defer cancelComp()
		if _, rollbackErr := ec2Client.CancelMarketplaceListing(compCtx, result.ListingID); rollbackErr != nil {
			return ec2svc.MarketplaceListingResult{}, h.keepUncanceledListing(compCtx, purchaseID, result, rollbackErr)
		}
		logging.Warnf("marketplace: listing %s rolled back (canceled) after DB failure", result.ListingID)
		h.recordRolledBackListing(compCtx, ctx, purchaseID, claim, result.ListingID)
		return ec2svc.MarketplaceListingResult{}, fmt.Errorf("listing created but could not be persisted; listing has been rolled back: %w", dbErr)
	}

	return result, nil
}

func isClientFaultError(err error) bool {
	ce, ok := IsClientError(err)
	return ok && ce.code == 400
}

func isDeadListingState(state string) bool {
	return strings.EqualFold(state, config.ListingStateCancelled) || strings.EqualFold(state, config.ListingStateClosed)
}

// recordDeadListing handles a listing of this attempt that is canceled or
// closed on AWS: it records the listing, which also drops the attempt's token,
// so the next attempt starts fresh instead of AWS replaying the dead listing.
func (h *Handler) recordDeadListing(ctx context.Context, purchaseID string, claim *config.MarketplaceListingClaim, listing ec2svc.MarketplaceListingResult) error {
	if err := h.config.UpdatePurchaseHistoryListing(ctx, purchaseID, listing.ListingID, listing.State); err != nil {
		logging.Errorf("marketplace: failed to record dead listing %s for purchase %s: %v", listing.ListingID, purchaseID, err)
		h.releaseMarketplaceClaim(ctx, purchaseID, claim, false)
	}
	return NewClientError(502, fmt.Sprintf("AWS returned listing %s in state %s instead of a live listing; retry to create a new one", listing.ListingID, listing.State))
}

// recordRolledBackListing records the canceled listing and with it drops the
// attempt's token, so the next attempt does not replay the canceled listing.
func (h *Handler) recordRolledBackListing(compCtx, ctx context.Context, purchaseID string, claim *config.MarketplaceListingClaim, listingID string) {
	if err := h.config.UpdatePurchaseHistoryListing(compCtx, purchaseID, listingID, config.ListingStateCancelled); err != nil {
		logging.Errorf("marketplace: failed to record rolled-back listing %s for purchase %s: %v", listingID, purchaseID, err)
		h.releaseMarketplaceClaim(ctx, purchaseID, claim, false)
	}
}

// releaseMarketplaceClaim moves the row from pending back to the state it had
// when claimed. It runs on every failure path after a successful claim so a
// failed attempt does not leave the row stuck in the transient pending state
// (which would block future list attempts unless a token resumes it).
// keepAttempt keeps the persisted token and schedule for the retry.
// Best-effort: on error it logs loudly because the row may stay pending.
func (h *Handler) releaseMarketplaceClaim(ctx context.Context, purchaseID string, claim *config.MarketplaceListingClaim, keepAttempt bool) {
	prior := claim.PriorState
	if strings.EqualFold(prior, config.ListingStatePending) {
		prior = ""
	}
	if err := h.config.ReleaseMarketplaceListingClaim(ctx, purchaseID, claim.ClientToken, prior, keepAttempt); err != nil {
		logging.Errorf("marketplace: failed to release listing claim for purchase %s (row may be stuck in %q): %v", purchaseID, config.ListingStatePending, err)
	}
}
