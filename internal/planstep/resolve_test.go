package planstep

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// All fixtures below are in-memory fakes (fixture/mock evidence, not a real
// database or cloud).

type fakeSource struct {
	shown []config.RecommendationRecord
	err   error
}

func (f fakeSource) ListRecommendationsStrict(context.Context, config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return f.shown, f.err
}

type fakeStore struct {
	rows      []config.RecommendationRecord
	listErr   error
	global    config.GlobalConfig
	globalErr error
}

func (f fakeStore) ListStoredRecommendations(context.Context, config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return f.rows, f.listErr
}
func (f fakeStore) GetGlobalConfig(context.Context) (*config.GlobalConfig, error) {
	return &f.global, f.globalErr
}

// resolveStep1 is the pre-cumulative call shape: step 1, no baseline, no ledger.
func resolveStep1(t *testing.T, src Source, store Store, plan *config.PurchasePlan, accountIDs []string) ([]config.RecommendationRecord, error) {
	t.Helper()
	res, err := Resolve(t.Context(), src, store, plan, accountIDs, 1, nil, nil)
	return res.Recs, err
}

func ptr[T any](v T) *T { return &v }

func azureRec(count int, upfront float64) config.RecommendationRecord {
	return config.RecommendationRecord{
		ID: "azure|acct-T|compute|westeurope|Standard_D2s_v3||1|monthly", Provider: "azure", Service: "compute",
		Region: "westeurope", ResourceType: "Standard_D2s_v3", Term: 1, Payment: "monthly",
		Count: count, UpfrontCost: upfront, MonthlyCost: ptr(upfront / 10), Savings: upfront / 2,
		OnDemandCost: ptr(upfront * 2), CloudAccountID: ptr("acct-T"),
	}
}

func azurePlan(coverage, pct float64) *config.PurchasePlan {
	return &config.PurchasePlan{
		ID:           "plan-1",
		Services:     map[string]config.ServiceConfig{"azure:compute": {Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: coverage}},
		RampSchedule: config.RampSchedule{PercentPerStep: pct},
	}
}

var accts = []string{"acct-T"}

func TestResolveSizesAndPricesFromUnsuppressedRow(t *testing.T) {
	stored := azureRec(20, 1234.56)
	shown := stored
	shown.Count = 10 // step 1 suppressed 10
	shown.SuppressedCount = 10
	shown.UpfrontCost = 1 // the displayed row's totals must never be the price source
	// Plan payment "no-upfront" must match the Azure token "monthly".
	got, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{shown}}, fakeStore{rows: []config.RecommendationRecord{stored}}, azurePlan(80, 25), accts)
	require.NoError(t, err)
	require.Len(t, got, 1)
	n := 2 // floor(10*80*25/10000)
	ratio := float64(n) / float64(stored.Count)
	assert.Equal(t, n, got[0].Count)
	assert.Equal(t, 10, got[0].RecommendedCount)
	assert.True(t, got[0].Selected)
	assert.Zero(t, got[0].SuppressedCount)
	assert.Equal(t, stored.UpfrontCost*ratio, got[0].UpfrontCost)
	assert.Equal(t, stored.Savings*ratio, got[0].Savings)
	assert.Equal(t, *stored.MonthlyCost*ratio, *got[0].MonthlyCost)
	assert.Equal(t, *stored.OnDemandCost*ratio, *got[0].OnDemandCost)
}

// The costs come from the stored row, so the currency must too: a shown row
// that says USD cannot relabel a stored EUR price, and an unknown stored
// currency stays unknown (issue #361).
func TestResolveTakesCurrencyFromStoredRow(t *testing.T) {
	for _, tc := range []struct{ stored, shown string }{{"EUR", "USD"}, {"", "USD"}, {"USD", ""}} {
		stored := azureRec(20, 1000)
		stored.CommitmentCurrency = tc.stored
		shown := stored
		shown.CommitmentCurrency = tc.shown
		got, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{shown}}, fakeStore{rows: []config.RecommendationRecord{stored}}, azurePlan(80, 25), accts)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, tc.stored, got[0].CommitmentCurrency)
	}
}

func TestResolveFloorsProductFirst(t *testing.T) {
	stored := azureRec(100, 1000)
	// 0.7*0.1 as a ratio is 0.06999999999999999, which floors 7 to 6.
	got, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{stored}}, fakeStore{rows: []config.RecommendationRecord{stored}}, azurePlan(70, 10), accts)
	require.NoError(t, err)
	assert.Equal(t, 7, got[0].Count)

	// 0.29 as a ratio is 0.28999999999999998, which floors 29 to 28.
	got, err = resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{stored}}, fakeStore{rows: []config.RecommendationRecord{stored}}, azurePlan(100, 29), accts)
	require.NoError(t, err)
	assert.Equal(t, 29, got[0].Count)
}

