package config

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLadderNetting_ExactRatesAndBoundary(t *testing.T) {
	od, native := LadderAmountOnDemand, LadderAmountCommitment
	third, half, bad, above := "1/3", "1/2", "bad", "2"
	for _, tc := range []struct {
		amount string
		basis  *LadderAmountBasis
		rate   *string
		want   string
	}{
		{"55", &od, nil, "55"}, {"3.5", &od, nil, "7/2"},
		{"1", &native, &third, "3"}, {"1", &native, &half, "2"}, {"0", &native, &half, "0"},
		{"0", nil, nil, ""}, {"1", &native, nil, ""}, {"1", &od, &half, ""},
		{"1", &native, &bad, ""}, {"1", &native, &above, ""}, {"-1", &od, nil, ""}, {"NaN", &od, nil, ""},
	} {
		value, err := nettingOnDemand(tc.amount, &LadderTrancheDB{AmountBasis: tc.basis, CommitmentRate: tc.rate})
		if tc.want == "" {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, tc.want, value.RatString())
	}
	for _, text := range []string{"55", "3", "7/2", "55/3", "1/10"} {
		exact, ok := new(big.Rat).SetString(text)
		require.True(t, ok)
		value, err := nettingFloat(exact)
		require.NoError(t, err)
		require.GreaterOrEqual(t, new(big.Rat).SetFloat64(value).Cmp(exact), 0)
		require.Less(t, new(big.Rat).SetFloat64(math.Nextafter(value, math.Inf(-1))).Cmp(exact), 0)
		if text == "55" || text == "3" || text == "7/2" {
			nearest, _ := exact.Float64()
			require.Equal(t, nearest, value)
		}
	}
	_, err := nettingFloat(new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 2048)))
	require.Error(t, err)
}

func TestLadderInFlightOnDemand_RoundsAwayFromUnderstating(t *testing.T) {
	native, third := LadderAmountCommitment, "1/3"
	tr := &LadderTrancheDB{AmountBasis: &native, CommitmentRate: &third}
	row, err := newLadderInFlightRow("1.000001", tr)
	require.NoError(t, err)
	exact := big.NewRat(3000003, 1000000)
	require.LessOrEqual(t, new(big.Rat).SetFloat64(row.Rate).Cmp(big.NewRat(1, 3)), 0, "rate rounds down")
	require.GreaterOrEqual(t, new(big.Rat).SetFloat64(row.CommitmentUSDPerHour).Cmp(big.NewRat(1000001, 1000000)), 0, "commitment rounds up")
	row.exactOD = nil // consumers that only see the floats must still not understate
	total, err := LadderInFlightOnDemand([]LadderInFlightRow{row})
	require.NoError(t, err)
	require.GreaterOrEqual(t, new(big.Rat).SetFloat64(total).Cmp(exact), 0)
}

func TestLadderInFlightOnDemand_RejectsUnusableRows(t *testing.T) {
	for _, row := range []LadderInFlightRow{
		{CommitmentUSDPerHour: 1, Rate: 0}, {CommitmentUSDPerHour: 1, Rate: 1.5}, {CommitmentUSDPerHour: 1, Rate: math.NaN()},
		{CommitmentUSDPerHour: -1, Rate: 1}, {CommitmentUSDPerHour: math.Inf(1), Rate: 1},
	} {
		_, err := LadderInFlightOnDemand([]LadderInFlightRow{row})
		require.Error(t, err, "%+v", row)
	}
}

func TestAssumeOnDemandIfUnknown_OnlyForUnprovenBasis(t *testing.T) {
	assumed := assumeOnDemandIfUnknown(&LadderTrancheDB{})
	require.NotNil(t, assumed.AmountBasis)
	require.Equal(t, LadderAmountOnDemand, *assumed.AmountBasis)
	rate := "1/2"
	orphan := &LadderTrancheDB{CommitmentRate: &rate}
	require.Same(t, orphan, assumeOnDemandIfUnknown(orphan), "a rate without a basis is corrupt, not legacy")
	_, err := nettingOnDemand("1", &LadderTrancheDB{})
	require.Error(t, err, "netting still refuses an unknown basis")
}

