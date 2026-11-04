package config

import (
	"slices"

	"github.com/google/uuid"
)

// ambientAccountKey is the account_key of rows with no registered account.
var ambientAccountKey = uuid.Nil.String()

// carryAzureHold keeps the Azure pricing hold alive for as long as the held
// rows are. Every upsert replaces last_collection_partial, but an account's
// Advisor rows stay at their old price until a later cycle re-prices them, so
// the previous hold (prev, read under the state row lock) is merged into this
// cycle's partial: accounts re-priced now (in collects) drop out, newly held
// ones are added, the rest carry over. That covers another outage, an outright
// Azure failure and a run that skips Azure.
//
// A hold lasts only while stored Azure rows it protects exist: stored holds the
// account_key of every provider=azure row, read in the same transaction. A
// deleted account's rows cascade and pruned rows disappear, which ends the
// hold; a disabled account keeps its rows, so it stays held. A hold with no
// account IDs (an unresolved ambient identity) covers the ambient rows, keyed
// by the nil UUID, and carries until an ambient Azure collection succeeds or
// none remain. Once the hold also names accounts, the ambient part is recorded
// as the nil UUID in the held IDs, so it outlives the named accounts' re-pricing.
func carryAzureHold(prev, partial *RecommendationsCollectionPartial, collects []SuccessfulCollect, stored map[string]bool) *RecommendationsCollectionPartial {
	held := previousAzureHold(prev)
	if held == nil {
		return partial
	}
	carried, unresolved := stillHeld(held, collects, stored)
	if len(carried) == 0 && !unresolved {
		return partial
	}
	if unresolved {
		carried = append(carried, ambientAccountKey)
	}
	return mergeCarriedHold(partial, carried)
}

// mergeCarriedHold adds the carried account IDs and the pricing_unavailable
// reason to partial's Azure entry, creating the entry when this cycle had none.
// An entry that already records the pricing reason with no account IDs is this
// cycle's ambient outage; it gains the nil UUID so it survives a named hold.
func mergeCarriedHold(partial *RecommendationsCollectionPartial, carried []string) *RecommendationsCollectionPartial {
	if partial == nil {
		partial = &RecommendationsCollectionPartial{}
	}
	idx := slices.IndexFunc(partial.Providers, func(p PartialProviderSweep) bool { return p.Provider == "azure" })
	if idx < 0 {
		partial.Providers = append(partial.Providers, PartialProviderSweep{Provider: "azure", Reasons: map[string]int{}})
		idx = len(partial.Providers) - 1
	}
	sweep := &partial.Providers[idx]
	ownAmbient := false
	if sweep.Reasons == nil {
		sweep.Reasons = map[string]int{}
	}
	if sweep.Reasons[PartialReasonPricingUnavailable] == 0 {
		sweep.Reasons[PartialReasonPricingUnavailable] = 1
	} else if len(sweep.HeldAccountIDs) == 0 {
		carried = append(carried, ambientAccountKey)
		ownAmbient = true
	}
	before := len(sweep.HeldAccountIDs)
	held := slices.Concat(sweep.HeldAccountIDs, carried)
	slices.Sort(held)
	sweep.HeldAccountIDs = slices.Compact(held)
	// Each newly held account adds to the incomplete count; this cycle's own
	// ambient outage was already counted.
	sweep.IncompleteAccounts += len(sweep.HeldAccountIDs) - before
	if ownAmbient {
		sweep.IncompleteAccounts--
	}
	return partial
}

// previousAzureHold returns the Azure entry that records a pricing hold, or nil.
func previousAzureHold(partial *RecommendationsCollectionPartial) *PartialProviderSweep {
	if partial == nil {
		return nil
	}
	for i := range partial.Providers {
		sweep := &partial.Providers[i]
		if sweep.Provider == "azure" && sweep.Reasons[PartialReasonPricingUnavailable] > 0 {
			return sweep
		}
	}
	return nil
}

// stillHeld returns the previously held accounts this cycle did not re-price
// that still have stored Azure rows, and whether an account-less (ambient) hold is still
// unresolved.
func stillHeld(prev *PartialProviderSweep, collects []SuccessfulCollect, stored map[string]bool) (carried []string, unresolved bool) {
	repriced := map[string]bool{}
	ambientRepriced := false
	for _, sc := range collects {
		switch {
		case sc.Provider != "azure":
		case sc.CloudAccountID == nil:
			ambientRepriced = true
			repriced[ambientAccountKey] = true
		default:
			repriced[*sc.CloudAccountID] = true
		}
	}
	for _, id := range prev.HeldAccountIDs {
		if !repriced[id] && stored[id] {
			carried = append(carried, id)
		}
	}
	return carried, len(prev.HeldAccountIDs) == 0 && !ambientRepriced && stored[ambientAccountKey]
}
