package config

import (
	"fmt"
	"math"
	"math/big"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
)

type LadderAmountBasis string

const (
	LadderAmountOnDemand   LadderAmountBasis = "on_demand_equivalent"
	LadderAmountCommitment LadderAmountBasis = "commitment"
)

func trancheCommitmentRate(tr *LadderTrancheDB) (*string, error) {
	if tr.AmountBasis == nil {
		if tr.CommitmentRate != nil {
			return nil, fmt.Errorf("commitment rate requires a known amount basis")
		}
		return nil, nil
	}
	switch *tr.AmountBasis {
	case LadderAmountOnDemand:
		if tr.CommitmentRate != nil {
			return nil, fmt.Errorf("on-demand-equivalent amount must not have a commitment rate")
		}
		return nil, nil
	case LadderAmountCommitment:
		if tr.CommitmentRate == nil {
			return nil, fmt.Errorf("commitment amount requires its stored rate")
		}
		rate, err := parseStoredCommitmentRate(*tr.CommitmentRate)
		if err != nil {
			return nil, err
		}
		canonical := rate.RatString()
		return &canonical, nil
	default:
		return nil, fmt.Errorf("unknown ladder amount basis %q", *tr.AmountBasis)
	}
}

func parseStoredCommitmentRate(text string) (*big.Rat, error) {
	rate, ok := new(big.Rat).SetString(text)
	if !ok || rate.Sign() <= 0 || rate.Cmp(big.NewRat(1, 1)) > 0 {
		return nil, fmt.Errorf("stored commitment rate must be a rational in (0, 1]")
	}
	return rate, nil
}

func nettingOnDemand(amount string, tr *LadderTrancheDB) (*big.Rat, error) {
	canonical, err := trancheCommitmentRate(tr)
	if err != nil {
		return nil, err
	}
	if tr.AmountBasis == nil {
		return nil, fmt.Errorf("cannot net an unproven amount basis")
	}
	total, ok := new(big.Rat).SetString(amount)
	if !ok || total.Sign() < 0 {
		return nil, fmt.Errorf("netting amount must be finite and non-negative")
	}
	if canonical != nil {
		rate, err := parseStoredCommitmentRate(*canonical)
		if err != nil {
			return nil, err
		}
		total.Quo(total, rate)
	}
	return total, nil
}

func nettingFloat(total *big.Rat) (float64, error) {
	value, _ := total.Float64()
	if math.IsInf(value, 0) {
		return 0, fmt.Errorf("netting total exceeds float64")
	}
	// The old SDK accepts one float; never round already committed OD down.
	if new(big.Rat).SetFloat64(value).Cmp(total) < 0 {
		value = math.Nextafter(value, math.Inf(1))
	}
	if math.IsInf(value, 0) {
		return 0, fmt.Errorf("netting total exceeds float64")
	}
	return value, nil
}

// LadderInFlightRow is one netted tranche. Rate is rounded down and the
// commitment up, so CommitmentUSDPerHour/Rate never understates the exact
// on-demand-equivalent amount. Legacy on-demand rows carry Rate 1.
type LadderInFlightRow struct {
	Layer                ladder.LayerType
	CommitmentUSDPerHour float64
	Rate                 float64
	exactOD              *big.Rat // set by the store so legacy totals stay bit-identical to the exact sum
}

// assumeOnDemandIfUnknown covers only the amend path: rows from before 000121
// have no basis and #886 deliberately left them unknown, so the amend ceiling
// ASSUMES on-demand-equivalent (its behavior before the basis existed).
// Netting never calls this and keeps failing loud on an unknown basis.
func assumeOnDemandIfUnknown(tr *LadderTrancheDB) *LadderTrancheDB {
	if tr.AmountBasis != nil || tr.CommitmentRate != nil {
		return tr
	}
	assumed, basis := *tr, LadderAmountOnDemand
	assumed.AmountBasis = &basis
	return &assumed
}

func newLadderInFlightRow(amount string, tr *LadderTrancheDB) (LadderInFlightRow, error) {
	od, err := nettingOnDemand(amount, tr)
	if err != nil {
		return LadderInFlightRow{}, err
	}
	canonical, err := trancheCommitmentRate(tr)
	if err != nil {
		return LadderInFlightRow{}, err
	}
	rate := big.NewRat(1, 1)
	if canonical != nil {
		if rate, err = parseStoredCommitmentRate(*canonical); err != nil {
			return LadderInFlightRow{}, err
		}
	}
	commitment, _ := new(big.Rat).SetString(amount)
	commitmentUp, err := nettingFloat(commitment)
	if err != nil {
		return LadderInFlightRow{}, err
	}
	rateDown, _ := rate.Float64()
	if new(big.Rat).SetFloat64(rateDown).Cmp(rate) > 0 {
		rateDown = math.Nextafter(rateDown, 0)
	}
	rateDown = max(rateDown, math.SmallestNonzeroFloat64)
	return LadderInFlightRow{Layer: tr.LayerType, CommitmentUSDPerHour: commitmentUp, Rate: rateDown, exactOD: od}, nil
}

func (r LadderInFlightRow) onDemand() (*big.Rat, error) {
	if r.exactOD != nil {
		return r.exactOD, nil
	}
	commitment := new(big.Rat).SetFloat64(r.CommitmentUSDPerHour)
	if commitment == nil || commitment.Sign() < 0 || !(r.Rate > 0 && r.Rate <= 1) {
		return nil, fmt.Errorf("invalid in-flight row: commitment %v at rate %v", r.CommitmentUSDPerHour, r.Rate)
	}
	return commitment.Quo(commitment, new(big.Rat).SetFloat64(r.Rate)), nil
}

func sumLadderOnDemand(rows ...[]LadderInFlightRow) (*big.Rat, error) {
	total := new(big.Rat)
	for _, set := range rows {
		for _, r := range set {
			od, err := r.onDemand()
			if err != nil {
				return nil, err
			}
			total.Add(total, od)
		}
	}
	return total, nil
}

// LadderInFlightOnDemand is the rows' on-demand-equivalent total, summed
// exactly and rounded up to the next float only when the float is below it.
func LadderInFlightOnDemand(rows []LadderInFlightRow) (float64, error) {
	total, err := sumLadderOnDemand(rows)
	if err != nil {
		return 0, err
	}
	return nettingFloat(total)
}
