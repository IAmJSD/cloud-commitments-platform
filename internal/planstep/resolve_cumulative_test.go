package planstep

import (
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture/mock evidence: in-memory fakes, no database or cloud.

func cumKey(t *testing.T, r config.RecommendationRecord) string {
	t.Helper()
	k, err := config.RecIdentityKey(&r)
	require.NoError(t, err)
	return k
}

func cumPlan(coverage, pps float64, typ string, steps int) *config.PurchasePlan {
	p := azurePlan(coverage, pps)
	p.RampSchedule.Type, p.RampSchedule.TotalSteps = typ, steps
	return p
}

// runRamp drives Resolve step by step like the scheduler will: step 1 freezes
// the baseline, every bought count lands in the ledger. shownAt models what the
// strict read shows before each step given what is committed so far.
func runRamp(t *testing.T, plan *config.PurchasePlan, stored int, steps int, shownAt func(step, committed int) int) []int {
	t.Helper()
	row := azureRec(stored, 1000)
	key := cumKey(t, row)
	var baseline config.PlanRampBaseline
	committed := map[string]config.PlanRampQuantity{}
	var bought []int
	for step := 1; step <= steps; step++ {
		shown := row
		shown.Count = shownAt(step, committed[key].Count)
		res, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{shown}}, fakeStore{rows: []config.RecommendationRecord{row}}, plan, accts, step, baseline, committed)
		n := 0
		if err == nil {
			n = res.Recs[0].Count
		} else {
			require.True(t, errors.Is(err, ErrTargetMet) || errors.Is(err, ErrNoMatch), "unexpected error: %v", err)
		}
		if step == 1 {
			baseline = res.Baseline
		}
		committed[key] = config.PlanRampQuantity{Count: committed[key].Count + n}
		bought = append(bought, n)
	}
	return bought
}

// suppressed models step-1..k buys hiding capacity from the strict read.
func suppressed(demand int) func(int, int) int {
	return func(_, committed int) int { return demand - committed }
}

func TestCumulativeWeekly25Coverage80(t *testing.T) {
	// The pre-cumulative formula bought 20,16,12,10 here.
	got := runRamp(t, cumPlan(80, 25, "weekly", 4), 100, 5, suppressed(100))
	assert.Equal(t, []int{20, 20, 20, 20, 0}, got)
}

func TestCumulativeCustom3x40(t *testing.T) {
	got := runRamp(t, cumPlan(80, 40, "custom", 3), 100, 4, suppressed(100))
	assert.Equal(t, []int{32, 32, 16, 0}, got)
}

func TestCumulativeLaggingShownDoesNotOverBuy(t *testing.T) {
	// Suppressions expired: the provider still lists the capacity step 1 bought,
	// so shown regrows to the full 100. A base of shown+committed would grow the
	// target to 120*0.8*50% = 48 and buy 28; the frozen base buys exactly 20.
	full := func(int, int) int { return 100 }
	got := runRamp(t, cumPlan(80, 25, "weekly", 4), 100, 6, full)
	assert.Equal(t, []int{20, 20, 20, 20, 0, 0}, got)
	assert.Equal(t, 80, got[0]+got[1]+got[2]+got[3], "total never exceeds frozen base*coverage")
}

func TestCumulativeManualPurchaseLowersShownClampsToShown(t *testing.T) {
	// Someone bought 85 elsewhere before step 2: only 15 remain shown, below the
	// 20 the target asks for.
	got := runRamp(t, cumPlan(80, 25, "weekly", 4), 100, 2, func(step, committed int) int {
		if step == 1 {
			return 100
		}
		return 15
	})
	assert.Equal(t, []int{20, 15}, got)
}

func TestCumulativeTargetMet(t *testing.T) {
	row := azureRec(100, 1000)
	key := cumKey(t, row)
	plan := cumPlan(80, 25, "weekly", 4)
	baseline := config.PlanRampBaseline{key: {Count: 100}}
	for _, committed := range []int{40, 55} { // step 2 target is 40
		res, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{row}}, fakeStore{rows: []config.RecommendationRecord{row}}, plan, accts, 2, baseline, map[string]config.PlanRampQuantity{key: {Count: committed}})
		require.ErrorIs(t, err, ErrTargetMet)
		assert.NotErrorIs(t, err, ErrNoMatch)
		assert.Empty(t, res.Recs)
	}
}

func TestCumulativeShownZero(t *testing.T) {
	// A rec that shows nothing buys nothing and is no-match, not target-met.
	row := azureRec(100, 1000)
	zero := row
	zero.Count = 0
	res, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{zero}}, fakeStore{rows: []config.RecommendationRecord{row}}, cumPlan(80, 25, "weekly", 4), accts, 1, nil, nil)
	require.ErrorIs(t, err, ErrNoMatch)
	assert.NotErrorIs(t, err, ErrTargetMet)
	assert.Nil(t, res.Baseline)
}

func TestCumulativePercentOver100Clamps(t *testing.T) {
	// 60%/step: step 2 is 120% -> 100%, step 3 stays 100% and is met.
	got := runRamp(t, cumPlan(100, 60, "custom", 3), 100, 3, suppressed(100))
	assert.Equal(t, []int{60, 40, 0}, got)
}

