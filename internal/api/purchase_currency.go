package api

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// purchaseCapCurrency is the currency every purchases MaxPurchaseAmount
// (group permission and API-key max_amount) is denominated in. The cap has
// no currency field of its own (PR #1515): it is USD by construction, and
// there is no FX conversion here.
const purchaseCapCurrency = "USD"

// nonUSDPurchaseCurrency returns "" when every rec's CommitmentCurrency is
// USD, so its total commitment can be compared against a USD cap. Otherwise
// it returns the first rec's currency that is not USD, or "unknown" when a
// rec carries none (issue #361).
func nonUSDPurchaseCurrency(recs []config.RecommendationRecord) string {
	for i := range recs {
		currency := strings.TrimSpace(recs[i].CommitmentCurrency)
		if strings.EqualFold(currency, purchaseCapCurrency) {
			continue
		}
		if currency == "" {
			return "unknown"
		}
		return currency
	}
	return ""
}

// requireUncappedPurchaseGrant enforces sets for a batch whose amounts are
// not provably USD. Comparing the raw amount against a USD cap is wrong in
// whichever direction the exchange rate points: 900 KWD (about 2,900 USD)
// clears a 1,000 USD cap, and 5,000 CNY (about 700 USD) is refused by it.
// So the amount is replaced with an unmatchable sentinel
// (math.MaxFloat64): a grant with ANY MaxPurchaseAmount refuses it, and a
// grant with no amount cap still allows it. This mirrors
// checkAzureExecuteConstraints on the Azure RI exchange path.
//
// When the sentinel check is refused, a second check with the amount
// dimension neutralized (0, which a well-formed cap always satisfies)
// tells the two causes apart: if it is refused too, another dimension
// (account, provider, service, region) is the reason and that error is
// returned unchanged; otherwise the spend cap is, and a 403 naming the
// currency is returned.
func (h *Handler) requireUncappedPurchaseGrant(ctx context.Context, session *Session, action string, sets []auth.PermissionConstraints, currency string) error {
	err := h.requirePermissionConstraints(ctx, session, action, "purchases", withPurchaseAmount(sets, math.MaxFloat64))
	if err == nil {
		return nil
	}
	if ce, ok := IsClientError(err); !ok || ce.code != 403 {
		return err
	}
	if otherErr := h.requirePermissionConstraints(ctx, session, action, "purchases", withPurchaseAmount(sets, 0)); otherErr != nil {
		return otherErr
	}
	return NewClientError(403, fmt.Sprintf(
		"permission denied: the constraints configured on your %s permission for purchases include a USD spend cap (MaxPurchaseAmount), "+
			"and this purchase is priced in %s currency, so the cap cannot be enforced against it; "+
			"refresh the recommendations so they carry a USD price, or ask an administrator to remove the cap",
		action, currency))
}

// withPurchaseAmount returns a copy of sets with every MaxPurchaseAmount set
// to amount, leaving the caller's slice untouched.
func withPurchaseAmount(sets []auth.PermissionConstraints, amount float64) []auth.PermissionConstraints {
	out := make([]auth.PermissionConstraints, len(sets))
	for i := range sets {
		out[i] = sets[i]
		out[i].MaxPurchaseAmount = amount
	}
	return out
}
