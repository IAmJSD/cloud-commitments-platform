package server

import (
	"encoding/json"
	"math"
	"math/big"
	"testing"

	pkgladder "github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Units in these tests: "commitment" is commitment USD/hr as the provider
// reports it (SavingsPlan.Commitment); "OD" is on-demand-equivalent USD/hr.
// od = commitment / rate.

func f64(v float64) *float64 { return &v }

func spState(existing, expiring *float64) pkgladder.LayerState {
	return pkgladder.LayerState{Layer: pkgladder.LayerEC2InstanceSP, ExistingUSDPerHour: existing, ExpiringUSDPerHour: expiring}
}

func TestLadderLayerStatesOnDemand_PinsCommitmentToOnDemandConversion(t *testing.T) {
	cases := []struct {
		name                   string
		existing, expiring     float64
		rate                   float64
		wantExisting, wantExpg float64
	}{
		{"half rate doubles", 5, 2, 0.5, 10, 4},
		{"quarter rate quadruples", 3, 1, 0.25, 12, 4},
		{"rate one is identity", 7, 3, 1, 7, 3},
		{"zero expiring stays zero", 6, 0, 0.75, 8, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			states := map[pkgladder.LayerType]pkgladder.LayerState{
				pkgladder.LayerEC2InstanceSP: spState(f64(tc.existing), f64(tc.expiring)),
			}
			out, missing, err := ladderLayerStatesOnDemand(states, map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: tc.rate})
			require.NoError(t, err)
			require.Empty(t, missing)
			got := out[pkgladder.LayerEC2InstanceSP]
			assert.Equal(t, tc.wantExisting, *got.ExistingUSDPerHour, "existing OD")
			assert.Equal(t, tc.wantExpg, *got.ExpiringUSDPerHour, "expiring OD")
			assert.Equal(t, pkgladder.LayerEC2InstanceSP, got.Layer)
		})
	}
}

// 1/0.3 is not a float64. Existing must round up and expiring down, so the
// net existing coverage (existing - expiring) is never understated, which
// would over-buy.
func TestLadderLayerStatesOnDemand_RoundsTowardUnderBuying(t *testing.T) {
	const rate = 0.3
	states := map[pkgladder.LayerType]pkgladder.LayerState{
		pkgladder.LayerEC2InstanceSP: spState(f64(1), f64(1)),
	}
	out, _, err := ladderLayerStatesOnDemand(states, map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: rate})
	require.NoError(t, err)
	exact := new(big.Rat).Quo(big.NewRat(1, 1), new(big.Rat).SetFloat64(rate))

	existing := *out[pkgladder.LayerEC2InstanceSP].ExistingUSDPerHour
	require.GreaterOrEqual(t, new(big.Rat).SetFloat64(existing).Cmp(exact), 0, "existing must not be below the exact OD")
	require.Less(t, new(big.Rat).SetFloat64(math.Nextafter(existing, 0)).Cmp(exact), 0, "existing is the nearest float at or above")

	expiring := *out[pkgladder.LayerEC2InstanceSP].ExpiringUSDPerHour
	require.LessOrEqual(t, new(big.Rat).SetFloat64(expiring).Cmp(exact), 0, "expiring must not be above the exact OD")
	require.Greater(t, new(big.Rat).SetFloat64(math.Nextafter(expiring, math.Inf(1))).Cmp(exact), 0, "expiring is the nearest float at or below")
}

func TestLadderLayerStatesOnDemand_DoesNotMutateInput(t *testing.T) {
	existing, expiring := f64(5), f64(2)
	states := map[pkgladder.LayerType]pkgladder.LayerState{pkgladder.LayerEC2InstanceSP: spState(existing, expiring)}
	_, _, err := ladderLayerStatesOnDemand(states, map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: 0.5})
	require.NoError(t, err)
	assert.Equal(t, 5.0, *existing)
	assert.Equal(t, 2.0, *expiring)
	assert.Same(t, existing, states[pkgladder.LayerEC2InstanceSP].ExistingUSDPerHour)
}

// A layer with no commitment needs no rate (the AWS provider has none for
// Compute SP or Convertible RI today); nil amounts stay nil for Allocate.
func TestLadderLayerStatesOnDemand_EmptyLayersNeedNoRate(t *testing.T) {
	states := map[pkgladder.LayerType]pkgladder.LayerState{
		pkgladder.LayerComputeSP:     {Layer: pkgladder.LayerComputeSP, ExistingUSDPerHour: f64(0), ExpiringUSDPerHour: f64(0)},
		pkgladder.LayerConvertibleRI: {Layer: pkgladder.LayerConvertibleRI},
	}
	out, missing, err := ladderLayerStatesOnDemand(states, map[pkgladder.LayerType]float64{})
	require.NoError(t, err)
	require.Empty(t, missing)
	assert.Equal(t, 0.0, *out[pkgladder.LayerComputeSP].ExistingUSDPerHour)
	assert.Nil(t, out[pkgladder.LayerConvertibleRI].ExistingUSDPerHour)
	assert.Nil(t, out[pkgladder.LayerConvertibleRI].ExpiringUSDPerHour)
}

