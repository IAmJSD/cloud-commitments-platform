//go:build integration

package config

import (
	"context"
	"math"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/stretchr/testify/require"
)

// Issue #1182: tranche amounts are float64 stored in NUMERIC(20,6). Against a
// real Postgres, every amount the store accepts must read back as the same
// float64 through the claim path a tranche executor will use, so the go
// precision contract (range check, then five-decimal truncation; see
// spCommitmentWire) gives the same answer for the stored amount as for the
// written one, and for planner amounts, as for the exact plan.
func TestLadderTrancheAmount_RoundTripsProviderPrecisionContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfig(ctx, t, store)
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	due := asOf.Add(-time.Minute)

	written := map[string]float64{}
	planned := map[string]*big.Rat{}
	var rows []LadderTrancheDB
	add := func(amount float64, plan *big.Rat) {
		tr := lifecycleTranche(cfgID, amount, ladder.TrancheStatusScheduled, due)
		written[tr.ID], planned[tr.ID] = amount, plan
		rows = append(rows, tr)
	}
	// Contract edges the column holds exactly, including values float64
	// cannot represent exactly (0.1, 0.29, 999999.99999) and the range ends.
	for _, amount := range []float64{
		0.001, 0.00101, 0.000999, 0.1, 0.29, 0.12345, 1.00001, 123456.78901,
		999999.99999, 999999.999999, 1000000, 1000000.000001, 99999999.999999,
	} {
		add(amount, nil)
	}
	// Exact planner amounts finer than the column, floored by LadderAmountFloat.
	for _, text := range []string{
		"1/3", "12345999999999999/100000000000000000", "1234567/10000000",
		"9995/10000000", "9999999999999/10000000", "1000000",
	} {
		plan, ok := new(big.Rat).SetString(text)
		require.True(t, ok, text)
		add(LadderAmountFloat(plan), plan)
	}
	require.NoError(t, store.SaveLadderTranches(ctx, rows))

	var calls atomic.Int64
	claimed, err := store.ClaimDueLadderTranches(ctx, cfgID, asOf, len(rows), newExecCreator(ctx, t, store, &calls))
	require.NoError(t, err)
	require.Len(t, claimed, len(rows))
	for i := range claimed {
		tr := &claimed[i]
		amount, ok := written[tr.ID]
		require.True(t, ok, tr.ID)
		require.Equal(t, math.Float64bits(amount), math.Float64bits(tr.AmountUSDHr), "stored %v read back as %v", amount, tr.AmountUSDHr)
		var text string
		require.NoError(t, store.db.QueryRow(ctx, `SELECT amount_usd_hr::text FROM ladder_tranches WHERE id = $1`, tr.ID).Scan(&text))
		stored, _ := new(big.Rat).SetString(text)
		wantText, err := ladderAmountText(amount)
		require.NoError(t, err)
		decimal, _ := new(big.Rat).SetString(wantText)
		require.Zero(t, decimal.Cmp(stored), "column holds %s for %s", text, wantText)

		want, wantOK := spCommitmentWire(amount)
		if plan := planned[tr.ID]; plan != nil {
			require.LessOrEqual(t, stored.Cmp(plan), 0, "stored %s exceeds planned %s", text, plan.RatString())
			want, wantOK = spCommitmentWireExact(plan)
		}
		got, gotOK := spCommitmentWire(tr.AmountUSDHr)
		requireSameWire(t, want, wantOK, got, gotOK, text)
	}

	// An amount the column would round half-up is refused, and its batch
	// rolls back, instead of being stored as a different amount.
	for _, amount := range []float64{0.1234599, 0.0009995, 999999.9999999, 1.0 / 3, math.NaN()} {
		first := lifecycleTranche(cfgID, 1, ladder.TrancheStatusScheduled, due)
		bad := lifecycleTranche(cfgID, amount, ladder.TrancheStatusScheduled, due)
		require.Error(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{first, bad}), "%v", amount)
		var count int
		require.NoError(t, store.db.QueryRow(ctx, `SELECT count(*) FROM ladder_tranches`).Scan(&count))
		require.Equal(t, len(rows), count, "refused batch for %v must roll back", amount)
	}
}
