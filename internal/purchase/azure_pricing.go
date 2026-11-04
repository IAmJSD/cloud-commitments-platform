package purchase

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

var ErrStaleAzurePricing = errors.New("stale Azure recommendation pricing; refresh recommendations and submit a new purchase")

func (m *Manager) loadCurrentAzurePricing(ctx context.Context) ([]config.RecommendationRecord, error) {
	rows, err := m.config.ListStoredRecommendations(ctx, config.RecommendationFilter{Provider: "azure", RequireAzurePricingMigration: true})
	if err != nil {
		if errors.Is(err, config.ErrAzurePricingNotReady) {
			return nil, fmt.Errorf("%w: %w", ErrStaleAzurePricing, err)
		}
		return nil, fmt.Errorf("load current Azure recommendation pricing: %w", err)
	}
	return rows, nil
}

func (m *Manager) staleAzurePricingRefusal(ctx context.Context, exec *config.PurchaseExecution) error {
	var azure []config.RecommendationRecord
	for i := range exec.Recommendations {
		rec := &exec.Recommendations[i]
		if rec.Provider == "azure" {
			azure = append(azure, *rec)
		}
	}
	if len(azure) == 0 {
		return nil
	}
	rows, err := m.loadCurrentAzurePricing(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]config.RecommendationRecord, len(rows))
	for i := range rows {
		byID[rows[i].ID] = rows[i]
	}
	for i := range azure {
		rec := &azure[i]
		stored, ok := byID[rec.ID]
		if rec.ID == "" || !ok || !azurePricingMatches(*rec, stored) {
			return fmt.Errorf("%w: Azure recommendation %q no longer has the approved price", ErrStaleAzurePricing, rec.ID)
		}
	}
	return m.heldAzurePricingRefusal(ctx, azure)
}

// heldAzurePricingRefusal refuses Azure recs whose account's stored rows are
// held through a Retail Prices outage. Those rows are the previous cycle's, so
// they match the approved execution while their price may be days old.
// A rec without an account (an unresolved ambient identity) cannot be named in
// the hold, so any current Azure hold refuses it. Fails closed when the
// collection state cannot be read.
func (m *Manager) heldAzurePricingRefusal(ctx context.Context, azure []config.RecommendationRecord) error {
	fresh, err := m.config.GetRecommendationsFreshness(ctx)
	if err != nil {
		return fmt.Errorf("%w: cannot read the latest collection state to confirm Azure prices are current: %w", ErrStaleAzurePricing, err)
	}
	if fresh == nil {
		return fmt.Errorf("%w: the latest collection state is missing, so Azure prices cannot be confirmed current", ErrStaleAzurePricing)
	}
	if fresh.LastCollectionPartial == nil {
		return nil
	}
	for _, sweep := range fresh.LastCollectionPartial.Providers {
		if sweep.Provider != "azure" || sweep.Reasons[config.PartialReasonPricingUnavailable] == 0 {
			continue
		}
		for i := range azure {
			if azureAccountHeld(azure[i].CloudAccountID, sweep.HeldAccountIDs) {
				return fmt.Errorf("%w: Azure prices for recommendation %q could not be refreshed (price catalog unavailable); "+
					"refresh recommendations after prices recover and submit a new purchase", ErrStaleAzurePricing, azure[i].ID)
			}
		}
	}
	return nil
}

func azureAccountHeld(accountID *string, held []string) bool {
	return accountID == nil || *accountID == "" || slices.Contains(held, *accountID)
}

func azurePricingMatches(rec, stored config.RecommendationRecord) bool {
	if rec.Count <= 0 || stored.Count <= 0 {
		return false
	}
	total := stored.UpfrontCost
	if stored.MonthlyCost != nil {
		total += *stored.MonthlyCost * float64(stored.Term) * 12
	}
	if total <= 0 || !azureCostEqual(total, total, 1) {
		return false
	}
	ratio := float64(rec.Count) / float64(stored.Count)
	if !azureCostEqual(rec.UpfrontCost, stored.UpfrontCost, ratio) {
		return false
	}
	if rec.MonthlyCost == nil || stored.MonthlyCost == nil {
		return rec.MonthlyCost == nil && stored.MonthlyCost == nil
	}
	return azureCostEqual(*rec.MonthlyCost, *stored.MonthlyCost, ratio)
}

func azureCostEqual(a, b, ratio float64) bool {
	if a < 0 || b < 0 || math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return false
	}
	b *= ratio
	if math.IsInf(b, 0) {
		return false
	}
	// Nonnegative IEEE-754 bit patterns are ordered; allow only rounding drift.
	x, y := math.Float64bits(math.Abs(a)), math.Float64bits(math.Abs(b))
	if x > y {
		return x-y <= 8
	}
	return y-x <= 8
}