func TestResolveFilters(t *testing.T) {
	stored := azureRec(20, 1000)
	other := stored
	other.CloudAccountID = ptr("acct-O")
	other.ID = "other"
	nilAcct := stored
	nilAcct.CloudAccountID = nil
	nilAcct.ID = "nil"
	threeYear := stored
	threeYear.Term = 3
	upfront := stored
	upfront.Payment = "upfront"
	mixed := []config.RecommendationRecord{other, nilAcct, threeYear, upfront}
	// The source ignores the filter, so the explicit re-check is what holds.
	_, err := resolveStep1(t, fakeSource{shown: mixed}, fakeStore{rows: append(mixed, stored)}, azurePlan(100, 100), accts)
	assert.ErrorIs(t, err, ErrNoMatch)

	disabled := azurePlan(100, 100)
	svc := disabled.Services["azure:compute"]
	svc.Enabled = false
	disabled.Services["azure:compute"] = svc
	_, err = resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{stored}}, fakeStore{rows: []config.RecommendationRecord{stored}}, disabled, accts)
	assert.ErrorIs(t, err, ErrNoMatch)

	_, err = resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{stored}}, fakeStore{rows: []config.RecommendationRecord{stored}, global: config.GlobalConfig{EnabledProviders: []string{"gcp"}}}, azurePlan(100, 100), accts)
	assert.ErrorIs(t, err, ErrNoMatch, "plan must apply the same enabled-providers filter as Opportunities")
}

func TestResolveFailsClosed(t *testing.T) {
	stored := azureRec(20, 1000)
	ok := fakeSource{shown: []config.RecommendationRecord{stored}}
	okStore := fakeStore{rows: []config.RecommendationRecord{stored}}
	dupe := fakeStore{rows: []config.RecommendationRecord{stored, stored}}
	zeroPrice := stored
	zeroPrice.UpfrontCost, zeroPrice.MonthlyCost, zeroPrice.Savings = 0, nil, 0
	badPay := stored
	badPay.Payment = "bogus"
	badPayPlan := azurePlan(100, 100)
	svc := badPayPlan.Services["azure:compute"]
	svc.Payment = "bogus"
	badPayPlan.Services["azure:compute"] = svc

	cases := []struct {
		name  string
		src   Source
		store Store
		plan  *config.PurchasePlan
		accts []string
	}{
		{"source (suppression/override) error", fakeSource{err: errors.New("suppressions down")}, okStore, azurePlan(100, 100), accts},
		{"stored list error", ok, fakeStore{listErr: errors.New("db down")}, azurePlan(100, 100), accts},
		{"global config error", ok, fakeStore{rows: okStore.rows, globalErr: errors.New("cfg down")}, azurePlan(100, 100), accts},
		{"no target accounts", ok, okStore, azurePlan(100, 100), nil},
		{"zero coverage", ok, okStore, azurePlan(0, 100), accts},
		{"coverage over 100", ok, okStore, azurePlan(101, 100), accts},
		{"zero percent per step", ok, okStore, azurePlan(100, 0), accts},
		{"percent per step over 100", ok, okStore, azurePlan(100, 101), accts},
		{"invalid plan payment", ok, okStore, badPayPlan, accts},
		{"unrecognized rec payment", fakeSource{shown: []config.RecommendationRecord{badPay}}, okStore, azurePlan(100, 100), accts},
		{"shown rec has no stored row", ok, fakeStore{}, azurePlan(100, 100), accts},
		{"duplicate stored identity", ok, dupe, azurePlan(100, 100), accts},
		{"stored row has no usable price", fakeSource{shown: []config.RecommendationRecord{zeroPrice}}, fakeStore{rows: []config.RecommendationRecord{zeroPrice}}, azurePlan(100, 100), accts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveStep1(t, tc.src, tc.store, tc.plan, tc.accts)
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrNoMatch)
			assert.Empty(t, got, "nothing may be returned to attach")
		})
	}
}

func TestResolveJoinsOnFullIdentity(t *testing.T) {
	mk := func(az, platform string, upfront float64) config.RecommendationRecord {
		r := azureRec(10, upfront)
		r.Provider, r.Service, r.Payment = "aws", "ec2", "no-upfront"
		r.ID = "aws|" + az + platform
		r.AvailabilityZone = az
		r.Details = json.RawMessage(`{"platform":"` + platform + `","tenancy":"default"}`)
		return r
	}
	rows := []config.RecommendationRecord{mk("us-east-1a", "Linux/UNIX", 100), mk("us-east-1b", "Linux/UNIX", 200), mk("us-east-1a", "Windows", 300)}
	plan := &config.PurchasePlan{ID: "p", Services: map[string]config.ServiceConfig{"aws:ec2": {Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 100}}, RampSchedule: config.RampSchedule{PercentPerStep: 50}}
	got, err := resolveStep1(t, fakeSource{shown: rows}, fakeStore{rows: rows}, plan, accts)
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i := range got {
		assert.Equal(t, rows[i].UpfrontCost*0.5, got[i].UpfrontCost, "priced from its own row, not a sibling")
		assert.Equal(t, rows[i].ID, got[i].ID)
		assert.Equal(t, rows[i].AvailabilityZone, got[i].AvailabilityZone)
	}
}

func TestResolveRefusesOverlappingPlanServices(t *testing.T) {
	stored := azureRec(20, 1000)
	plan := azurePlan(100, 100)
	plan.Services["azure:compute-again"] = plan.Services["azure:compute"]
	_, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{stored}}, fakeStore{rows: []config.RecommendationRecord{stored}}, plan, accts)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoMatch)
}

