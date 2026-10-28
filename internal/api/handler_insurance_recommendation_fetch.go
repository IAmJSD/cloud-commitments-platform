package api

import (
	"context"
	"errors"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/archera"
)

// insuranceRecommendationTimeout bounds secret resolution and the vendor reads. A var so tests can shorten it.
var insuranceRecommendationTimeout = 20 * time.Second

// lineItemClient is the QuoteClient plus the line-item read that the published interface lacks.
type lineItemClient interface {
	insurance.QuoteClient
	LineItems(ctx context.Context, planID string) ([]insurance.LineItem, error)
}

// FetchRecommendationComparison reads every line item, selects the recommendation's own one and asks for
// a comparison scoped to it. No line item, or an ambiguous one, returns an empty comparison without a
// comparison call. Any failure returns no partial result.
func (h *Handler) FetchRecommendationComparison(ctx context.Context, rec *archera.RecommendationIdentity) (*insurance.Comparison, []insurance.LineItem, error) {
	cctx, cancel := context.WithTimeout(ctx, insuranceRecommendationTimeout)
	defer cancel()
	c, items, err := h.readRecommendationVendor(cctx, rec)
	if err == nil {
		return c, items, nil
	}
	if cctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return nil, nil, NewClientError(504, insuranceTimeoutMessage)
	}
	if ce, ok := IsClientError(err); ok {
		return nil, nil, ce
	}
	return nil, nil, mapInsuranceError(err)
}

func (h *Handler) readRecommendationVendor(ctx context.Context, rec *archera.RecommendationIdentity) (*insurance.Comparison, []insurance.LineItem, error) {
	quote, err := h.insuranceClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	client, ok := quote.(lineItemClient)
	if !ok {
		return nil, nil, NewClientError(503, "Archera recommendation comparison is not supported")
	}
	planID := h.insurance.PlanID()
	items, err := client.LineItems(ctx, planID)
	if err != nil {
		return nil, nil, err
	}
	_, identities, err := archera.ProjectContract(&insurance.Comparison{}, items)
	if err != nil {
		return nil, nil, err
	}
	selected, status := archera.SelectLineItem(rec, identities)
	if status != archera.SelectOne {
		return &insurance.Comparison{FetchedAt: time.Now()}, items, nil
	}
	c, err := client.Comparison(ctx, insurance.ComparisonRequest{PlanID: planID, LineItemIDs: []string{selected},
		PaymentOptions: []insurance.PaymentOption{insurance.PaymentNoUpfront, insurance.PaymentPartialUpfront, insurance.PaymentAllUpfront}})
	return c, items, err
}
