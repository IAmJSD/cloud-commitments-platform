package config

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

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

// ladderAmountScale is the fractional-digit scale of
// ladder_tranches.amount_usd_hr (NUMERIC(20,6), migration 000081).
const ladderAmountScale = 6

// LadderAmountFloor floors an exact planned tranche amount to the six decimal
// places ladder_tranches.amount_usd_hr stores, returning the exact floored
// decimal. A positive amount below 0.000001 floors to zero; callers must not
// persist that as a $0 tranche.
func LadderAmountFloor(amount *big.Rat) *big.Rat {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(ladderAmountScale), nil)
	floored := new(big.Int).Mul(amount.Num(), scale)
	floored.Div(floored, amount.Denom()) // Euclidean division: floor for the positive denominator
	return new(big.Rat).SetFrac(floored, scale)
}

// LadderAmountFloat converts an exact planned tranche amount to the float64 a
// tranche row stores, flooring it to the column's six decimal places (see
// LadderAmountFloor). The go precision contract (pkg/ladder
// LadderCapability.PurchaseLayer, go#354) floors provider commitments and
// forbids callers rounding a *big.Rat up on the way to float64; nearest-float
// conversion can do exactly that just below a decimal boundary. Flooring at six
// decimals keeps a provider's five-decimal truncation identical to truncating
// the exact amount and keeps an amount below a provider minimum below it.
//
// Below 1e9 USD/hr the floored value has at most 15 significant digits, so the
// result's shortest decimal form is the floored value itself and the store
// writes it without any further rounding. Above about 1e9 USD/hr the nearest
// float64 of the floored decimal can round it up by less than one float64 ulp,
// and nothing here refuses that; it is unreachable under the provider's 1e6
// USD/hr cap.
func LadderAmountFloat(amount *big.Rat) float64 {
	f, _ := LadderAmountFloor(amount).Float64()
	return f
}

// ladderAmountText is the exact decimal insertLadderTranchesTx writes for a
// tranche amount. Sent as a float, the amount's shortest decimal would be
// rounded half-up to the column's scale by Postgres, which can carry it across
// a provider's five-decimal truncation boundary or up to a provider minimum.
// An amount the column cannot hold exactly is refused instead, so every stored
// amount reads back as the same float64.
func ladderAmountText(amount float64) (string, error) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return "", fmt.Errorf("amount_usd_hr must be finite, got %v", amount)
	}
	text := strconv.FormatFloat(amount, 'f', -1, 64)
	if _, fraction, ok := strings.Cut(text, "."); ok && len(fraction) > ladderAmountScale {
		return "", fmt.Errorf("amount_usd_hr %s has more than %d decimal places and would be rounded by the column; floor it with LadderAmountFloat", text, ladderAmountScale)
	}
	return text, nil
}