func spRec(details string) config.RecommendationRecord {
	r := azureRec(1, 1000)
	r.Provider, r.Service, r.Payment, r.Term = "aws", "savings-plans-compute", "no-upfront", 1
	r.ID = "aws|sp"
	r.AvailabilityZone = "us-east-1a"
	r.Details = json.RawMessage(details)
	return r
}

func spPlan(coverage, pct float64) *config.PurchasePlan {
	return &config.PurchasePlan{ID: "p", Services: map[string]config.ServiceConfig{"aws:sp": {Provider: "aws", Service: "savings-plans-compute", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: coverage}}, RampSchedule: config.RampSchedule{PercentPerStep: pct}}
}

func TestResolveSavingsPlanRoundsDownAndKeepsRawKeys(t *testing.T) {
	stored := spRec(`{"hourly_commitment":1.234,"platform":"Linux/UNIX","tenancy":"default","plan_type":"Compute","extra":{"a":[1,2]}}`)
	before, err := stored.ComputeIdentity()
	require.NoError(t, err)
	stored.Count = 1
	shown := stored
	got, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{shown}}, fakeStore{rows: []config.RecommendationRecord{stored}}, spPlan(80, 25), accts)
	require.NoError(t, err)
	require.Len(t, got, 1)
	// 1.234 * 80 * 25 / 10000 = 0.2468, rounded DOWN to cents.
	var d map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got[0].Details, &d))
	assert.JSONEq(t, `0.24`, string(d["hourly_commitment"]))
	assert.JSONEq(t, `{"a":[1,2]}`, string(d["extra"]))
	assert.JSONEq(t, `"Compute"`, string(d["plan_type"]))
	after, err := got[0].ComputeIdentity()
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, stored.ID, got[0].ID)
	assert.Equal(t, stored.AvailabilityZone, got[0].AvailabilityZone)
	assert.Equal(t, stored.Count, got[0].Count, "SP is not count-denominated")
	newH, oldH := 0.24, 1.234
	assert.Equal(t, stored.UpfrontCost*(newH/oldH), got[0].UpfrontCost)
}

func TestResolveSavingsPlanBelowMinimumAndBadDetails(t *testing.T) {
	small := spRec(`{"hourly_commitment":0.04}`)
	_, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{small}}, fakeStore{rows: []config.RecommendationRecord{small}}, spPlan(80, 25), accts)
	assert.ErrorIs(t, err, ErrNoMatch, "0.008/hr is below one cent: dropped, never rounded up")
	for name, details := range map[string]string{"missing key": `{"platform":"x"}`, "zero": `{"hourly_commitment":0}`, "not json": `nope`, "empty": ``} {
		t.Run(name, func(t *testing.T) {
			bad := spRec(details)
			got, err := resolveStep1(t, fakeSource{shown: []config.RecommendationRecord{bad}}, fakeStore{rows: []config.RecommendationRecord{bad}}, spPlan(100, 100), accts)
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrNoMatch)
			assert.Empty(t, got)
		})
	}
}

// D4: the service values the Plans form offers must equal the Service the
// collector stores on a rec (string(common.ServiceType)); the exact-match
// join silently matches nothing otherwise.
func TestPlanFormServiceValuesMatchStoredServices(t *testing.T) {
	html, err := os.ReadFile("../../frontend/src/index.html")
	require.NoError(t, err)
	stored := map[string][]common.ServiceType{
		"aws": {common.ServiceEC2, common.ServiceRDS, common.ServiceElastiCache, common.ServiceOpenSearch, common.ServiceMemoryDB, common.ServiceRedshift,
			common.ServiceSavingsPlansCompute, common.ServiceSavingsPlansEC2Instance, common.ServiceSavingsPlansSageMaker, common.ServiceSavingsPlansDatabase},
		"azure": {common.ServiceCompute, common.ServiceRelationalDB, common.ServiceCache, common.ServiceSearch},
		"gcp":   {common.ServiceCompute, common.ServiceRelationalDB, common.ServiceCache},
	}
	groups := regexp.MustCompile(`(?s)<optgroup[^>]*data-provider="(\w+)"[^>]*>(.*?)</optgroup>`)
	opts := regexp.MustCompile(`<option value="([^"]+)"`)
	seen := map[string]int{}
	for _, g := range groups.FindAllStringSubmatch(string(html), -1) {
		provider := g[1]
		for _, o := range opts.FindAllStringSubmatch(g[2], -1) {
			seen[provider]++
			var want []string
			for _, s := range stored[provider] {
				want = append(want, string(s))
			}
			assert.Contains(t, want, o[1], "plan form offers %s/%s but no rec is stored with that Service", provider, o[1])
		}
	}
	assert.Equal(t, map[string]int{"aws": 10, "azure": 4, "gcp": 3}, seen)
	// The legacy umbrella is still a Savings Plan, but no form value equals it.
	assert.True(t, common.IsSavingsPlan("savingsplans"))
	assert.NotContains(t, string(html), `<option value="savingsplans"`)
}
