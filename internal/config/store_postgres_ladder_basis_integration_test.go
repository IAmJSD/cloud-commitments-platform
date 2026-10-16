//go:build integration

package config

// Amend-path behavior per stored amount basis (PR A). Commitment rows are
// seeded directly: no writer produces them yet.

import (
	"math"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/stretchr/testify/require"
)

func mustOnDemand(t *testing.T, rows []LadderInFlightRow) float64 {
	t.Helper()
	v, err := LadderInFlightOnDemand(rows)
	require.NoError(t, err)
	return v
}

func (f *ceilingFixture) setBasis(id, basis, rate string) {
	f.t.Helper()
	_, err := f.store.db.Exec(f.ctx, "UPDATE ladder_tranches SET amount_basis=NULLIF($2,''),commitment_rate=NULLIF($3,'') WHERE id=$1", id, basis, rate)
	require.NoError(f.t, err)
}

func seedNettingRows(t *testing.T, f *ceilingFixture, basis LadderAmountBasis, rate string, amounts ...string) {
	t.Helper()
	for _, a := range amounts {
		v, err := strconv.ParseFloat(a, 64)
		require.NoError(t, err)
		tr := lifecycleTranche(f.cfgID, v, ladder.TrancheStatusFired, lockTestAsOf.Add(-time.Hour))
		tr.AmountBasis = &basis
		if rate != "" {
			tr.CommitmentRate = &rate
		}
		require.NoError(t, f.store.SaveLadderTranches(f.ctx, []LadderTrancheDB{tr}))
	}
}

func plannedRows(t *testing.T, f *ceilingFixture) []LadderInFlightRow {
	t.Helper()
	var got []LadderInFlightRow
	_, err := f.store.PlanAndSaveLadderRun(f.ctx, f.cfgID, lockTestAsOf, func(rows []LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		got = rows
		return lockTestRun(f.cfgID), nil, nil
	})
	require.NoError(t, err)
	return got
}

// The reference is the pre-change algorithm: sum the decimals exactly, then
// round up to a float only when the nearest float is below the sum.
func TestLadderNetting_LegacyOnlyRowsKeepTheExactTotalBitForBit(t *testing.T) {
	f := newCeilingFixture(t)
	amounts := []string{"0.1", "0.2", "0.3", "1234.567891", "0.000001", "0.000001", "7.7", "19.999999"}
	seedNettingRows(t, f, LadderAmountOnDemand, "", amounts...)
	exact := new(big.Rat)
	for _, a := range amounts {
		v, ok := new(big.Rat).SetString(a)
		require.True(t, ok)
		exact.Add(exact, v)
	}
	want, err := nettingFloat(exact)
	require.NoError(t, err)
	got := mustOnDemand(t, plannedRows(t, f))
	require.Equal(t, math.Float64bits(want), math.Float64bits(got))
}

func TestLadderNetting_ThirdRateRowNeverNetsBelowTheExactRational(t *testing.T) {
	f := newCeilingFixture(t)
	seedNettingRows(t, f, LadderAmountCommitment, "1/3", "1.000001")
	rows := plannedRows(t, f)
	require.Len(t, rows, 1)
	require.LessOrEqual(t, new(big.Rat).SetFloat64(rows[0].Rate).Cmp(big.NewRat(1, 3)), 0)
	got := mustOnDemand(t, rows)
	require.GreaterOrEqual(t, new(big.Rat).SetFloat64(got).Cmp(big.NewRat(3000003, 1000000)), 0)
}

func TestLadderNetting_NullBasisStillFailsLoudBeforeBuilder(t *testing.T) {
	f := newCeilingFixture(t)
	_, ids := f.plan(fptr(10), fptr(0), fptr(100), 5)
	f.setBasis(ids[0], "", "")
	built := false
	_, err := f.store.PlanAndSaveLadderRun(f.ctx, f.cfgID, time.Now().UTC().Truncate(time.Microsecond), func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		built = true
		return nil, nil, nil
	})
	require.Error(t, err)
	require.False(t, built)
}
