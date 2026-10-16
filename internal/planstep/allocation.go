package planstep

import (
	"fmt"
	"math"
	"math/big"
	"strconv"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

func (r *ramp) validateCounts(counts ...int) error {
	if r.mode == config.RampAllocationEqualCount {
		for _, count := range counts {
			if count < 0 {
				return fmt.Errorf("negative allocation count: %d", count)
			}
		}
	}
	return nil
}

func decimalRatio(v float64) (*big.Rat, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return nil, fmt.Errorf("invalid allocation quantity: %v", v)
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(v, 'g', -1, 64))
	if !ok {
		return nil, fmt.Errorf("invalid allocation decimal: %v", v)
	}
	return r, nil
}

func (r *ramp) equalCountShare(coverage float64, step int) (*big.Rat, error) {
	share, err := decimalRatio(coverage)
	if err != nil {
		return nil, err
	}
	return share.Mul(share, big.NewRat(int64(min(step, r.count)), int64(100*r.count))), nil
}

func floorAllocation(v *big.Rat) (int, error) {
	n := new(big.Int).Quo(v.Num(), v.Denom())
	if !n.IsInt64() || n.Sign() < 0 || n.Cmp(big.NewInt(math.MaxInt)) > 0 {
		return 0, fmt.Errorf("allocation exceeds whole-unit range")
	}
	return int(n.Int64()), nil
}

func exactSPHourly(hourly float64, share *big.Rat) (float64, error) {
	h, err := decimalRatio(hourly)
	if err != nil {
		return 0, err
	}
	h.Mul(h, share).Mul(h, big.NewRat(100, 1))
	cents := new(big.Int).Quo(h.Num(), h.Denom())
	sized, _ := new(big.Rat).SetFrac(cents, big.NewInt(100)).Float64()
	for _, text := range []string{strconv.FormatFloat(sized, 'g', -1, 64), fmt.Sprintf("%.2f", sized)} {
		decimal, ok := new(big.Rat).SetString(text)
		if !ok || decimal.Mul(decimal, big.NewRat(100, 1)).Cmp(new(big.Rat).SetInt(cents)) != 0 {
			return 0, fmt.Errorf("SP allocation cannot represent whole cents")
		}
	}
	return sized, nil
}
