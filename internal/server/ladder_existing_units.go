package server

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"

	"github.com/google/uuid"

	pkgladder "github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
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
// safe upper bound, so the gap cannot be sized. The run is persisted as a
// Hold that buys nothing and says why (planMissingRateHold), rather than
// guessing a rate or failing on every tick (owner decision D3 on #1238).

// ladderMissingRate is one layer whose existing or expiring commitment needs a
// rate the provider did not return. Amounts are commitment USD/hr.
type ladderMissingRate struct {
	layer              pkgladder.LayerType
	existing, expiring *float64
}

// ladderLayerStatesOnDemand returns a copy of states with ExistingUSDPerHour
// and ExpiringUSDPerHour converted from commitment dollars to
// on-demand-equivalent dollars by each layer's rate. The input map and its
// values are not modified. Nil amounts stay nil for Allocate to reject.
//
// When any layer holds commitment but has no rate, it returns a nil map and
// every such layer (sorted by layer) instead: the gap cannot be sized, and the
// caller must not hand a partial conversion to Allocate. Invalid provider data
// (a rate outside (0, 1], a negative or non-finite amount) is an error.
func ladderLayerStatesOnDemand(
	states map[pkgladder.LayerType]pkgladder.LayerState,
	rates map[pkgladder.LayerType]float64,
) (map[pkgladder.LayerType]pkgladder.LayerState, []ladderMissingRate, error) {
	out := make(map[pkgladder.LayerType]pkgladder.LayerState, len(states))
	var missing []ladderMissingRate
	for _, layer := range slices.Sorted(maps.Keys(states)) {
		st := states[layer]
		rate, hasRate := rates[layer]
		if !hasRate && holdsCommitment(st) {
			if err := validateCommitments(st); err != nil {
				return nil, nil, fmt.Errorf("layer %s: %w", layer, err)
			}
			missing = append(missing, ladderMissingRate{layer: layer, existing: st.ExistingUSDPerHour, expiring: st.ExpiringUSDPerHour})
			continue
		}
		od, err := layerStateOnDemand(st, rate)
		if err != nil {
			return nil, nil, fmt.Errorf("layer %s: %w", layer, err)
		}
		out[layer] = od
	}
	if len(missing) > 0 {
		return nil, missing, nil
	}
	return out, nil, nil
}

// validateCommitments checks the amounts of a layer that cannot be converted,
// so bad provider data still fails the run instead of reaching a Hold message.
func validateCommitments(st pkgladder.LayerState) error {
	if _, err := commitmentToOnDemand("existing", st.ExistingUSDPerHour, big.NewRat(1, 1), true); err != nil {
		return err
	}
	_, err := commitmentToOnDemand("expiring", st.ExpiringUSDPerHour, big.NewRat(1, 1), false)
	return err
}

// layerStateOnDemand converts one layer's existing and expiring commitment.
// A layer with no commitment (both amounts nil or zero) needs no rate.
func layerStateOnDemand(st pkgladder.LayerState, rate float64) (pkgladder.LayerState, error) {
	if !holdsCommitment(st) {
		return st, nil
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

// holdsCommitment reports whether a layer has existing or expiring commitment
// that must be converted (and so needs a rate).
func holdsCommitment(st pkgladder.LayerState) bool {
	return !isZeroOrNil(st.ExistingUSDPerHour) || !isZeroOrNil(st.ExpiringUSDPerHour)
}

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

// ladderHoldMissingDiscountRate is the plan reason_code of a Hold-only run
// whose gap could not be sized because existing coverage has no discount rate.
// A Hold from Allocate (target met, baseline unavailable, ...) has no
// reason_code.
const ladderHoldMissingDiscountRate = "missing_discount_rate"

// planMissingRateHold builds the run persisted when existing coverage on one
// or more layers has no rate (see ladderLayerStatesOnDemand). Allocate never
// runs, so no rate is guessed and nothing is bought: one Hold per layer says
// which layer could not be sized and why. Existing and gap stay nil
// (unmeasured, never 0), so the amend ceiling refuses any raise against this
// run. The in-flight rows are not needed.
func (p *ladderRunPlanner) planMissingRateHold() (*config.LadderRunDB, []config.LadderTrancheDB, error) {
	actions := make([]pkgladder.PlannedAction, 0, len(p.missing))
	for _, m := range p.missing {
		actions = append(actions, pkgladder.PlannedAction{
			Action:      pkgladder.ActionHold,
			Layer:       m.layer,
			Rationale:   m.rationale(p.term, p.paymentOpt),
			DataSources: []string{dataSourceAWSCostExplorer},
		})
	}
	plan := &pkgladder.LadderPlan{
		Scope:            p.engineCfg.Scope,
		GeneratedAt:      p.now,
		TargetUSDPerHour: ladderPlanTarget(p.baseline, p.engineCfg.TargetCoveragePct),
		Actions:          actions,
		Baseline:         p.baseline,
	}
	p.holds = len(actions)
	return p.buildRows(uuid.New().String(), plan, &pkgladder.TrancheResult{})
}

// rationale is the Hold's human message. It must read as "not sized yet", never
// as "covered", so it is not mistaken for a target-met Hold.
func (m ladderMissingRate) rationale(term pkgladder.Term, payment pkgladder.PaymentOption) string {
	return fmt.Sprintf("Can't size this yet because layer %s holds existing coverage (%s commitment USD/hr, %s expiring) "+
		"and the provider returned no discount rate for it at %s %s, so that coverage cannot be converted to on-demand-equivalent dollars. "+
		"Nothing is bought this run: the rate is never estimated or assumed 1:1. The ladder sizes the gap once a rate is available.",
		m.layer, fmtFloatPtr(m.existing), fmtFloatPtr(m.expiring), term, payment)
}
