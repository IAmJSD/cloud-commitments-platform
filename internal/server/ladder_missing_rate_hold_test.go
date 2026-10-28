package server

import (
	"encoding/json"
	"testing"

	pkgladder "github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Owner decision D3 on #1238: existing coverage with no discount rate makes the
// run a persisted Hold that buys nothing and says why, instead of an Errored
// run on every tick. Low-water 12.5 at 80% coverage is a target of 10 OD.

// awsLadderLayers mirrors the AWS provider's three layers so every layer kind
// can hold coverage.
var awsLadderLayers = []pkgladder.LayerSpec{
	{Type: pkgladder.LayerConvertibleRI, Roles: []pkgladder.LayerRole{pkgladder.RoleBase}},
	{Type: pkgladder.LayerComputeSP, Roles: []pkgladder.LayerRole{pkgladder.RoleFlex}},
	{Type: pkgladder.LayerEC2InstanceSP, Roles: []pkgladder.LayerRole{pkgladder.RoleBase}},
}

// ladderPlanJSON decodes the stored plan generically, so the assertions do not
// depend on the DTO type.
type ladderPlanJSON struct {
	ReasonCode string `json:"reason_code"`
	Actions    []struct {
		Action    string `json:"action"`
		Layer     string `json:"layer"`
		Rationale string `json:"rationale"`
	} `json:"actions"`
}

func decodeLadderPlan(t *testing.T, raw []byte) ladderPlanJSON {
	t.Helper()
	var plan ladderPlanJSON
	require.NoError(t, json.Unmarshal(raw, &plan))
	return plan
}

func emptyLayerState(layer pkgladder.LayerType) pkgladder.LayerState {
	return pkgladder.LayerState{Layer: layer, ExistingUSDPerHour: f64(0), ExpiringUSDPerHour: f64(0)}
}

// Each AWS layer kind with existing coverage and no rate: Compute SP and
// Convertible RI (the provider has no rate for them today) and EC2 Instance SP
// (Cost Explorer returned none). The other layers have rates, so only the
// missing one stops sizing.
func TestExecuteLadderRun_MissingDiscountRatePersistsHoldWithReason(t *testing.T) {
	allRates := map[pkgladder.LayerType]float64{
		pkgladder.LayerConvertibleRI: 0.5, pkgladder.LayerComputeSP: 0.5, pkgladder.LayerEC2InstanceSP: 0.5,
	}
	for _, missing := range []pkgladder.LayerType{pkgladder.LayerComputeSP, pkgladder.LayerConvertibleRI, pkgladder.LayerEC2InstanceSP} {
		t.Run(string(missing), func(t *testing.T) {
			rates := map[pkgladder.LayerType]float64{}
			states := map[pkgladder.LayerType]pkgladder.LayerState{}
			for _, spec := range awsLadderLayers {
				states[spec.Type] = emptyLayerState(spec.Type)
				if spec.Type != missing {
					rates[spec.Type] = allRates[spec.Type]
				}
			}
			states[missing] = pkgladder.LayerState{Layer: missing, ExistingUSDPerHour: f64(2.5), ExpiringUSDPerHour: f64(0.5)}
			store := &ladderTestStore{}
			cap := &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5), rates: rates,
				layerStates: states, supportedLayers: awsLadderLayers}

			require.NoError(t, runWithRates(t, store, cap), "a missing rate is a Hold, not an Errored run")

			run := store.savedRun
			require.NotNil(t, run, "the Hold run is persisted")
			assert.Equal(t, pkgladder.RunStatusPlanned, run.Status)
			assert.Empty(t, store.savedTranches, "nothing is bought")
			assert.Zero(t, run.TotalHourlyCommit)
			assert.Nil(t, run.ExistingUSDHr, "existing coverage is unmeasured, never netted 1:1")
			assert.Nil(t, run.GapUSDHr, "the gap is not sized")
			require.NotNil(t, run.TargetUSDHr)
			assert.InDelta(t, 10.0, *run.TargetUSDHr, 1e-9)

			plan := decodeLadderPlan(t, run.Plan)
			assert.Equal(t, "missing_discount_rate", plan.ReasonCode)
			require.Len(t, plan.Actions, 1)
			hold := plan.Actions[0]
			assert.Equal(t, string(pkgladder.ActionHold), hold.Action)
			assert.Equal(t, string(missing), hold.Layer)
			assert.Regexp(t, `^Can't size this yet because layer `+string(missing)+` `, hold.Rationale)
			assert.Contains(t, hold.Rationale, "2.5 commitment USD/hr, 0.5 expiring")
			assert.Contains(t, hold.Rationale, "no discount rate")
			assert.Contains(t, hold.Rationale, "1yr no-upfront")
			assert.NotContains(t, hold.Rationale, "already met")
		})
	}
}

