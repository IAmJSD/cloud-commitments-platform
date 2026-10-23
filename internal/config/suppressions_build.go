package config

import (
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
)

// suppressionBucket is the key BuildSuppressions aggregates on: the 6-tuple
// (account, provider, service, region, resource_type, engine) plus the rec's
// variant (availability zone, platform, tenancy; issue #816). unknownVariant
// marks a rec whose Details could not be read.
type suppressionBucket struct {
	accountID, provider, service, region, resourceType, engine string
	variant                                                    SuppressionVariant
	unknownVariant                                             bool
}

// suppressionBucketOf returns rec's bucket. A rec whose Details cannot be read
// gets no variant: its row is then a wildcard over the 6-tuple, the pre-#816
// behavior, which over-suppresses (under-buys) rather than leaving the
// purchased capacity on show. The purchase paths reject such recs before
// they get here (RecIdentityKey reads the same Details), so this is a
// fallback, not a path in normal use.
func suppressionBucketOf(rec *RecommendationRecord) suppressionBucket {
	accountID := ""
	if rec.CloudAccountID != nil {
		accountID = *rec.CloudAccountID
	}
	b := suppressionBucket{
		accountID:    accountID,
		provider:     rec.Provider,
		service:      rec.Service,
		region:       rec.Region,
		resourceType: rec.ResourceType,
		engine:       rec.Engine,
	}
	v, err := rec.SuppressionVariant()
	if err != nil {
		logging.Warnf("suppression for %s/%s/%s: %v; suppressing the whole 6-tuple", rec.Provider, rec.Region, rec.ResourceType, err)
		b.unknownVariant = true
		return b
	}
	b.variant = v
	return b
}

// BuildSuppressions converts the recs on an executePurchase request into
// purchase_suppressions rows, one per unique 6-tuple (account, provider,
// service, region, resource_type, engine) and variant (availability zone,
// platform, tenancy; issue #816), so a purchase of one zonal EC2 variant does
// not hide its sibling AZ, regional, platform or tenancy recs. Counts for
// duplicate keys within the same request are summed (defensive — the UI
// should collapse them client-side, but the backend doesn't trust that).
// Providers with a grace period of 0 (feature disabled) contribute no rows.
func BuildSuppressions(recs []RecommendationRecord, executionID string, cfg *GlobalConfig, now time.Time) []PurchaseSuppression {
	// Aggregate count per key before writing suppression rows so the
	// UNIQUE(execution_id, ...key) constraint can't fire from a request
	// that repeated the same key.
	agg := map[suppressionBucket]int{}
	order := []suppressionBucket{}
	for i := range recs {
		if recs[i].Count <= 0 {
			continue
		}
		k := suppressionBucketOf(&recs[i])
		if _, seen := agg[k]; !seen {
			order = append(order, k)
		}
		agg[k] += recs[i].Count
	}

	out := make([]PurchaseSuppression, 0, len(order))
	for i := range order {
		k := &order[i]
		graceDays := DefaultGracePeriodDays
		if cfg != nil {
			graceDays = cfg.GracePeriodFor(k.provider)
		}
		if graceDays <= 0 {
			continue // feature disabled for this provider
		}
		sup := PurchaseSuppression{
			ExecutionID:     executionID,
			AccountID:       k.accountID,
			Provider:        k.provider,
			Service:         k.service,
			Region:          k.region,
			ResourceType:    k.resourceType,
			Engine:          k.engine,
			SuppressedCount: agg[*k],
			ExpiresAt:       now.Add(time.Duration(graceDays) * 24 * time.Hour),
		}
		if !k.unknownVariant {
			v := k.variant
			sup.Variant = &v
		}
		out = append(out, sup)
	}
	return out
}
