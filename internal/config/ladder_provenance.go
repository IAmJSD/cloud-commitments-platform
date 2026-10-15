package config

import (
	"fmt"
	"math"
	"math/big"
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
