package server

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	pkgladder "github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

var ratesTestNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// runWithRates plans one run (low-water 12.5 -> target 10, one base allocation
// of 10 on-demand-equivalent) and returns the store and the error.
func runWithRates(t *testing.T, store *ladderTestStore, cap pkgladder.LadderCapability) error {
	t.Helper()
	dbCfg := validTestDBConfig("cfg-rates")
	app := &Application{Config: store}
	return app.executeLadderRun(testutil.TestContext(t), &dbCfg, cap, "123456789012", pkgladder.Term1Year, pkgladder.PaymentNoUpfront, ratesTestNow)
}

func TestExecuteLadderRun_RateHalfPersistsCommitmentDollars(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5),
		rates: map[pkgladder.LayerType]float64{pkgladder.LayerConvertibleRI: 0.5}}
	require.NoError(t, runWithRates(t, store, cap))

	require.Len(t, store.savedTranches, 1)
	tr := store.savedTranches[0]
	assert.InDelta(t, 5.0, tr.AmountUSDHr, 1e-9, "10 on-demand at rate 1/2 is 5 commitment")
	require.NotNil(t, tr.AmountBasis)
	assert.Equal(t, config.LadderAmountCommitment, *tr.AmountBasis)
	require.NotNil(t, tr.CommitmentRate)
	assert.Equal(t, "1/2", *tr.CommitmentRate)

	run := store.savedRun
	assert.InDelta(t, 5.0, run.TotalHourlyCommit, 1e-9, "total_hourly_commit stays commitment dollars")
	require.NotNil(t, run.GapUSDHr)
	assert.InDelta(t, 10.0, *run.GapUSDHr, 1e-9, "gap_usd_hr is on-demand-equivalent, comparable with target")
}

func TestExecuteLadderRun_PlanJSONGapIsOnDemand(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5),
		rates: map[pkgladder.LayerType]float64{pkgladder.LayerConvertibleRI: 0.5}}
	require.NoError(t, runWithRates(t, store, cap))
	var dto ladderPlanJSONDTO
	require.NoError(t, json.Unmarshal(store.savedRun.Plan, &dto))
	require.NotNil(t, dto.GapUSDHr)
	assert.InDelta(t, 10.0, *dto.GapUSDHr, 1e-9)
}

func TestExecuteLadderRun_NoRateCapabilityIsErrored(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaseline()}
	// Embedding the interface hides GetCommitmentRates.
	err := runWithRates(t, store, struct{ pkgladder.LadderCapability }{cap})
	require.ErrorContains(t, err, "commitment rates")
	assert.Nil(t, store.savedRun)
}

func TestExecuteLadderRun_RateFetchErrorIsErrored(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaseline(), ratesErr: errors.New("ce throttled")}
	err := runWithRates(t, store, cap)
	require.ErrorContains(t, err, "ce throttled")
	assert.Nil(t, store.savedRun)
}

func TestExecuteLadderRun_EmptyRateMapIsHoldOnlyAndPersists(t *testing.T) {
	store := &ladderTestStore{}
	cap := &fakeLadderCapability{t: t, baseline: testBaselineLowWater(12.5), rates: map[pkgladder.LayerType]float64{}}
	require.NoError(t, runWithRates(t, store, cap))
	require.NotNil(t, store.savedRun)
	assert.Empty(t, store.savedTranches)
	assert.Zero(t, store.savedRun.TotalHourlyCommit)
}

func TestExecuteLadderRun_InFlightOnUnsupportedLayerFailsLoud(t *testing.T) {
	store := &ladderTestStore{inFlightRows: []config.LadderInFlightRow{{Layer: pkgladder.LayerComputeSP, CommitmentUSDPerHour: 1, Rate: 1}}}
	cap := &fakeLadderCapability{t: t, baseline: testBaseline()}
	err := runWithRates(t, store, cap)
	require.ErrorContains(t, err, "not in the supported layers")
	assert.Nil(t, store.savedRun)
}

func TestBuildTrancheDBRows_EmptyCommitmentRateErrors(t *testing.T) {
	cfgID := "cfg"
	_, err := buildTrancheDBRows([]pkgladder.Tranche{{
		ID: "tr-norate", Layer: pkgladder.LayerEC2InstanceSP, AmountUSDPerHour: "5", FireAfter: ratesTestNow,
	}}, "run", &cfgID)
	require.ErrorContains(t, err, "tr-norate")
}
