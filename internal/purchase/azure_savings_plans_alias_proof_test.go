package purchase

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/billingbenefits/armbillingbenefits"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-go/providers/azure/services/savingsplans"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// --- Azure Savings Plans row of the duplicate-purchase matrix (issue #838) ---
//
// The matrix in #838 (written against cloud-commitments-go ad67030387e4) says
// the Azure savings-plans client names its order alias from
// time.Now().UnixNano(), so the platform's derived idempotency token never
// reaches Azure. The module pinned in go.mod since then
// (providers/azure 3def3f898617, services/savingsplans/client.go
// PurchaseCommitment) derives the alias name from the token through
// common.OrderIDFromToken and keeps the timestamp name only for an empty token
// (the CLI path).
//
// TestRedriveReusesDerivedTokensForEveryProvider stops at the service-client
// boundary with a mock client, so it says nothing about what the real Azure
// client does with the token. This test runs the platform's purchase fan-out
// against the REAL pinned savings-plans client, with only the Azure
// order-alias API faked, and records the alias name each purchase PUTs. A
// go.mod bump that brings the timestamp alias back fails here instead of
// landing silently.
//
// What it does NOT prove: that Azure answers a second PUT of an existing
// savingsPlanOrderAliases name by returning the first order instead of
// creating a second plan or failing. That is server behavior no fake can show,
// which is why recRedriveRefusalReason still refuses Azure savings-plans
// re-drive (asserted at the end).

// aliasRecorder fakes savingsplans.SavingsPlanOrderAliasAPI. It records the
// alias name each purchase PUTs, keyed by the hourly commitment (each fixture
// recommendation has its own), because the fan-out runs recommendations in
// parallel and call order is not stable.
type aliasRecorder struct {
	mu    sync.Mutex
	names map[float64][]string
}

func (r *aliasRecorder) BeginCreate(_ context.Context, name string, body armbillingbenefits.SavingsPlanOrderAliasModel, _ *armbillingbenefits.SavingsPlanOrderAliasClientBeginCreateOptions) (savingsplans.SavingsPlanOrderAliasPoller, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	amount := *body.Properties.Commitment.Amount
	r.names[amount] = append(r.names[amount], name)
	id := "/providers/Microsoft.BillingBenefits/savingsPlanOrderAliases/" + name
	return aliasPoller{id: id}, nil
}

type aliasPoller struct{ id string }

func (p aliasPoller) PollUntilDone(context.Context, *savingsplans.PollOptions) (armbillingbenefits.SavingsPlanOrderAliasClientCreateResponse, error) {
	return armbillingbenefits.SavingsPlanOrderAliasClientCreateResponse{
		SavingsPlanOrderAliasModel: armbillingbenefits.SavingsPlanOrderAliasModel{ID: &p.id},
	}, nil
}

// azureSavingsPlansExec is a persisted two-recommendation Azure savings-plans
// execution on lineage. Each recommendation carries its own hourly commitment
// so aliasRecorder can tell them apart.
func azureSavingsPlansExec(t *testing.T, service, lineage string) *config.PurchaseExecution {
	t.Helper()
	details := func(hourly float64) json.RawMessage {
		raw, err := json.Marshal(common.SavingsPlanDetails{PlanType: "Compute_Savings_Plan", HourlyCommitment: hourly, Currency: "USD"})
		require.NoError(t, err)
		return raw
	}
	recs := []config.RecommendationRecord{
		{ID: "rec-a", Provider: "azure", Service: service, ResourceType: "sp-a", Region: "westeurope", Count: 1, Term: 1, Selected: true, Details: details(1.5)},
		{ID: "rec-b", Provider: "azure", Service: service, ResourceType: "sp-b", Region: "westeurope", Count: 1, Term: 3, Selected: true, Details: details(2.5)},
	}
	return &config.PurchaseExecution{ExecutionID: "exec-" + lineage, IdempotencyKey: lineage, Recommendations: recs}
}

