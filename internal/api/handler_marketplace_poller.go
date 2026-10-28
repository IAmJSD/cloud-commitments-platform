package api

// handler_marketplace_poller.go is the RI Marketplace status poller (issue
// #27). The list and cancel endpoints record a listing as active or canceled;
// nothing else told CUDly when AWS sold it. The poller reads from AWS
// (DescribeReservedInstancesListings, signed as the purchase's own account
// like the endpoints) every active listing, and every listing recorded as
// pending because AWS gave no status when it was created (issue #1494). It
// records the listing as active with its AWS CreateDate while it is for sale,
// and its terminal state with the realized proceeds and the AWS fee once it
// closes (all sold) or is canceled after a partial sale.
//
// It runs from the finalize_revocations scheduled task (see
// internal/server/handler_marketplace_poll.go), so only where that task is
// scheduled; docs/DEPLOYMENT.md lists the platforms. Its writes are the
// store's status-guarded UPDATEs, so a second run, the cancel handler or a
// relist racing a poll never double-record proceeds or undo a recorded cancel.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	sdkec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// marketplaceListingStatus is what the poller needs from one AWS listing.
type marketplaceListingStatus struct {
	ListingID string
	// State is the AWS listing status, lowercased.
	State string
	// CreatedAt is the listing's CreateDate; zero when AWS did not report it.
	CreatedAt time.Time
	// SoldCount is the number of the listing's instances AWS reports as sold.
	SoldCount int64
	// UnitPrice is the per-instance USD price of the listing's active price
	// schedule tier, the price a buyer pays. PriceKnown is false when no single
	// active USD tier with a price exists.
	UnitPrice  float64
	PriceKnown bool
}

var errMarketplaceListingNotFound = errors.New("marketplace listing not found on AWS")

// DescribeMarketplaceListingStatus reads one listing by id.
func (f awsMarketplaceListingFinder) DescribeMarketplaceListingStatus(ctx context.Context, listingID string) (marketplaceListingStatus, error) {
	out, err := f.api.DescribeReservedInstancesListings(ctx, &sdkec2.DescribeReservedInstancesListingsInput{
		ReservedInstancesListingId: aws.String(listingID),
	})
	if err != nil {
		return marketplaceListingStatus{}, fmt.Errorf("DescribeReservedInstancesListings failed: %w", err)
	}
	for i := range out.ReservedInstancesListings {
		l := &out.ReservedInstancesListings[i]
		if aws.ToString(l.ReservedInstancesListingId) == listingID {
			return marketplaceListingStatusFromSDK(l), nil
		}
	}
	return marketplaceListingStatus{}, fmt.Errorf("%w: %s", errMarketplaceListingNotFound, listingID)
}

func marketplaceListingStatusFromSDK(l *ec2types.ReservedInstancesListing) marketplaceListingStatus {
	st := marketplaceListingStatus{
		ListingID: aws.ToString(l.ReservedInstancesListingId),
		State:     strings.ToLower(string(l.Status)),
		CreatedAt: aws.ToTime(l.CreateDate),
	}
	for _, c := range l.InstanceCounts {
		if c.State == ec2types.ListingStateSold {
			st.SoldCount += int64(aws.ToInt32(c.InstanceCount))
		}
	}
	st.UnitPrice, st.PriceKnown = activeUSDListingPrice(l.PriceSchedules)
	return st
}

// activeUSDListingPrice returns the price of the one active price schedule
// tier. AWS marks exactly one tier active, the one matching the term the RI
// has left; anything else (none, several, a non-USD or missing or negative
// price) is reported as unknown rather than guessed.
func activeUSDListingPrice(schedules []ec2types.PriceSchedule) (float64, bool) {
	var price *float64
	for i := range schedules {
		s := &schedules[i]
		if !aws.ToBool(s.Active) {
			continue
		}
		if price != nil || s.Price == nil || *s.Price < 0 ||
			(s.CurrencyCode != "" && s.CurrencyCode != ec2types.CurrencyCodeValuesUsd) {
			return 0, false
		}
		price = s.Price
	}
	if price == nil {
		return 0, false
	}
	return *price, true
}

// marketplaceSettlementAmounts returns the seller's net proceeds and the AWS
// fee, in USD, for sold instances at unitPrice each. The fee is
// awsMarketplaceFeePercent of the sale price. The math is done in whole cents
// so proceeds + fee always equals the sale price exactly.
func marketplaceSettlementAmounts(sold int64, unitPrice float64) (proceeds, fee float64) {
	grossCents := int64(math.Round(unitPrice*100)) * sold
	feeCents := (grossCents*awsMarketplaceFeePercent + 50) / 100
	return float64(grossCents-feeCents) / 100, float64(feeCents) / 100
}