func TestLadderLayerStatesOnDemand_RejectsUnconvertibleCoverage(t *testing.T) {
	cases := []struct {
		name    string
		state   pkgladder.LayerState
		rates   map[pkgladder.LayerType]float64
		wantErr string
	}{
		{"negative existing without a rate", spState(f64(-5), f64(0)), map[pkgladder.LayerType]float64{pkgladder.LayerComputeSP: 0.5}, "non-negative"},
		{"NaN expiring without a rate", spState(f64(0), f64(math.NaN())), nil, "non-negative"},
		{"zero rate", spState(f64(5), f64(0)), map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: 0}, "must be in (0, 1]"},
		{"rate above one", spState(f64(5), f64(0)), map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: 1.5}, "must be in (0, 1]"},
		{"NaN rate", spState(f64(5), f64(0)), map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: math.NaN()}, "must be in (0, 1]"},
		{"negative existing", spState(f64(-1), f64(0)), map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: 0.5}, "non-negative"},
		{"NaN expiring", spState(f64(1), f64(math.NaN())), map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: 0.5}, "non-negative"},
		{"overflow", spState(f64(math.MaxFloat64), f64(0)), map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: 0.5}, "exceeds float64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ladderLayerStatesOnDemand(map[pkgladder.LayerType]pkgladder.LayerState{pkgladder.LayerEC2InstanceSP: tc.state}, tc.rates)
			require.ErrorContains(t, err, tc.wantErr)
			assert.ErrorContains(t, err, string(pkgladder.LayerEC2InstanceSP))
		})
	}
}

// End to end through executeLadderRun: low-water 12.5 at 80% coverage is a
// target of 10 OD. An existing EC2 Instance SP of 2.5 commitment at rate 0.5
// covers 5 OD, so the run places 5 OD = 2.5 commitment. Treating the 2.5 as
// OD (the bug) placed 7.5 OD = 3.75 commitment.
func TestExecuteLadderRun_ExistingSavingsPlanCoverageIsOnDemandEquivalent(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5),
		rates: map[pkgladder.LayerType]float64{pkgladder.LayerConvertibleRI: 0.5, pkgladder.LayerEC2InstanceSP: 0.5},
		layerStates: map[pkgladder.LayerType]pkgladder.LayerState{
			pkgladder.LayerConvertibleRI: {Layer: pkgladder.LayerConvertibleRI, ExistingUSDPerHour: f64(0), ExpiringUSDPerHour: f64(0)},
			pkgladder.LayerEC2InstanceSP: spState(f64(2.5), f64(0)),
		}}
	require.NoError(t, runWithRates(t, store, cap))

	run := store.savedRun
	require.NotNil(t, run)
	require.NotNil(t, run.ExistingUSDHr)
	assert.InDelta(t, 5.0, *run.ExistingUSDHr, 1e-9, "existing_usd_hr is OD, comparable with target")
	require.NotNil(t, run.GapUSDHr)
	assert.InDelta(t, 5.0, *run.GapUSDHr, 1e-9, "gap OD = target 10 - existing 5")
	assert.InDelta(t, 2.5, run.TotalHourlyCommit, 1e-9, "5 OD at rate 1/2 is 2.5 commitment")

	var dto ladderPlanJSONDTO
	require.NoError(t, json.Unmarshal(run.Plan, &dto))
	require.NotNil(t, dto.ExistingUSDHr)
	assert.InDelta(t, 5.0, *dto.ExistingUSDHr, 1e-9, "plan JSON existing is OD")
}

// Expiring coverage converts by the same rate: existing 4 and expiring 2
// commitment at rate 0.5 leave a net 4 OD, so the gap is 10 - 4 = 6 OD.
func TestExecuteLadderRun_ExpiringCoverageIsOnDemandEquivalent(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5),
		rates: map[pkgladder.LayerType]float64{pkgladder.LayerConvertibleRI: 0.5, pkgladder.LayerEC2InstanceSP: 0.5},
		layerStates: map[pkgladder.LayerType]pkgladder.LayerState{
			pkgladder.LayerConvertibleRI: {Layer: pkgladder.LayerConvertibleRI, ExistingUSDPerHour: f64(0), ExpiringUSDPerHour: f64(0)},
			pkgladder.LayerEC2InstanceSP: spState(f64(4), f64(2)),
		}}
	require.NoError(t, runWithRates(t, store, cap))
	require.NotNil(t, store.savedRun.GapUSDHr)
	assert.InDelta(t, 6.0, *store.savedRun.GapUSDHr, 1e-9)
	assert.InDelta(t, 3.0, store.savedRun.TotalHourlyCommit, 1e-9)
}

// Existing or expiring commitment on a layer with no rate cannot be
// converted. Every such layer is reported, in layer order, with no partial
// conversion for Allocate; layers that hold nothing still need no rate.
func TestLadderLayerStatesOnDemand_ReportsEveryLayerWithoutARate(t *testing.T) {
	states := map[pkgladder.LayerType]pkgladder.LayerState{
		pkgladder.LayerEC2InstanceSP:    spState(f64(4), f64(0)),
		pkgladder.LayerConvertibleRI:    {Layer: pkgladder.LayerConvertibleRI, ExistingUSDPerHour: f64(3), ExpiringUSDPerHour: f64(0)},
		pkgladder.LayerComputeSP:        {Layer: pkgladder.LayerComputeSP, ExistingUSDPerHour: f64(0), ExpiringUSDPerHour: f64(1)},
		pkgladder.LayerAzureSavingsPlan: {Layer: pkgladder.LayerAzureSavingsPlan},
	}
	out, missing, err := ladderLayerStatesOnDemand(states, map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: 0.5})
	require.NoError(t, err)
	assert.Nil(t, out, "no partial conversion reaches Allocate")
	require.Equal(t, []ladderMissingRate{
		{layer: pkgladder.LayerComputeSP, existing: f64(0), expiring: f64(1)},
		{layer: pkgladder.LayerConvertibleRI, existing: f64(3), expiring: f64(0)},
	}, missing)
}
