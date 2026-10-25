package api

import (
	"context"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// priceAndEnforcePurchaseConstraints replaces every rec's cost fields with
// values derived from the stored recommendation that matches it, then
// enforces the execute:purchases Constraints against those values. The
// client-supplied upfront_cost / monthly_cost / savings / details are never
// consulted for the spend cap, the execution row or the approval email
// (issue #1905, audit A01-001). Wrapped in one call so
// validateExecutePurchaseRequest keeps its gocyclo score (it is at the
// pre-commit ceiling of 10).
func (h *Handler) priceAndEnforcePurchaseConstraints(ctx context.Context, session *Session, recs []config.RecommendationRecord) error {
	if err := h.priceRecommendationsFromStore(ctx, recs); err != nil {
		return err
	}
	return h.enforcePurchaseConstraints(ctx, session, "execute", recs)
}

// priceRecommendationsFromStore rewrites recs in place. Each rec must match
// a row of the stored recommendation set on its identity tuple
// (config.RecIdentityKey); unmatched recs are refused rather than priced from the
// request. Matching by tuple instead of by rec.ID lets the purchase modal's
// term/payment change (#111, #197, #1903) resolve to the stored variant the
// user actually chose: AWS stores every (term, payment) combination and
// Azure stores both payment variants, each under its own id.
func (h *Handler) priceRecommendationsFromStore(ctx context.Context, recs []config.RecommendationRecord) error {
	byKey, byID, err := h.loadStoredRecommendationIndex(ctx, recs)
	if err != nil {
		return err
	}
	for i := range recs {
		key, keyErr := config.RecIdentityKey(&recs[i])
		if keyErr != nil {
			return NewClientError(400, fmt.Sprintf("recommendation %d (%s): details are malformed: %v", i, describeRec(&recs[i]), keyErr))
		}
		match, ok := byKey[key]
		if !ok {
			return NewClientError(409, fmt.Sprintf(
				"recommendation %d (%s) is not in the current recommendation set; refresh the recommendations and try again",
				i, describeRec(&recs[i])))
		}
		if recs[i].ID != "" {
			origin, ok := byID[recs[i].ID]
			if !ok {
				// The request names a specific origin recommendation that no
				// longer exists in the current stored set (e.g. a collection
				// refresh's stale-row eviction removed it between page
				// load and purchase, while the NEWLY requested term/payment
				// still resolves via config.RecIdentityKey). Refusing here, not
				// skipping the check, closes the fail-open gap: an honest
				// client whose origin row went stale must not silently fall
				// through to whatever configuration `match` happens to
				// carry. Same refusal shape as the tuple-miss case above.
				return NewClientError(409, fmt.Sprintf(
					"recommendation %d (%s): originally selected recommendation %q is no longer in the current recommendation set; refresh recommendations and try again",
					i, describeRec(&recs[i]), recs[i].ID))
			}
			if err := checkPurchaseDetailIdentity(&origin, &match, &recs[i], i); err != nil {
				return err
			}
		} else if err := checkRequestDetailDiscriminators(&recs[i], &match, i); err != nil {
			return err
		}
		priced, err := priceFromStored(&recs[i], &match, i)
		if err != nil {
			return err
		}
		recs[i] = priced
	}
	return nil
}

// loadStoredRecommendationIndex reads the stored rows for every provider in
// the batch (one query per distinct provider, at most three) and indexes
// them two ways: by identity tuple (config.RecIdentityKey, for resolving the
// term/payment-scaled row to price from) and by ID (for the request's OWN
// id, which config.RecIdentityKey-based matching deliberately ignores, but issue
// #334's mismatch check needs to recover the row the caller's id used to
// point at). The store's unique index on the same tuple (migration 000043)
// guarantees one row per key only when provider and payment are
// byte-identical: the index is case-sensitive on both columns, while
// config.RecIdentityKey folds their case, so two rows differing only in case would
// collide here (unreachable today because the scheduler always writes
// lowercase, but not guaranteed by the index itself). Rather than silently
// picking whichever row wins the map insert, a collision is refused: this is
// a money path, and the caller (priceRecommendationsFromStore) must never
// price a purchase off an arbitrarily chosen row.
func (h *Handler) loadStoredRecommendationIndex(ctx context.Context, recs []config.RecommendationRecord) (byKey, byID map[string]config.RecommendationRecord, err error) {
	byKey = make(map[string]config.RecommendationRecord)
	byID = make(map[string]config.RecommendationRecord)
	seen := make(map[string]bool)
	for i := range recs {
		provider := recs[i].Provider
		if seen[provider] {
			continue
		}
		seen[provider] = true
		rows, err := h.config.ListStoredRecommendations(ctx, config.RecommendationFilter{Provider: provider})
		if err != nil {
			return nil, nil, fmt.Errorf("load stored recommendations for %s: %w", provider, err)
		}
		for j := range rows {
			key, err := config.RecIdentityKey(&rows[j])
			if err != nil {
				return nil, nil, fmt.Errorf("stored recommendation %s: %w", rows[j].ID, err)
			}
			if _, dup := byKey[key]; dup {
				return nil, nil, fmt.Errorf("stored recommendations for %s contain more than one row for identity key %q", provider, key)
			}
			byKey[key] = rows[j]
			byID[rows[j].ID] = rows[j]
		}
	}
	return byKey, byID, nil
}

// priceFromStored returns a copy of stored carrying the request's Count,
// RecommendedCount and Selected, with UpfrontCost, MonthlyCost, Savings and
// OnDemandCost scaled by Count/stored.Count (stored costs are totals for
// stored.Count; RI/CUD/Azure prices are linear in count). Savings Plans are
// not count-denominated, so their Count must equal the stored placeholder.
// A stored row that cannot yield a positive price is refused: the cap must
// never be evaluated against a zero or negative commitment.
func priceFromStored(req, stored *config.RecommendationRecord, idx int) (config.RecommendationRecord, error) {
	if stored.Count <= 0 {
		return config.RecommendationRecord{}, NewClientError(409, fmt.Sprintf(
			"recommendation %d (%s): stored recommendation has count %d, cannot derive a per-unit price",
			idx, describeRec(req), stored.Count))
	}
	if stored.UpfrontCost < 0 || (stored.MonthlyCost != nil && *stored.MonthlyCost < 0) || recTotalCommitment(stored) <= 0 {
		return config.RecommendationRecord{}, NewClientError(409, fmt.Sprintf(
			"recommendation %d (%s): stored recommendation carries no usable price (upfront %.2f, monthly %s); the spend cap cannot be enforced against it",
			idx, describeRec(req), stored.UpfrontCost, formatOptionalCost(stored.MonthlyCost)))
	}
	if common.IsSavingsPlan(common.ServiceType(stored.Service)) && req.Count != stored.Count {
		return config.RecommendationRecord{}, NewClientError(409, fmt.Sprintf(
			"recommendation %d (%s): savings plan count %d must equal the recommended count %d; savings plans are priced by hourly commitment, not by count",
			idx, describeRec(req), req.Count, stored.Count))
	}
	ratio := float64(req.Count) / float64(stored.Count)
	out := *stored
	out.Count = req.Count
	out.RecommendedCount = req.RecommendedCount
	out.Selected = req.Selected
	out.UpfrontCost = stored.UpfrontCost * ratio
	out.Savings = stored.Savings * ratio
	out.MonthlyCost = scaledCost(stored.MonthlyCost, ratio)
	out.OnDemandCost = scaledCost(stored.OnDemandCost, ratio)
	return out, nil
}

// checkPurchaseDetailIdentity refuses a repriced purchase when the row the
// request's id used to point at (origin) and the row config.RecIdentityKey just
// resolved the request to (match) carry different purchase-critical Details
// discriminators (EC2 tenancy/platform/scope, RDS AZ config) -- issue #334.
//
// config.RecIdentityKey's tuple already covers Service/Region/ResourceType/Engine/
// Term/Payment, but tenancy/platform/scope/az_config live inside the opaque
// Details blob, so a term/payment change (the purchase modal's #111/#197/
// #1903 flow) can silently resolve to a DIFFERENT purchase configuration
// while the visible account/service/region/instance-type tuple stays the
// same -- e.g. a request built from a 3yr/default-tenancy row, re-priced to
// 1yr, lands on a 1yr/dedicated-tenancy row, and priceFromStored's
// `out := *stored` would copy dedicated tenancy into the response even
// though the caller never asked for it.
//
// Both origin and match come from the SERVER's own trusted stored index
// (never from the client-supplied Details payload, which #1905/audit
// A01-001 established must be ignored entirely for pricing/purchase
// decisions -- see TestHandler_executePurchase_PersistsStoredCostsNotClientCosts).
// The only client input this check trusts is the request's id field, used
// purely as a LOOKUP KEY into that trusted index; if it doesn't resolve to a
// real stored row (bogus, stale, or simply omitted), there is nothing to
// compare against and this is a no-op, identical to pre-fix behavior.
// origin == match (the id still names the row config.RecIdentityKey matched, i.e.
// no term/payment change occurred) is also a no-op.
//
// Savings Plans are deliberately excluded -- hourly_commitment and
// offering_id legitimately vary across priced alternatives and are not
// purchase discriminators (purchaseDetailMismatch has no case for
// *common.SavingsPlanDetails).
//
// Mirrors the frontend's samePurchaseVariantIdentity
// (frontend/src/recommendations.ts), which narrows the purchase modal's
// term/payment alternatives to same-configuration siblings. That frontend
// guard cannot protect every caller (API scripts, the MCP server, a future
// UI bug), so this backend check is the shared identity contract's actual
// enforcement point.
func checkPurchaseDetailIdentity(origin, match, req *config.RecommendationRecord, idx int) error {
	if origin.ID == match.ID {
		return nil
	}
	originDetails, err := common.DecodeServiceDetailsFor(origin.Service, origin.Details)
	if err != nil {
		return fmt.Errorf("recommendation %d (%s): originally-selected recommendation %q details could not be decoded: %w",
			idx, describeRec(req), origin.ID, err)
	}
	matchDetails, err := common.DecodeServiceDetailsFor(match.Service, match.Details)
	if err != nil {
		return fmt.Errorf("recommendation %d (%s): stored recommendation %q details could not be decoded: %w",
			idx, describeRec(req), match.ID, err)
	}
	if mismatch := purchaseDetailMismatch(originDetails, matchDetails); mismatch != "" {
		return NewClientError(409, fmt.Sprintf(
			"recommendation %d (%s): changing term/payment would also change the purchase configuration (%s); "+
				"this is a different commitment than %q, refresh recommendations and submit a fresh purchase for the desired configuration",
			idx, describeRec(req), mismatch, origin.ID))
	}
	return nil
}

// checkRequestDetailDiscriminators is the empty-id counterpart of
// checkPurchaseDetailIdentity (issue #418). With no id there is no origin
// row, so the discriminators the client states in req.Details are compared
// refuse-only against the stored match: a stated tenancy/platform/scope/
// memory_gb/az_config that differs from (or is unknown on) the match is
// refused with 409. Omitted discriminators are not compared, and the client
// values are never copied into the priced record; priceFromStored still
// takes everything from match.
func checkRequestDetailDiscriminators(req, match *config.RecommendationRecord, idx int) error {
	stated, err := common.DecodeServiceDetailsFor(req.Service, req.Details)
	if err != nil {
		return NewClientError(400, fmt.Sprintf("recommendation %d (%s): details are malformed: %v", idx, describeRec(req), err))
	}
	matchDetails, err := common.DecodeServiceDetailsFor(match.Service, match.Details)
	if err != nil {
		return fmt.Errorf("recommendation %d (%s): stored recommendation %q details could not be decoded: %w",
			idx, describeRec(req), match.ID, err)
	}
	if mismatch := purchaseDetailMismatch(stated, matchDetails); mismatch != "" {
		return NewClientError(409, fmt.Sprintf(
			"recommendation %d (%s): the requested purchase configuration does not match the stored recommendation (%s); "+
				"refresh recommendations and submit the desired configuration, or omit details",
			idx, describeRec(req), mismatch))
	}
	return nil
}

// purchaseDetailMismatch reports the purchase-critical discriminator field
// where origin and match diverge, or "" when there is no mismatch (or no
// typed discriminator to compare for this service). See
// checkPurchaseDetailIdentity for the full contract.
//
// A field is compared only when origin is non-empty: a pre-#453 legacy
// ORIGIN row decodes to a zero-valued typed pointer (empty Tenancy/
// Platform/Scope/AZConfig/MemoryGB) precisely because its true
// configuration was never recorded, so there is nothing known to
// contradict. But once origin IS known, an empty match is ALSO a mismatch,
// not a pass: a client may substitute a default for an empty match field,
// which can silently buy a different configuration than the one origin
// recorded (e.g. origin=dedicated tenancy, match=legacy-empty -> the purchase
// would resolve to the default-tenancy substitute). The AWS EC2 client
// rejects an empty Platform, Tenancy or Scope outright, but this check still
// has to refuse it here, before the purchase is attempted.
// "Unknown" is therefore refused right alongside "different", not treated
// as compatible.
func purchaseDetailMismatch(origin, match common.ServiceDetails) string {
	switch m := match.(type) {
	case *common.ComputeDetails:
		o, ok := origin.(*common.ComputeDetails)
		if !ok {
			return ""
		}
		return computeDetailMismatch(o, m)
	case *common.DatabaseDetails:
		o, ok := origin.(*common.DatabaseDetails)
		if !ok {
			return ""
		}
		return databaseDetailMismatch(o, m)
	}
	return ""
}

// mismatchField compares one purchase-critical string field. Returns "" when
// origin is empty (nothing known to contradict). Otherwise returns a
// "<field>: originally %q, now %q" description whenever match is empty
// (unknown -- see purchaseDetailMismatch) or differs from origin.
func mismatchField(field, origin, match string) string {
	if origin == "" {
		return ""
	}
	if match == "" || origin != match {
		return fmt.Sprintf("%s: originally %q, now %q", field, origin, match)
	}
	return ""
}

// mismatchFieldFloat64 is mismatchField for a numeric field whose zero value
// means "unknown" (per ComputeDetails.MemoryGB's doc comment), applying the
// same origin-known/match-unknown-or-different asymmetry.
func mismatchFieldFloat64(field string, origin, match float64) string {
	if origin == 0 {
		return ""
	}
	if match == 0 || origin != match {
		return fmt.Sprintf("%s: originally %g, now %g", field, origin, match)
	}
	return ""
}

func computeDetailMismatch(o, m *common.ComputeDetails) string {
	for _, f := range [...]string{
		mismatchField("tenancy", o.Tenancy, m.Tenancy),
		mismatchField("platform", o.Platform, m.Platform),
		mismatchField("scope", o.Scope, m.Scope),
		// Zonal EC2 reservations are bought in one AZ; a match in another
		// AZ (or with an unknown one) is a different commitment (issue #792).
		mismatchField("availability_zone", o.AvailabilityZone, m.AvailabilityZone),
		// GCP custom machine types (Compute Engine CUDs) read MemoryGB at
		// purchase time to build the machine spec, so a memory mismatch is
		// as purchase-critical as tenancy for that provider.
		mismatchFieldFloat64("memory_gb", o.MemoryGB, m.MemoryGB),
	} {
		if f != "" {
			return f
		}
	}
	return ""
}

func databaseDetailMismatch(o, m *common.DatabaseDetails) string {
	return mismatchField("az_config", o.AZConfig, m.AZConfig)
}

func sumKnownCosts(a, b *float64) *float64 {
	if a == nil || b == nil {
		return nil
	}
	return new(*a + *b)
}

func scaledCost(v *float64, ratio float64) *float64 {
	if v == nil {
		return nil
	}
	s := *v * ratio
	return &s
}

func formatOptionalCost(v *float64) string {
	if v == nil {
		return "absent"
	}
	return fmt.Sprintf("%.2f", *v)
}

func describeRec(rec *config.RecommendationRecord) string {
	desc := fmt.Sprintf("%s/%s %s %s engine=%q term=%d payment=%s account=%s",
		rec.Provider, rec.Service, rec.Region, rec.ResourceType, rec.Engine, rec.Term, rec.Payment, derefStringOrEmpty(rec.CloudAccountID))
	if rec.AvailabilityZone != "" {
		desc += " az=" + rec.AvailabilityZone
	}
	// A malformed Details blob is reported by the caller's own decode; the
	// description just omits what it cannot read.
	if ci, err := rec.ComputeIdentity(); err == nil && !ci.IsZero() {
		desc += fmt.Sprintf(" platform=%q tenancy=%q", ci.Platform, ci.Tenancy)
	}
	return desc
}
