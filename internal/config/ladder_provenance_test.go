package config

import (
	"math"
	"math/big"
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
