package config

import (
	"fmt"
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
		rate, ok := new(big.Rat).SetString(*tr.CommitmentRate)
		if !ok || rate.Sign() <= 0 || rate.Cmp(big.NewRat(1, 1)) > 0 {
			return nil, fmt.Errorf("stored commitment rate must be a rational in (0, 1]")
		}
		canonical := rate.RatString()
		return &canonical, nil
	default:
		return nil, fmt.Errorf("unknown ladder amount basis %q", *tr.AmountBasis)
	}
}
