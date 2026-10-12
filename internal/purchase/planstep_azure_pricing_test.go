package purchase

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type azureStepSource struct{ shown []config.RecommendationRecord }

func (s azureStepSource) ListRecommendationsStrict(context.Context, config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return s.shown, nil
}

type azureStepStore struct{ rows []config.RecommendationRecord }

func (s azureStepStore) ListStoredRecommendations(context.Context, config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return s.rows, nil
}
func (azureStepStore) GetGlobalConfig(context.Context) (*config.GlobalConfig, error) {
	return &config.GlobalConfig{}, nil
}

// Fixture/mock evidence. Step 2 of an Azure plan after step 1 suppressed half
// the rec: the sized rec must pass the stale-pricing check against the
// unsuppressed stored row, which scaling the suppressed totals would not.
func TestResolvedAzureStepPassesStalePricingRefusal(t *testing.T) {
	acct := "acct-T"
	stored := config.RecommendationRecord{
		ID: "azure-1", Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: "Standard_D2s_v3",
		Term: 1, Payment: "monthly", Count: 20, UpfrontCost: 1234.56, MonthlyCost: new(77.31), Savings: 400.17, CloudAccountID: &acct, Selected: true,
	}
	shown := stored
	shown.Count = 10 // step 1 bought 10; totals still describe 20
	plan := &config.PurchasePlan{ID: "p", Services: map[string]config.ServiceConfig{"azure:compute": {Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 100}}, RampSchedule: config.RampSchedule{PercentPerStep: 25}}

	recs, err := planstep.Resolve(context.Background(), azureStepSource{[]config.RecommendationRecord{shown}}, azureStepStore{[]config.RecommendationRecord{stored}}, plan, []string{acct})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, 2, recs[0].Count)

	check := func(rec config.RecommendationRecord) error {
		exec := armedExecution("azure", "compute", 0, "approved")
		exec.Recommendations = []config.RecommendationRecord{rec}
		mgr, store, _ := armedHarness(t, common.ServiceCompute)
		store.On("ListStoredRecommendations", mock.Anything, config.RecommendationFilter{Provider: "azure", RequireAzurePricingMigration: true}).Return([]config.RecommendationRecord{stored}, nil).Maybe()
		return mgr.staleAzurePricingRefusal(context.Background(), exec)
	}
	assert.NoError(t, check(recs[0]))

	// The pre-fix shape: count lowered by suppression, totals left at 20 units.
	wrong := shown
	wrong.Count = 2
	assert.ErrorIs(t, check(wrong), ErrStaleAzurePricing)
}
