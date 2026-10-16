package planstep

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture evidence: actual Resolve with in-memory recommendation reads.
func equalCountRamp(t *testing.T, p *config.PurchasePlan, n int, percent float64) {
	t.Helper()
	require.NoError(t, json.Unmarshal(fmt.Appendf(nil, `{"type":"custom","allocation_mode":"equal_count","total_steps":%d,"percent_per_step":%v}`, n, percent), &p.RampSchedule))
}

func TestEqualCountResolve(t *testing.T) {
	p := cumPlan(100, 100.0/3, "custom", 3)
	equalCountRamp(t, p, 3, 100.0/3)
	assert.Equal(t, []int{32, 32, 33}, runRamp(t, p, 97, 3, suppressed(97)))
	equalCountRamp(t, p, 97, 100.0/97)
	got := runRamp(t, p, 97, 97, suppressed(97))
	for step, n := range got {
		assert.Equal(t, 1, n, "step %d", step+1)
	}
	equalCountRamp(t, p, 3, 33)
	p.Services["azure:compute"] = azurePlan(50.5, 33).Services["azure:compute"]
	assert.Equal(t, []int{16, 16, 16}, runRamp(t, p, 97, 3, suppressed(97)))
	assert.Equal(t, []int{33, 33, 33}, runRamp(t, cumPlan(100, 33, "custom", 3), 100, 3, suppressed(100)))
	assert.InDelta(t, 100.0/3, p.RampSchedule.CumulativePercent(1), 1e-12)
	row := azureRec(97, 1000)
	key := cumKey(t, row)
	shown := row
	shown.Count = 2
	res, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{shown}}, fakeStore{rows: []config.RecommendationRecord{row}}, p, accts, 2, config.PlanRampBaseline{key: {Count: 97}}, map[string]config.PlanRampQuantity{key: {Count: 17}})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Recs[0].Count, "own ledger gap clamps to shown")
	_, err = Resolve(t.Context(), fakeSource{}, fakeStore{}, p, accts, 2, config.PlanRampBaseline{key: {Count: 97}}, map[string]config.PlanRampQuantity{key: {Count: 32}})
	require.ErrorIs(t, err, ErrTargetMet)
}

func TestEqualCountSavingsPlan(t *testing.T) {
	for _, tc := range []struct {
		count        int
		hourly, want string
		percent      float64
	}{{1, "1.234", "1.23", 33}, {1, "1.234", "1.23", 0}, {3, "1.234", "0.41", 33}, {97, "0.97", "0.01", 33}, {1, "1e14", "100000000000000", 33}, {1, "1e20", "100000000000000000000", 33}} {
		t.Run(fmt.Sprint(tc.count), func(t *testing.T) {
			p := spPlan(100, 33)
			equalCountRamp(t, p, tc.count, tc.percent)
			if tc.count == 1 {
				p.RampSchedule.Type = "immediate"
			}
			row := spRec(`{"hourly_commitment":` + tc.hourly + `,"extra":9007199254740993}`)
			got, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{row}}, fakeStore{rows: []config.RecommendationRecord{row}}, p, accts)
			require.NoError(t, err)
			var details map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(got[0].Details, &details))
			assert.Equal(t, tc.want, string(details["hourly_commitment"]))
			assert.Equal(t, "9007199254740993", string(details["extra"]))
			if tc.count > 1 {
				p.Enabled, p.AutoPurchase = true, true
				require.ErrorIs(t, Precheck(p), config.ErrMultiStepSavingsPlans)
			}
		})
	}
}

func TestEqualCountImmediateOmittedPercent(t *testing.T) {
	p := azurePlan(100, 0)
	equalCountRamp(t, p, 1, 0)
	p.RampSchedule.Type = "immediate"
	require.NoError(t, p.RampSchedule.Validate())
	row := azureRec(97, 1000)
	got, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{row}}, fakeStore{rows: []config.RecommendationRecord{row}}, p, accts)
	require.NoError(t, err)
	assert.Equal(t, 97, got[0].Count)
	require.NoError(t, json.Unmarshal([]byte(`{"allocation_mode":""}`), &p.RampSchedule))
	_, err = resolveStep1(t, nil, nil, p, accts)
	require.ErrorIs(t, err, ErrUnsupportedPlan)
}

func TestEqualCountRejectsBeforeReads(t *testing.T) {
	for _, raw := range []string{`{"allocation_mode":"future"}`, `{"allocation_mode":"equal_count","total_steps":0}`, `{"allocation_mode":"equal_count","total_steps":101}`, `{"allocation_mode":"equal_count","total_steps":2,"type":"immediate"}`} {
		p := azurePlan(100, 33)
		require.NoError(t, json.Unmarshal([]byte(raw), &p.RampSchedule))
		require.Error(t, p.RampSchedule.Validate())
		_, err := resolveStep1(t, nil, nil, p, accts)
		require.ErrorIs(t, err, ErrUnsupportedPlan)
	}
}

func TestEqualCountInvalidQuantities(t *testing.T) {
	p := cumPlan(100, 33, "custom", 3)
	equalCountRamp(t, p, 3, 33)
	row := azureRec(97, 1000)
	key := cumKey(t, row)
	for _, counts := range [][3]int{{-1, 97, 0}, {97, -1, 0}, {97, 97, -1}} {
		shown := row
		shown.Count = counts[0]
		_, err := Resolve(t.Context(), fakeSource{shown: []config.RecommendationRecord{shown}}, fakeStore{rows: []config.RecommendationRecord{row}}, p, accts, 2, config.PlanRampBaseline{key: {Count: counts[1]}}, map[string]config.PlanRampQuantity{key: {Count: counts[2]}})
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNoMatch)
	}
	for _, tc := range []struct {
		hourly   string
		coverage float64
		want     string
	}{
		{"90071992547409", 99, "cannot represent whole cents"},
		{"100000000000000.1", 100, "cannot represent whole cents"},
	} {
		p = spPlan(tc.coverage, 33)
		equalCountRamp(t, p, 1, 33)
		row = spRec(`{"hourly_commitment":` + tc.hourly + `}`)
		_, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{row}}, fakeStore{rows: []config.RecommendationRecord{row}}, p, accts)
		require.ErrorContains(t, err, tc.want)
	}
}