// marketplaceSettlement builds the store write for a listing that ended on
// AWS. priced is false when instances sold but their price is unknown; the
// state is then recorded without proceeds and the row stays due for a poll.
func marketplaceSettlement(row *config.PurchaseHistoryRecord, st marketplaceListingStatus) (settlement config.MarketplaceListingSettlement, priced bool) {
	settlement = config.MarketplaceListingSettlement{PurchaseID: row.PurchaseID, ListingID: row.ListingID, State: st.State}
	if !st.CreatedAt.IsZero() {
		listedAt := st.CreatedAt
		settlement.ListedAt = &listedAt
	}
	if st.SoldCount > 0 && !st.PriceKnown {
		return settlement, false
	}
	var proceeds, fee float64
	if st.SoldCount > 0 {
		proceeds, fee = marketplaceSettlementAmounts(st.SoldCount, st.UnitPrice)
	}
	settlement.ProceedsReceived, settlement.FeePaid = &proceeds, &fee
	return settlement, true
}

// MarketplacePollResult reports one poller run.
type MarketplacePollResult struct {
	// Found is the number of listings the run read.
	Found int `json:"found"`
	// Active listings are still for sale on AWS.
	Active int `json:"active"`
	// Settled listings were recorded closed or canceled with their proceeds.
	Settled int `json:"settled"`
	// Unpriced listings were recorded closed or canceled, but some instances
	// sold at a price AWS did not identify; they are read again next run.
	Unpriced int `json:"unpriced"`
	// Skipped listings needed no write, or the row changed under the poll
	// (canceled, relisted or settled by another run) and the write was dropped.
	Skipped int `json:"skipped"`
	// Errored listings could not be read from AWS or recorded.
	Errored int `json:"errored"`
}

type marketplacePollOutcome int

const (
	marketplacePollSkipped marketplacePollOutcome = iota
	marketplacePollActive
	marketplacePollSettled
	marketplacePollUnpriced
)

func (r *MarketplacePollResult) count(o marketplacePollOutcome) {
	switch o {
	case marketplacePollActive:
		r.Active++
	case marketplacePollSettled:
		r.Settled++
	case marketplacePollUnpriced:
		r.Unpriced++
	default:
		r.Skipped++
	}
}

// PollMarketplaceListings reads every listing due for a poll from AWS and
// records what changed. A failure on one listing is logged and counted, and
// the run goes on with the next; only failing to list the rows, or the context
// ending, fails the run.
func (h *Handler) PollMarketplaceListings(ctx context.Context) (*MarketplacePollResult, error) {
	rows, err := h.config.ListMarketplaceListingsToPoll(ctx)
	if err != nil {
		return nil, err
	}
	result := &MarketplacePollResult{Found: len(rows)}
	for i := range rows {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		row := &rows[i]
		outcome, err := h.pollMarketplaceListing(ctx, row)
		if err != nil {
			logging.Warnf("marketplace poller: listing %s of purchase %s: %v", row.ListingID, row.PurchaseID, err)
			result.Errored++
			continue
		}
		if outcome == marketplacePollUnpriced {
			logging.Warnf("marketplace poller: listing %s of purchase %s ended with sold instances but no active USD price; proceeds not recorded", row.ListingID, row.PurchaseID)
		}
		result.count(outcome)
	}
	return result, nil
}

func (h *Handler) pollMarketplaceListing(ctx context.Context, row *config.PurchaseHistoryRecord) (marketplacePollOutcome, error) {
	cfg, err := h.loadMarketplaceAWSConfig(ctx, row)
	if err != nil {
		return marketplacePollSkipped, err
	}
	st, err := h.buildMarketplaceFinder(cfg).DescribeMarketplaceListingStatus(ctx, row.ListingID)
	if err != nil {
		return marketplacePollSkipped, err
	}
	return h.recordMarketplaceListingStatus(ctx, row, st)
}

// recordMarketplaceListingStatus writes what AWS reported for the row's
// listing. AWS's own pending state (a listing still being set up) needs no
// write.
func (h *Handler) recordMarketplaceListingStatus(ctx context.Context, row *config.PurchaseHistoryRecord, st marketplaceListingStatus) (marketplacePollOutcome, error) {
	switch st.State {
	case config.ListingStateActive:
		return h.recordMarketplaceListingActive(ctx, row, st)
	case config.ListingStateClosed, config.ListingStateCancelled:
		settlement, priced := marketplaceSettlement(row, st)
		applied, err := h.config.SettleMarketplaceListing(ctx, settlement)
		switch {
		case err != nil:
			return marketplacePollSkipped, err
		case !applied:
			return marketplacePollSkipped, nil
		case !priced:
			return marketplacePollUnpriced, nil
		}
		return marketplacePollSettled, nil
	}
	return marketplacePollSkipped, nil
}

// recordMarketplaceListingActive records a listing AWS reports for sale. An
// active row needs a write only to stamp the CreateDate. A row recorded as
// pending is moved to active even when AWS reported no CreateDate, so it can
// be canceled and relisted like any active listing (issue #1494).
func (h *Handler) recordMarketplaceListingActive(ctx context.Context, row *config.PurchaseHistoryRecord, st marketplaceListingStatus) (marketplacePollOutcome, error) {
	if st.CreatedAt.IsZero() && !strings.EqualFold(row.ListingState, config.ListingStatePending) {
		return marketplacePollActive, nil
	}
	if _, err := h.config.RecordMarketplaceListingActive(ctx, row.PurchaseID, row.ListingID, st.CreatedAt); err != nil {
		return marketplacePollSkipped, err
	}
	return marketplacePollActive, nil
}
