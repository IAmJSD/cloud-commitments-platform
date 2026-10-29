package scheduler

import (
	"context"
	"sync"
	"testing"

	awsrec "github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	awscostexplorer "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// The AWS per-service fallback must never hand a non-AWS payment option
// (Azure/GCP "monthly"/"upfront") to the AWS recommendations client (#1229).
func TestFetchAndConvert_AWSFallbackPaymentOption(t *testing.T) {
	cases := []struct{ stored, want string }{
		{"monthly", "no-upfront"},
		{"upfront", "all-upfront"},
		{"", "no-upfront"},
		{"bogus", "no-upfront"},
		{"all-upfront", "all-upfront"},
		{"partial-upfront", "partial-upfront"},
		{"no-upfront", "no-upfront"},
	}
	for _, tc := range cases {
		t.Run("stored_"+tc.stored, func(t *testing.T) {
			var mu sync.Mutex
			var got []string
			client := new(MockRecommendationsClient)
			t.Cleanup(func() { client.AssertExpectations(t) })
			client.On("GetAllRecommendations", mock.Anything).Return([]common.Recommendation{}, nil)
			client.On("GetRecommendations", mock.Anything, mock.MatchedBy(func(p *common.RecommendationParams) bool {
				mu.Lock()
				defer mu.Unlock()
				got = append(got, p.PaymentOption)
				return true
			})).Return([]common.Recommendation{}, nil)

			s := &Scheduler{}
			_, _, err := s.fetchAndConvert(context.Background(), newRecProvider(t, client), "aws", nil, "",
				&config.GlobalConfig{DefaultTerm: 1, DefaultPayment: tc.stored, RecommendationsLookbackDays: 30})
			require.NoError(t, err)
			require.NotEmpty(t, got)
			for _, g := range got {
				assert.Equal(t, tc.want, g)
			}
		})
	}
}

// spPaymentCE is a Cost Explorer stub: every call succeeds and the Savings
// Plans payment option is recorded. Unstubbed methods panic via the nil
// embedded interface.
type spPaymentCE struct {
	awsrec.CostExplorerAPI
	mu        sync.Mutex
	spPayment []types.PaymentOption
}

func (c *spPaymentCE) GetReservationPurchaseRecommendation(context.Context, *awscostexplorer.GetReservationPurchaseRecommendationInput, ...func(*awscostexplorer.Options)) (*awscostexplorer.GetReservationPurchaseRecommendationOutput, error) {
	return &awscostexplorer.GetReservationPurchaseRecommendationOutput{}, nil
}

func (c *spPaymentCE) GetSavingsPlansPurchaseRecommendation(_ context.Context, in *awscostexplorer.GetSavingsPlansPurchaseRecommendationInput, _ ...func(*awscostexplorer.Options)) (*awscostexplorer.GetSavingsPlansPurchaseRecommendationOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spPayment = append(c.spPayment, in.PaymentOption)
	return &awscostexplorer.GetSavingsPlansPurchaseRecommendationOutput{}, nil
}

// Savings Plans rejects an empty payment option, so the fallback must hand the
// real AWS client a concrete one for every stored value (#1229).
func TestFetchAndConvert_AWSFallbackRealClientSavingsPlans(t *testing.T) {
	for _, stored := range []string{"monthly", "upfront", "", "no-upfront", "partial-upfront", "all-upfront"} {
		t.Run("stored_"+stored, func(t *testing.T) {
			ce := &spPaymentCE{}
			prov := new(MockProvider)
			t.Cleanup(func() { prov.AssertExpectations(t) })
			prov.On("GetRecommendationsClient", mock.Anything).Return(awsrec.NewClientWithAPI(ce, "us-east-1"), nil)
			// The stub returns nothing, so GetAllRecommendations is empty and
			// the per-service fallback runs against the real client.
			s := &Scheduler{}
			_, _, err := s.fetchAndConvert(context.Background(), prov, "aws", nil, "", &config.GlobalConfig{DefaultTerm: 1, DefaultPayment: stored, RecommendationsLookbackDays: 30})
			require.NoError(t, err)
			require.NotEmpty(t, ce.spPayment)
			for _, p := range ce.spPayment {
				assert.Contains(t, []types.PaymentOption{types.PaymentOptionNoUpfront, types.PaymentOptionPartialUpfront, types.PaymentOptionAllUpfront}, p)
			}
		})
	}
}