// Every layer without a rate gets its own Hold, in layer order.
func TestExecuteLadderRun_MissingDiscountRateNamesEveryLayer(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5), supportedLayers: awsLadderLayers,
		rates: map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: 0.5},
		layerStates: map[pkgladder.LayerType]pkgladder.LayerState{
			pkgladder.LayerConvertibleRI: {Layer: pkgladder.LayerConvertibleRI, ExistingUSDPerHour: f64(3), ExpiringUSDPerHour: f64(0)},
			pkgladder.LayerComputeSP:     {Layer: pkgladder.LayerComputeSP, ExistingUSDPerHour: f64(0), ExpiringUSDPerHour: f64(1)},
			pkgladder.LayerEC2InstanceSP: spState(f64(4), f64(0)),
		}}
	require.NoError(t, runWithRates(t, store, cap))
	plan := decodeLadderPlan(t, store.savedRun.Plan)
	assert.Equal(t, "missing_discount_rate", plan.ReasonCode)
	require.Len(t, plan.Actions, 2)
	assert.Equal(t, string(pkgladder.LayerComputeSP), plan.Actions[0].Layer)
	assert.Equal(t, string(pkgladder.LayerConvertibleRI), plan.Actions[1].Layer)
	assert.Empty(t, store.savedTranches)
}

// A genuinely met target is still the ordinary Hold: no reason_code and
// Allocate's "target already met" rationale. 20 commitment at rate 0.5 covers
// 20 OD, above the target of 10.
func TestExecuteLadderRun_TargetMetHoldKeepsOrdinaryReason(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5),
		rates: map[pkgladder.LayerType]float64{pkgladder.LayerConvertibleRI: 0.5, pkgladder.LayerEC2InstanceSP: 0.5},
		layerStates: map[pkgladder.LayerType]pkgladder.LayerState{
			pkgladder.LayerConvertibleRI: emptyLayerState(pkgladder.LayerConvertibleRI),
			pkgladder.LayerEC2InstanceSP: spState(f64(10), f64(0)),
		}}
	require.NoError(t, runWithRates(t, store, cap))

	run := store.savedRun
	require.NotNil(t, run)
	assert.Empty(t, store.savedTranches)
	require.NotNil(t, run.ExistingUSDHr)
	assert.InDelta(t, 20.0, *run.ExistingUSDHr, 1e-9)
	plan := decodeLadderPlan(t, run.Plan)
	assert.Empty(t, plan.ReasonCode, "a target-met Hold has no reason_code")
	require.NotEmpty(t, plan.Actions)
	for _, a := range plan.Actions {
		assert.Equal(t, string(pkgladder.ActionHold), a.Action)
		assert.NotContains(t, a.Rationale, "Can't size this yet")
	}
	assert.Regexp(t, `^target already met: `, plan.Actions[0].Rationale)
}

// Bad provider data on a layer without a rate still fails the run: the Hold is
// only for a rate that is missing, not for amounts that cannot be trusted.
func TestExecuteLadderRun_InvalidCoverageWithoutRateIsErrored(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5),
		rates: map[pkgladder.LayerType]float64{pkgladder.LayerEC2InstanceSP: 0.5},
		layerStates: map[pkgladder.LayerType]pkgladder.LayerState{
			pkgladder.LayerConvertibleRI: {Layer: pkgladder.LayerConvertibleRI, ExistingUSDPerHour: f64(-3), ExpiringUSDPerHour: f64(0)},
			pkgladder.LayerEC2InstanceSP: spState(f64(0), f64(0)),
		}}
	err := runWithRates(t, store, cap)
	require.ErrorContains(t, err, "non-negative")
	require.ErrorContains(t, err, string(pkgladder.LayerConvertibleRI))
	assert.Nil(t, store.savedRun)
}
