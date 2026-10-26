package purchase

import (
	"context"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Mock store, mock provider and fixture recommendations (nothing reaches a
// cloud). They cover the plan-step money path that the attach PR (#836) opened.

// azureStepTwo is step 2 of an Azure plan: the stored (unsuppressed) row holds
// 20 units, step 1 suppressed 10, and the plan buys 25% of what is shown.
func azureStepTwo(t *testing.T) (stored config.RecommendationRecord, resolved config.RecommendationRecord) {
	t.Helper()
	acct := "acct-T"
	stored = config.RecommendationRecord{
		ID: "azure-1", Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: "Standard_D2s_v3",
		Term: 1, Payment: "monthly", Count: 20, UpfrontCost: 1234.56, MonthlyCost: new(77.31), Savings: 400.17, CloudAccountID: &acct, Selected: true,
	}
	shown := stored
	shown.Count = 10
	plan := &config.PurchasePlan{ID: "p", Services: map[string]config.ServiceConfig{"azure:compute": {Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 100}}, RampSchedule: config.RampSchedule{PercentPerStep: 25}}
	res, err := planstep.Resolve(context.Background(), azureStepSource{[]config.RecommendationRecord{shown}}, azureStepStore{[]config.RecommendationRecord{stored}}, plan, []string{acct}, 1, nil, nil)
	recs := res.Recs
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, 2, recs[0].Count)
	return stored, recs[0]
}

// S9 through executeAndFinalize (the funnel every executor uses): a step priced
// from the unsuppressed stored row buys; the same step priced from the
// suppressed row is refused before the provider; a re-collection that changes
// the price between notify and run is refused and the step fails.
func TestAzureStepTwo_ExecuteAndFinalize(t *testing.T) {
	stored, resolved := azureStepTwo(t)
	// The pre-fix shape: the count lowered by suppression and sizing, the
	// totals left at the unsuppressed 20 units.
	suppressedPriced := resolved
	suppressedPriced.UpfrontCost = stored.UpfrontCost
	suppressedPriced.MonthlyCost = stored.MonthlyCost
	repriced := stored
	repriced.UpfrontCost++ // Azure re-collection between notify and run

	for _, tc := range []struct {
		name      string
		rec       config.RecommendationRecord
		current   config.RecommendationRecord
		wantCalls int
		wantErr   bool
	}{
		{"resolved from the stored row buys", resolved, stored, 1, false},
		{"priced from the suppressed row is refused", suppressedPriced, stored, 0, true},
		{"price changed after notify is refused", resolved, repriced, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := armedExecution("azure", "compute", 0, "running")
			exec.Recommendations = []config.RecommendationRecord{tc.rec}
			mgr, store, recorder := armedHarness(t, common.ServiceCompute)
			store.On("ListStoredRecommendations", mock.Anything, config.RecommendationFilter{Provider: "azure", RequireAzurePricingMigration: true}).
				Return([]config.RecommendationRecord{tc.current}, nil).Maybe()

			err := mgr.executeAndFinalize(context.Background(), exec)

			assert.Equal(t, tc.wantCalls, recorder.count())
			if tc.wantErr {
				require.ErrorIs(t, err, ErrStaleAzurePricing)
				assert.Equal(t, "failed", exec.Status, "the step fails instead of buying a repriced commitment")
			} else {
				require.NoError(t, err)
				assert.Equal(t, "completed", exec.Status)
			}
		})
	}
}

