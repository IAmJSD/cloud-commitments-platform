package server

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
)

// marketplaceListingPoller is the part of the API handler the
// finalize_revocations sweep uses to poll RI Marketplace listings (issue #27).
type marketplaceListingPoller interface {
	PollMarketplaceListings(ctx context.Context) (*api.MarketplacePollResult, error)
}

// finalizeSweepResult is the finalize_revocations task's result. The embedded
// revocation counts keep their existing keys; the marketplace poll is added
// under its own key.
type finalizeSweepResult struct {
	*purchase.FinalizeResult
	MarketplaceListings *api.MarketplacePollResult `json:"marketplace_listings,omitempty"`
}

// handleFinalizeSweep runs the finalize_revocations task: the in-flight
// revocation repair, then the RI Marketplace listing poll. The poll rides on
// this sweep because both reconcile purchase_history with the provider on the
// same hourly cadence, and it needs no new schedule or infrastructure. The two
// steps are independent: either one failing does not skip the other, and the
// task fails if either did.
func (app *Application) handleFinalizeSweep(ctx context.Context) (*finalizeSweepResult, error) {
	revocations, revErr := app.handleFinalizeRevocations(ctx)
	listings, pollErr := app.pollMarketplaceListings(ctx)
	return &finalizeSweepResult{FinalizeResult: revocations, MarketplaceListings: listings}, errors.Join(revErr, pollErr)
}

// pollMarketplaceListings records listing state changes, proceeds and fees
// from AWS. It is a no-op when no API handler is wired (no database yet).
func (app *Application) pollMarketplaceListings(ctx context.Context) (*api.MarketplacePollResult, error) {
	poller := app.marketplaceListingPoller()
	if poller == nil {
		return nil, nil
	}
	log.Println("Polling RI Marketplace listings...")
	result, err := poller.PollMarketplaceListings(ctx)
	if err != nil {
		log.Printf("Failed to poll RI Marketplace listings: %v", err)
		return result, fmt.Errorf("marketplace listing poll: %w", err)
	}
	log.Printf("Marketplace listing poll complete: found=%d active=%d settled=%d unpriced=%d skipped=%d errored=%d",
		result.Found, result.Active, result.Settled, result.Unpriced, result.Skipped, result.Errored)
	return result, nil
}

func (app *Application) marketplaceListingPoller() marketplaceListingPoller {
	if app.marketplacePoller != nil {
		return app.marketplacePoller
	}
	if app.API != nil {
		return app.API
	}
	return nil
}
