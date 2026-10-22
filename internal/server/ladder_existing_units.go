package server

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	pkgladder "github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
)

// Existing coverage units (#946).
//
// Providers report LayerState.ExistingUSDPerHour and ExpiringUSDPerHour in
// native commitment dollars per hour: the AWS provider sums
// SavingsPlan.Commitment for the Savings Plan layers and the amortized RI cost
// for the RI layer. Allocate compares them with an on-demand-equivalent (OD)
// target, and the pkg/ladder LayerState contract says callers must convert
// them first. With a commitment rate r in (0, 1], the commitment dollars paid
// per OD dollar covered:
//
//	od = commitment / r
//
// so 5 commitment $/hr at r = 0.5 covers 10 OD $/hr. The rate is today's
// new-purchase rate for the config's term and payment option, from
// GetCommitmentRates (option 2 on #946): DescribeSavingsPlans does not expose
// each plan's own rate, so plans bought at a different discount are
// approximated.
//
// Rounding favors under-buying. When the exact quotient is not a float64,
// existing is rounded up and expiring down, so the net existing coverage
// (existing - expiring) Allocate subtracts from the target is never
// understated.
//
// A layer with non-zero existing or expiring commitment and no rate cannot be
// converted. 1:1 would understate its coverage and over-buy, and no value is a
// safe upper bound, so the run fails instead of guessing.

// errNoRateForExistingCoverage marks a layer whose existing commitment cannot
// be converted to on-demand-equivalent dollars.
var errNoRateForExistingCoverage = errors.New("no commitment rate to convert existing commitment dollars to on-demand-equivalent dollars")

// ladderLayerStatesOnDemand returns a copy of states with ExistingUSDPerHour
// and ExpiringUSDPerHour converted from commitment dollars to
// on-demand-equivalent dollars by each layer's rate. The input map and its
// values are not modified. Nil amounts stay nil for Allocate to reject.
func ladderLayerStatesOnDemand(
	states map[pkgladder.LayerType]pkgladder.LayerState,
	rates map[pkgladder.LayerType]float64,
) (map[pkgladder.LayerType]pkgladder.LayerState, error) {
	out := make(map[pkgladder.LayerType]pkgladder.LayerState, len(states))
	for layer, st := range states {
		rate, hasRate := rates[layer]
		od, err := layerStateOnDemand(st, rate, hasRate)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", layer, err)
		}
		out[layer] = od
	}
	return out, nil
}

// layerStateOnDemand converts one layer's existing and expiring commitment.
// A layer with no commitment (both amounts nil or zero) needs no rate.
func layerStateOnDemand(st pkgladder.LayerState, rate float64, hasRate bool) (pkgladder.LayerState, error) {
	if isZeroOrNil(st.ExistingUSDPerHour) && isZeroOrNil(st.ExpiringUSDPerHour) {
		return st, nil
	}
	if !hasRate {
		return st, fmt.Errorf("%w (existing %s, expiring %s commitment USD/hr); refusing to assume 1:1",
			errNoRateForExistingCoverage, fmtFloatPtr(st.ExistingUSDPerHour), fmtFloatPtr(st.ExpiringUSDPerHour))
	}
	r, err := commitmentRateRat(rate)
	if err != nil {
		return st, err
	}
	existing, err := commitmentToOnDemand("existing", st.ExistingUSDPerHour, r, true)
	if err != nil {
		return st, err
	}
	expiring, err := commitmentToOnDemand("expiring", st.ExpiringUSDPerHour, r, false)
	if err != nil {
		return st, err
	}
	st.ExistingUSDPerHour, st.ExpiringUSDPerHour = existing, expiring
	return st, nil
}

func isZeroOrNil(v *float64) bool { return v == nil || *v == 0 }

func fmtFloatPtr(v *float64) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprintf("%g", *v)
}

// commitmentRateRat validates a provider rate the same way Allocate does:
// finite and in (0, 1].
func commitmentRateRat(rate float64) (*big.Rat, error) {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 || rate > 1 {
		return nil, fmt.Errorf("commitment rate %g must be in (0, 1]", rate)
	}
	return new(big.Rat).SetFloat64(rate), nil
}

// commitmentToOnDemand returns commitment/rate as a float64, rounded up when
// roundUp is set and down otherwise. Nil stays nil.
func commitmentToOnDemand(name string, commitment *float64, rate *big.Rat, roundUp bool) (*float64, error) {
	if commitment == nil {
		return nil, nil
	}
	c := ratFromFloat(*commitment)
	if c == nil || c.Sign() < 0 {
		return nil, fmt.Errorf("%s commitment %g USD/hr must be finite and non-negative", name, *commitment)
	}
	exact := c.Quo(c, rate)
	od, _ := exact.Float64()
	if math.IsInf(od, 0) {
		return nil, fmt.Errorf("%s on-demand-equivalent amount exceeds float64", name)
	}
	if cmp := new(big.Rat).SetFloat64(od).Cmp(exact); roundUp && cmp < 0 {
		od = math.Nextafter(od, math.Inf(1))
	} else if !roundUp && cmp > 0 {
		od = math.Nextafter(od, 0)
	}
	if math.IsInf(od, 0) {
		return nil, fmt.Errorf("%s on-demand-equivalent amount exceeds float64", name)
	}
	return &od, nil
}
