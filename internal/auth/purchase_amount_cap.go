package auth

import (
	"fmt"
	"math"
)

// MaxPurchaseAmount (spend cap) semantics shared by enforcement
// (matchConstraints), the grant ceiling (amountCovers) and the group and
// API-key write paths. Zero is the unset zero-value and means "no cap"; a
// negative or NaN cap is malformed and fails closed everywhere (issue #83).

// matchPurchaseAmountConstraint checks if requested amount is within permitted limit.
//
// Zero is the unset zero-value and means "no cap". A negative (or NaN) cap is
// malformed spend-cap data, not an absent one, so it denies every request
// rather than falling through to "no cap" (issue #83). Write paths refuse such
// a value (validateMaxPurchaseAmount), but rows already stored that way must
// still fail closed here.
//
// When a cap applies, a malformed REQUESTED amount (negative, NaN or +/-Inf,
// see invalidRequestedAmount) is denied too: NaN compares false against the
// cap and a negative amount sits below it, so either would otherwise pass.
// reqMax is computed server-side, so this is defense in depth. With no cap the
// amount dimension is unrestricted and the request is allowed whatever its
// amount, matching a permission with no constraints at all
// (checkPermissionConstraints never calls the matcher for one).
func matchPurchaseAmountConstraint(permMax, reqMax float64) bool {
	if invalidPurchaseAmountCap(permMax) {
		return false
	}
	if permMax == 0 {
		return true
	}
	return !invalidRequestedAmount(reqMax) && reqMax <= permMax
}

// invalidRequestedAmount reports whether a requested purchase amount cannot be
// a real amount: negative, NaN or infinite. Zero is valid (a request that
// carries no amount, e.g. a non-money permission check).
func invalidRequestedAmount(v float64) bool {
	return v < 0 || math.IsNaN(v) || math.IsInf(v, 0)
}

// invalidPurchaseAmountCap reports whether a stored MaxPurchaseAmount is
// malformed: negative, or NaN (which compares false against everything and
// would otherwise read as "no cap"). Shared by enforcement, the grant ceiling
// and the write-time validators so the three cannot disagree.
func invalidPurchaseAmountCap(v float64) bool {
	return v < 0 || math.IsNaN(v)
}

// validateMaxPurchaseAmount refuses a permission whose MaxPurchaseAmount is
// malformed (see invalidPurchaseAmountCap) at write time, so a negative cap is
// rejected with a 400 instead of being stored as a deny-all row. permIndex
// names the offending entry in the refusal.
func validateMaxPurchaseAmount(permIndex int, p Permission) error {
	if p.Constraints == nil || !invalidPurchaseAmountCap(p.Constraints.MaxPurchaseAmount) {
		return nil
	}
	return fmt.Errorf(
		"%w: entry %d (%s:%s) has an invalid max purchase amount %v; a spend cap must be zero (no cap) or positive",
		ErrInvalidPermission, permIndex, p.Action, p.Resource, p.Constraints.MaxPurchaseAmount)
}