// tokenCapture runs processPurchaseRecommendations for exec on the given
// provider and returns the IdempotencyToken each
// PurchaseCommitment call received, keyed by resource type.
func tokenCapture(t *testing.T, providerName string, exec *config.PurchaseExecution) map[string]string {
	t.Helper()
	ctx := context.Background()
	store, sender, factory := new(MockConfigStore), new(MockEmailSender), new(MockProviderFactory)
	prov, svc := new(MockProvider), new(MockServiceClient)
	store.SavePurchaseExecutionFn = func(context.Context, *config.PurchaseExecution) error { return nil }
	store.On("SavePurchaseHistory", ctx, mock.Anything).Return(nil)
	factory.On("CreateAndValidateProvider", mock.Anything, providerName, mock.Anything).Return(prov, nil)
	prov.On("GetServiceClient", mock.Anything, mock.Anything, mock.Anything).Return(svc, nil)
	var mu sync.Mutex
	tokens := map[string]string{}
	svc.On("PurchaseCommitment", mock.Anything, mock.AnythingOfType("common.Recommendation"), mock.AnythingOfType("common.PurchaseOptions")).
		Run(func(a mock.Arguments) {
			mu.Lock()
			tokens[a.Get(1).(common.Recommendation).ResourceType] = a.Get(2).(common.PurchaseOptions).IdempotencyToken
			mu.Unlock()
		}).Return(common.PurchaseResult{Success: true, CommitmentID: "c"}, nil)
	m := &Manager{config: store, email: sender, providerFactory: factory}
	_, _, errs, err := m.processPurchaseRecommendations(ctx, exec, &config.PurchasePlan{Name: "p"}, "acct", (*provider.Config)(nil), "")
	require.NoError(t, err)
	require.Empty(t, errs)
	return tokens
}

func sortedAttachedExec(provider, service string) *config.PurchaseExecution {
	recs := []config.RecommendationRecord{
		{ID: "rec-a", Provider: provider, Service: service, ResourceType: "type-a", Region: "r", Count: 1, Term: 1, UpfrontCost: 1, Selected: true},
		{ID: "rec-b", Provider: provider, Service: service, ResourceType: "type-b", Region: "r", Count: 2, Term: 1, UpfrontCost: 2, Selected: true},
	}
	return &config.PurchaseExecution{ExecutionID: "exec-1", IdempotencyKey: "lineage-1", Recommendations: recs}
}

// S11, crash after the provider call: the reaper and re-drive run the same
// persisted (sorted) recommendations again, and every recommendation receives
// DeriveIdempotencyToken(lineage, index) both times, so a provider that dedupes
// on the token buys nothing twice. The token is also what reaches each
// provider's client (its use there is proven in the pinned cloud-commitments-go
// module; see the PR body).
func TestRedriveReusesDerivedTokensForEveryProvider(t *testing.T) {
	for _, tc := range []struct {
		provider, service string
	}{{"aws", "rds"}, {"aws", "savings-plans"}, {"azure", "compute"}, {"gcp", "compute"}} {
		t.Run(tc.provider+"/"+tc.service, func(t *testing.T) {
			first := tokenCapture(t, tc.provider, sortedAttachedExec(tc.provider, tc.service))
			redrive := tokenCapture(t, tc.provider, sortedAttachedExec(tc.provider, tc.service))

			want := map[string]string{
				"type-a": common.DeriveIdempotencyToken("lineage-1", 0),
				"type-b": common.DeriveIdempotencyToken("lineage-1", 1),
			}
			assert.Equal(t, want, first)
			assert.Equal(t, first, redrive, "a re-drive reuses the identical tokens")
			assert.NotEqual(t, first["type-a"], first["type-b"], "each recommendation gets its own token")
		})
	}
}

// Providers whose purchase API has no usable duplicate guard are refused on
// re-drive instead of bought again (the double-buy list in the PR body).
func TestRedriveRefusedWhereNoProviderGuardExists(t *testing.T) {
	for _, rec := range []config.RecommendationRecord{
		{Provider: "azure", Service: "savingsplans"},
		{Provider: "aws", Service: "ec2"},
		{Provider: "aws", Service: "redshift"},
		{Provider: "oracle", Service: "compute"},
	} {
		assert.NotEmpty(t, recRedriveRefusalReason(rec), "%s/%s", rec.Provider, rec.Service)
	}
	for _, rec := range []config.RecommendationRecord{
		{Provider: "aws", Service: "rds"},
		{Provider: "azure", Service: "compute"},
		{Provider: "gcp", Service: "compute"},
	} {
		assert.Empty(t, recRedriveRefusalReason(rec), "%s/%s", rec.Provider, rec.Service)
	}
}
