package auth

// Constraint-coverage comparators for the grant ceiling (group_ceiling.go):
// they decide whether a held permission's constraints are broad enough to
// cover a requested grant's, mirroring enforcement's semantics in
// matchConstraints.

// constraintsCover reports whether a permission constrained by held is broad
// enough to cover a grant constrained by req.
//
// Mirrors the enforcement-time semantics in matchConstraints: an empty list
// or a zero MaxPurchaseAmount means "no restriction on this dimension". So an
// unconstrained holder covers anything, while a constrained one requires the
// grant to name a subset of the same values and a cap no higher than its own.
// A nil req against a constrained held is a widening and is refused.
func constraintsCover(held, req *PermissionConstraints) bool {
	if held == nil {
		return true
	}
	if req == nil {
		req = &PermissionConstraints{}
	}
	return listCovers(held.AccountIDs, req.AccountIDs) &&
		listCovers(held.Providers, req.Providers) &&
		listCovers(held.Services, req.Services) &&
		listCovers(held.Regions, req.Regions) &&
		amountCovers(held.MaxPurchaseAmount, req.MaxPurchaseAmount)
}

// listCovers reports whether every value in req is permitted by held. An
// empty held list is "no restriction on this dimension" and covers anything;
// a non-empty held list requires req to be a NON-EMPTY subset, so a grant can
// never drop the restriction.
//
// Comparison is EXACT for every dimension. An earlier version normalised
// (trim + lower-case) and justified it as "matching matchAllRegionsConstraint"
// -- true for Regions only. AccountIDs, Providers and Services are enforced by
// containsAny, an exact case-sensitive lookup, so normalising made the ceiling
// LOOSER than enforcement there: a holder constrained to ["ACCT-1"] could
// grant ["acct-1"], a value they cannot themselves use.
//
// Being exact on Regions too is stricter than its enforcement, not looser, so
// it fails closed. A ceiling may exceed enforcement in strictness; it must
// never fall short.
func listCovers(held, req []string) bool {
	if len(held) == 0 {
		return true
	}
	if len(req) == 0 {
		return false
	}
	permitted := make(map[string]bool, len(held))
	for _, v := range held {
		permitted[v] = true
	}
	for _, v := range req {
		if !permitted[v] {
			return false
		}
	}
	return true
}

// amountCovers reports whether a grant capped at reqMax stays within a holder
// capped at heldMax. Zero means "no cap" on both sides (mirroring
// matchPurchaseAmountConstraint), so a capped holder cannot hand out an
// uncapped grant. A malformed (negative/NaN) held cap denies everything at
// enforcement, so it covers nothing here either (issue #83).
func amountCovers(heldMax, reqMax float64) bool {
	if invalidPurchaseAmountCap(heldMax) {
		return false
	}
	if heldMax == 0 {
		return true
	}
	return reqMax > 0 && reqMax <= heldMax
}