// purchaseThroughPinnedAzureClient runs exec through processPurchaseRecommendations
// with the real pinned savings-plans client behind a mock provider, and returns
// the alias name PUT for each hourly commitment.
func purchaseThroughPinnedAzureClient(t *testing.T, exec *config.PurchaseExecution) map[float64][]string {
	t.Helper()
	ctx := context.Background()
	recorder := &aliasRecorder{names: map[float64][]string{}}
	client := savingsplans.NewClient(nil, "sub-placeholder", "westeurope")
	client.SetOrderAliasClient(recorder)

	store, sender, factory, prov := new(MockConfigStore), new(MockEmailSender), new(MockProviderFactory), new(MockProvider)
	store.SavePurchaseExecutionFn = func(context.Context, *config.PurchaseExecution) error { return nil }
	store.On("SavePurchaseHistory", ctx, mock.Anything).Return(nil)
	factory.On("CreateAndValidateProvider", mock.Anything, "azure", mock.Anything).Return(prov, nil)
	// Azure's provider hands out its savings-plans client for exactly this
	// service type (see TestRedriveGuardReachMatchesDispatchReach). Matching
	// it here makes the test fail if the platform stopped dispatching the
	// slug there.
	prov.On("GetServiceClient", mock.Anything, common.ServiceSavingsPlansAll, mock.Anything).Return(provider.ServiceClient(client), nil)

	m := &Manager{config: store, email: sender, providerFactory: factory}
	_, _, errs, err := m.processPurchaseRecommendations(ctx, exec, &config.PurchasePlan{Name: "p"}, "acct", (*provider.ProviderConfig)(nil))
	require.NoError(t, err)
	require.Empty(t, errs)
	for i, rec := range exec.Recommendations {
		require.True(t, rec.Purchased, "recommendation %d must reach the fake alias API and succeed", i)
	}
	return recorder.names
}

func TestAzureSavingsPlansAliasIsDerivedFromTheRedriveToken(t *testing.T) {
	for _, service := range []string{"savings-plans", "savingsplans"} {
		t.Run(service, func(t *testing.T) {
			first := purchaseThroughPinnedAzureClient(t, azureSavingsPlansExec(t, service, "lineage-1"))
			redrive := purchaseThroughPinnedAzureClient(t, azureSavingsPlansExec(t, service, "lineage-1"))

			want := map[float64][]string{}
			for i, hourly := range []float64{1.5, 2.5} {
				alias, err := common.OrderIDFromToken(common.DeriveIdempotencyToken("lineage-1", i), "")
				require.NoError(t, err)
				want[hourly] = []string{alias}
			}
			assert.Equal(t, want, first, "each purchase PUTs the alias derived from DeriveIdempotencyToken(lineage, index)")
			assert.Equal(t, first, redrive, "a re-drive of the same lineage PUTs the identical alias names")
			assert.NotEqual(t, first[1.5], first[2.5], "each recommendation gets its own alias")
			for _, names := range first {
				for _, name := range names {
					assert.False(t, strings.HasPrefix(name, "cudly-"),
						"alias %q is the timestamp fallback, so the token never reached Azure", name)
				}
			}

			// A different lineage (a new purchase, not a re-drive) must not
			// collide with the first one, or Azure would hand back the
			// earlier order instead of buying.
			other := purchaseThroughPinnedAzureClient(t, azureSavingsPlansExec(t, service, "lineage-2"))
			for hourly, names := range other {
				assert.NotEqual(t, first[hourly], names, "lineage-2 must not reuse lineage-1's alias for hourly=%s", strconv.FormatFloat(hourly, 'f', -1, 64))
			}

			// The alias is deterministic, but nothing here proves Azure
			// collapses a repeat PUT onto the first order, so automatic
			// re-drive and Retry stay refused for this slug. Relaxing that
			// is an owner decision on #838, not a side effect of a bump.
			assert.NotEmpty(t, recRedriveRefusalReason(config.RecommendationRecord{Provider: "azure", Service: service}),
				"Azure savings-plans re-drive must stay refused until a repeat alias PUT is proven to dedupe")
		})
	}
}