func TestCumulativeRoundsDown(t *testing.T) {
	// base 7: floor(7*0.5)=3, then target 7 buys the remaining 4.
	assert.Equal(t, []int{3, 4}, runRamp(t, cumPlan(100, 50, "custom", 2), 7, 2, suppressed(7)))
	// base 7 at 50% coverage, 50%/step: floor(1.75)=1 then floor(3.5)=3 -> buys 2.
	assert.Equal(t, []int{1, 2}, runRamp(t, cumPlan(50, 50, "custom", 2), 7, 2, suppressed(7)))
}

func TestCumulativeBaselineRecordsEveryMatchedIdentity(t *testing.T) {
	tiny := azureRec(3, 1000) // floor(3*10*10/10000) = 0: sizes to nothing
	res, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{tiny}}, fakeStore{rows: []config.RecommendationRecord{tiny}}, cumPlan(10, 10, "weekly", 10), accts, 1, nil, nil)
	require.ErrorIs(t, err, ErrNoMatch)
	assert.Equal(t, config.PlanRampBaseline{cumKey(t, tiny): {Count: 3}}, res.Baseline)
}

func TestCumulativeSkipsIdentityWithoutBaselineAfterStep1(t *testing.T) {
	row := azureRec(100, 1000)
	res, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{row}}, fakeStore{rows: []config.RecommendationRecord{row}}, cumPlan(80, 25, "weekly", 4), accts, 2, config.PlanRampBaseline{"other": {Count: 9}}, nil)
	require.ErrorIs(t, err, ErrNoMatch)
	assert.Equal(t, []string{cumKey(t, row)}, res.Skipped)
	assert.Nil(t, res.Baseline, "never frozen after step 1")
}

func TestCumulativeRejectsStepBelowOne(t *testing.T) {
	row := azureRec(100, 1000)
	_, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{row}}, fakeStore{rows: []config.RecommendationRecord{row}}, cumPlan(80, 25, "weekly", 4), accts, 0, nil, nil)
	require.Error(t, err)
}

func TestCumulativeGapWithShownZeroIsNoMatchNotNoop(t *testing.T) {
	// G1: step 2 still has a gap of 20 but the identity is hidden (shown 0).
	row := azureRec(100, 1000)
	key := cumKey(t, row)
	zero := row
	zero.Count = 0
	_, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{zero}}, fakeStore{rows: []config.RecommendationRecord{row}}, cumPlan(80, 25, "weekly", 4), accts, 2,
		config.PlanRampBaseline{key: {Count: 100}}, map[string]config.PlanRampQuantity{key: {Count: 20}})
	require.ErrorIs(t, err, ErrNoMatch)
	assert.NotErrorIs(t, err, ErrTargetMet)
}

func TestCumulativeTargetMetIgnoresShown(t *testing.T) {
	// Fully bought and hidden from the read: still met, decided from the ledger.
	row := azureRec(100, 1000)
	key := cumKey(t, row)
	_, err := Resolve(t.Context(), fakeSource{}, fakeStore{rows: []config.RecommendationRecord{row}}, cumPlan(80, 25, "weekly", 4), accts, 4,
		config.PlanRampBaseline{key: {Count: 100}}, map[string]config.PlanRampQuantity{key: {Count: 80}})
	require.ErrorIs(t, err, ErrTargetMet)
}

func TestCumulativeEmptyStep1IsNoMatchWithoutBaseline(t *testing.T) {
	// G2: nothing shown at step 1 freezes nothing and is never a no-op.
	row := azureRec(100, 1000)
	res, err := Resolve(t.Context(), fakeSource{}, fakeStore{rows: []config.RecommendationRecord{row}}, cumPlan(80, 25, "weekly", 4), accts, 1, nil, nil)
	require.ErrorIs(t, err, ErrNoMatch)
	assert.Nil(t, res.Baseline)
}

func TestCumulativeNoBaselineIsNeverTargetMet(t *testing.T) {
	row := azureRec(100, 1000)
	_, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{row}}, fakeStore{rows: []config.RecommendationRecord{row}}, cumPlan(80, 25, "weekly", 4), accts, 2, nil, nil)
	require.ErrorIs(t, err, ErrNoMatch)
}

func TestCumulativeFlooredToZeroTargetIsLegitimateNoop(t *testing.T) {
	// base 1 at cov 80, 25%/step: floor(0.2)=0 target; baseline is kept and the
	// step is a no-op only once something is baselined.
	row := azureRec(1, 1000)
	key := cumKey(t, row)
	res, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{row}}, fakeStore{rows: []config.RecommendationRecord{row}}, cumPlan(80, 25, "weekly", 4), accts, 1, nil, nil)
	require.ErrorIs(t, err, ErrNoMatch) // first sight: nothing committed yet
	assert.Equal(t, config.PlanRampBaseline{key: {Count: 1}}, res.Baseline)
	_, err = Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{row}}, fakeStore{rows: []config.RecommendationRecord{row}}, cumPlan(80, 25, "weekly", 4), accts, 2, res.Baseline, nil)
	require.ErrorIs(t, err, ErrTargetMet)
}
