package purchase

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func heldFreshness(heldIDs ...string) *config.RecommendationsFreshness {
	return &config.RecommendationsFreshness{LastCollectionPartial: &config.RecommendationsCollectionPartial{
		Providers: []config.PartialProviderSweep{{
			Provider: "azure", IncompleteAccounts: len(heldIDs), HeldAccountIDs: heldIDs,
			Reasons: map[string]int{config.PartialReasonPricingUnavailable: 1},
		}},
	}}
}

// An Advisor row held through a pricing outage still matches the approved
// execution, because both carry the same old price. The collection state is
// what tells the purchase path the price may be days old.
func TestAzurePricingHold(t *testing.T) {
	acct := "11111111-1111-1111-1111-111111111111"
	other := "22222222-2222-2222-2222-222222222222"
	cases := []struct {
		name      string
		execAcct  *string
		fresh     *config.RecommendationsFreshness
		freshErr  error
		wantStale bool
	}{
		{"held account refused", &acct, heldFreshness(acct), nil, true},
		{"hold cleared by a good cycle", &acct, &config.RecommendationsFreshness{}, nil, false},
		{"unheld account unaffected", &other, heldFreshness(acct), nil, false},
		{"unreadable hold state refuses", &acct, nil, errors.New("database unavailable"), true},
		{"missing collection state refuses", &acct, nil, nil, true},
		// The reason gate: only a pricing_unavailable sweep holds rows. A sweep that
		// is partial for another reason (here throttling) must not refuse a purchase.
		{"partial sweep without the pricing reason does not refuse", &acct, &config.RecommendationsFreshness{LastCollectionPartial: &config.RecommendationsCollectionPartial{
			Providers: []config.PartialProviderSweep{{Provider: "azure", HeldAccountIDs: []string{acct}, Reasons: map[string]int{config.PartialReasonThrottled: 1}}},
		}}, nil, false},
		{"unresolved ambient rec refused during an outage", nil, heldFreshness(), nil, true},
		// Fail-closed by design: a rec without an account cannot be matched to the
		// hold list, so any Azure hold refuses it.
		{"accountless rec refused while other accounts are held", nil, heldFreshness(acct), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := config.RecommendationRecord{ID: "azure-held", Provider: "azure", Service: "compute", Region: "westeurope",
				ResourceType: "Standard_D2s_v3", Count: 2, Term: 3, UpfrontCost: 200, Selected: true, CloudAccountID: tc.execAcct}
			exec := armedExecution("azure", "compute", 0, "approved")
			exec.Recommendations = []config.RecommendationRecord{base}
			mgr, store, recorder := armedHarness(t, common.ServiceCompute)
			store.On("ListStoredRecommendations", mock.Anything, config.RecommendationFilter{Provider: "azure", RequireAzurePricingMigration: true}).
				Return([]config.RecommendationRecord{base}, nil).Maybe()
			store.On("GetRecommendationsFreshness", mock.Anything).Return(tc.fresh, tc.freshErr).Maybe()

			err := mgr.executeAndFinalize(context.Background(), exec)

			if tc.wantStale {
				require.ErrorIs(t, err, ErrStaleAzurePricing)
				assert.Zero(t, recorder.count())
				assert.Equal(t, "failed", exec.Status)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 1, recorder.count())
		})
	}
}
