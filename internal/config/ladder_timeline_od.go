package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// ErrLadderTimelineRate marks a stored amount basis or rate that cannot be
// converted; callers must not fall back to the native amount.
var ErrLadderTimelineRate = errors.New("ladder timeline row has an unreadable amount basis or commitment rate")

var ladderMicro = big.NewInt(1_000_000)

// ladderTimelineODMicro returns the row's on-demand-equivalent amount in
// micro-dollars, rounded UP so a displayed value never understates OD.
func ladderTimelineODMicro(amount string, basis *LadderAmountBasis, rate *string) (*big.Int, error) {
	// A NULL basis (rows from before 000121) is ASSUMED on-demand-equivalent,
	// as on the amend path; it is not verified.
	tr := assumeOnDemandIfUnknown(&LadderTrancheDB{AmountBasis: basis, CommitmentRate: rate})
	od, err := nettingOnDemand(amount, tr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLadderTimelineRate, err)
	}
	scaled := new(big.Rat).Mul(od, new(big.Rat).SetInt(ladderMicro))
	q, m := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q, nil
}

func formatLadderMicro(v *big.Int) string {
	q, r := new(big.Int).QuoRem(v, ladderMicro, new(big.Int))
	return fmt.Sprintf("%s.%06d", q, r.Int64())
}

// fillLadderTimelineOnDemand sets od_usd_hr on the page rows and the scope
// total. Both come from the one statement that paged the rows, and the total
// is the sum of the per-row rounded values so it equals what a client adds up.
func fillLadderTimelineOnDemand(page *LadderTimelinePage, scopeRaw []byte) error {
	var scope [][3]*string
	if err := json.Unmarshal(scopeRaw, &scope); err != nil {
		return fmt.Errorf("decode ladder timeline scope: %w", err)
	}
	total := new(big.Int)
	for _, row := range scope {
		if row[0] == nil {
			return fmt.Errorf("%w: missing amount", ErrLadderTimelineRate)
		}
		var basis *LadderAmountBasis
		if row[1] != nil {
			b := LadderAmountBasis(*row[1])
			basis = &b
		}
		micro, err := ladderTimelineODMicro(*row[0], basis, row[2])
		if err != nil {
			return err
		}
		total.Add(total, micro)
	}
	page.TotalODUSDHr = formatLadderMicro(total)
	for i := range page.Events {
		e := &page.Events[i]
		micro, err := ladderTimelineODMicro(e.AmountUSDHr, e.AmountBasis, e.CommitmentRate)
		if err != nil {
			return err
		}
		e.ODUSDHr = formatLadderMicro(micro)
	}
	return nil
}