func TestNewLadderInFlightRow_FloatFieldsFeedAllocate(t *testing.T) {
	od, native := LadderAmountOnDemand, LadderAmountCommitment
	legacy, err := newLadderInFlightRow("12.5", &LadderTrancheDB{AmountBasis: &od})
	require.NoError(t, err)
	require.Equal(t, 1.0, legacy.Rate)
	require.Equal(t, 12.5, legacy.CommitmentUSDPerHour)

	// Tight bound: the rate is the largest float not above the exact rate.
	for _, text := range []string{"1/10", "1/3", "1/2"} {
		exact, ok := new(big.Rat).SetString(text)
		require.True(t, ok)
		row, rowErr := newLadderInFlightRow("1", &LadderTrancheDB{AmountBasis: &native, CommitmentRate: &text})
		require.NoError(t, rowErr, text)
		require.LessOrEqual(t, new(big.Rat).SetFloat64(row.Rate).Cmp(exact), 0, text)
		require.Greater(t, new(big.Rat).SetFloat64(math.Nextafter(row.Rate, 2)).Cmp(exact), 0, text)
	}

	one := "1"
	row, err := newLadderInFlightRow("3", &LadderTrancheDB{AmountBasis: &native, CommitmentRate: &one})
	require.NoError(t, err)
	require.Equal(t, 1.0, row.Rate)

	tiny := "1/1" + strings.Repeat("0", 400)
	row, err = newLadderInFlightRow("1", &LadderTrancheDB{AmountBasis: &native, CommitmentRate: &tiny})
	require.NoError(t, err)
	require.Equal(t, math.SmallestNonzeroFloat64, row.Rate, "a rate below float range must not become 0")
}

// spCommitmentWire mirrors cloud-commitments-go
// providers/aws/internal/spcommitment.Normalize at the version go.mod pins
// (go#403, contract at pkg/ladder/capability.go): refuse an amount outside
// [0.001, 1000000] USD/hr, else truncate its shortest decimal to five places.
// ok=false is a refusal; the returned rat is the submitted commitment.
func spCommitmentWire(amount float64) (*big.Rat, bool) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0.001 || amount > 1000000 {
		return nil, false
	}
	whole, fraction, _ := strings.Cut(strconv.FormatFloat(amount, 'f', -1, 64), ".")
	if len(fraction) > 5 {
		fraction = fraction[:5]
	}
	wire, _ := new(big.Rat).SetString(whole + "." + fraction + "0")
	return wire, true
}

// spCommitmentWireExact is the commitment the contract would submit for the
// exact planned amount: the same range check, then a floor to five places.
func spCommitmentWireExact(amount *big.Rat) (*big.Rat, bool) {
	if amount.Cmp(big.NewRat(1, 1000)) < 0 || amount.Cmp(big.NewRat(1000000, 1)) > 0 {
		return nil, false
	}
	scaled := new(big.Int).Mul(amount.Num(), big.NewInt(100000))
	return new(big.Rat).SetFrac(scaled.Div(scaled, amount.Denom()), big.NewInt(100000)), true
}

func requireSameWire(t *testing.T, want *big.Rat, wantOK bool, got *big.Rat, gotOK bool, msg string) {
	t.Helper()
	require.Equal(t, wantOK, gotOK, "%s: provider refusal must not change", msg)
	if wantOK {
		require.Zero(t, want.Cmp(got), "%s: submitted commitment %s, want %s", msg, got.FloatString(6), want.FloatString(6))
	}
}

// Issue #1182: a planned amount is floored to the six-decimal column, never
// rounded up, so the provider submits what it would for the exact amount.
func TestLadderAmountFloat_FloorsToStoredScaleWithoutRoundingUp(t *testing.T) {
	for _, tc := range []struct{ planned, want string }{
		{"1/3", "0.333333"},
		{"12345999999999999/100000000000000000", "0.123459"}, // nearest float64 is 0.12346
		{"1234567/10000000", "0.123456"},
		{"9995/10000000", "0.000999"},               // half-up would reach the 0.001 minimum
		{"9999999999999/10000000", "999999.999999"}, // half-up would reach 1000000
		{"99999999999/100000", "999999.99999"},
		{"1000000", "1000000"},
		{"1/10", "0.1"},
		{"1/1000", "0.001"},
		{"1/10000000", "0"},
	} {
		planned, ok := new(big.Rat).SetString(tc.planned)
		require.True(t, ok, tc.planned)
		got := LadderAmountFloat(planned)
		text, err := ladderAmountText(got)
		require.NoError(t, err, tc.planned)
		require.Equal(t, tc.want, text, tc.planned)
		stored, _ := new(big.Rat).SetString(text)
		require.LessOrEqual(t, stored.Cmp(planned), 0, "%s: stored amount must not exceed the plan", tc.planned)
		want, wantOK := spCommitmentWireExact(planned)
		wire, wireOK := spCommitmentWire(got)
		requireSameWire(t, want, wantOK, wire, wireOK, tc.planned)
	}
}

// Issue #1182: the store refuses an amount the NUMERIC(20,6) column would
// round rather than letting Postgres round it half-up.
func TestLadderAmountText_RefusesAmountsTheColumnWouldRound(t *testing.T) {
	for amount, want := range map[float64]string{
		0.001: "0.001", 1000000: "1000000", 999999.99999: "999999.99999",
		999999.999999: "999999.999999", 0.1: "0.1", 0.29: "0.29", 12.5: "12.5",
	} {
		got, err := ladderAmountText(amount)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, amount := range []float64{0.1234599, 0.30000000000000004 /* float64 0.1+0.2 */, 1.0 / 3, 0.0009995, 999999.9999999, math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := ladderAmountText(amount)
		require.Error(t, err, "%v", amount)
	}
}
