package api

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestPurchaseHistoryUnknownUpfront(t *testing.T) {
	for _, costs := range [][]*float64{{nil}, {nil, new(float64(120))}, {new(float64(120)), nil}} {
		rows := make([]config.PurchaseHistoryRecord, len(costs))
		for i, cost := range costs {
			rows[i] = config.PurchaseHistoryRecord{UpfrontCost: cost, EstimatedSavings: 10}
		}
		summary := summarizePurchaseHistory(rows)
		assert.Nil(t, summary.TotalUpfront)
		assert.Equal(t, len(costs), summary.TotalCompleted)
		assert.Equal(t, 10*float64(len(costs)), summary.TotalMonthlySavings)
	}
	for _, rows := range [][]config.PurchaseHistoryRecord{nil, {{UpfrontCost: new(float64)}}, {{Status: "pending"}}} {
		summary := summarizePurchaseHistory(rows)
		require.NotNil(t, summary.TotalUpfront)
		assert.Zero(t, *summary.TotalUpfront)
	}
}

func TestCoverageUnknownUpfront(t *testing.T) {
	assert.Nil(t, commitmentCoveredMonthly(config.PurchaseHistoryRecord{Term: 1, MonthlyCost: new(float64(50))}))
	known := commitmentCoveredMonthly(config.PurchaseHistoryRecord{Term: 1, UpfrontCost: new(float64), MonthlyCost: new(float64(50))})
	require.NotNil(t, known)
	assert.Equal(t, 50.0, *known)

	for _, unknownService := range []string{"a", "z"} {
		resp := buildCoverageBreakdown(map[string]*float64{
			"aws:" + unknownService: nil,
			"aws:m":                 new(float64(50)),
			"azure:compute":         new(float64(50)),
		}, map[string]float64{"gcp:compute": 100, "azure:compute": 50})
		for _, provider := range resp.Providers {
			switch provider.Provider {
			case "aws":
				assert.Nil(t, provider.OverallCoveragePct)
			case "azure":
				require.NotNil(t, provider.OverallCoveragePct)
				assert.Equal(t, 50.0, *provider.OverallCoveragePct)
			case "gcp":
				require.NotNil(t, provider.OverallCoveragePct)
				assert.Zero(t, *provider.OverallCoveragePct)
				require.Len(t, provider.Services, 1)
				require.NotNil(t, provider.Services[0].CoveredMonthly)
				assert.Zero(t, *provider.Services[0].CoveredMonthly)
			}
		}
	}
}

func TestCoverageHandlerUnknownUpfrontSameService(t *testing.T) {
	for _, unknownFirst := range []bool{false, true} {
		name := "known first"
		if unknownFirst {
			name = "unknown first"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, scheduler := &MockConfigStore{}, &MockScheduler{}
			known := config.PurchaseHistoryRecord{AccountID: "acc-1", Provider: "aws", Service: "ec2", Timestamp: time.Now().Add(-time.Hour), Term: 1, UpfrontCost: new(float64(120)), MonthlyCost: new(float64(50))}
			unknown := known
			unknown.UpfrontCost = nil
			purchases := []config.PurchaseHistoryRecord{known, unknown}
			if unknownFirst {
				purchases[0], purchases[1] = purchases[1], purchases[0]
			}
			store.On("GetActivePurchaseHistory", ctx, mock.AnythingOfType("time.Time"), []string(nil), map[string][]string(nil)).Return(purchases, nil)
			store.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
				return []config.CloudAccount{}, nil
			}
			scheduler.On("ListRecommendations", mock.Anything, config.RecommendationFilter{}).Return([]config.RecommendationRecord{{Provider: "aws", Service: "ec2", Savings: 100}}, nil)
			auth, req := adminInventoryReq(t, ctx)
			handler := &Handler{auth: auth, config: store, scheduler: scheduler}
			result, err := handler.getCoverageBreakdown(ctx, req, map[string]string{})
			require.NoError(t, err)
			resp, ok := result.(CoverageBreakdownResponse)
			require.True(t, ok)
			require.Len(t, resp.Providers, 3)
			aws := resp.Providers[0]
			require.Equal(t, "aws", aws.Provider)
			require.Len(t, aws.Services, 1)
			assert.Nil(t, aws.Services[0].CoveredMonthly)
			assert.Nil(t, aws.Services[0].CoveragePct)
			assert.Equal(t, 100.0, aws.Services[0].OnDemandMonthly)
			assert.Nil(t, aws.OverallCoveragePct)
			store.AssertExpectations(t)
			scheduler.AssertExpectations(t)
		})
	}
}

func TestMarketplaceUnknownUpfront(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		cost       *float64
		allowed    bool
	}{
		{name: "unknown default"},
		{name: "unknown valid custom", body: `{"price_schedule":[{"term_months":1,"price":100}]}`},
		{name: "zero valid custom", body: `{"price_schedule":[{"term_months":1,"price":100}]}`, cost: new(float64), allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, authSvc, ec2 := &MockConfigStore{}, &MockAuthService{}, &stubMarketplaceEC2{}
			adminSession(t, authSvc)
			row := standardRow()
			row.UpfrontCost = tc.cost
			store.On("GetPurchaseHistoryByPurchaseID", mock.Anything, validMarketplacePurchaseID).Return(row, nil)
			if tc.allowed {
				store.On("ClaimMarketplaceListingSlot", mock.Anything, validMarketplacePurchaseID, mock.Anything, mock.Anything).Return(&config.MarketplaceListingClaim{ClientToken: "tok"}, nil)
				store.On("UpdatePurchaseHistoryListing", mock.Anything, validMarketplacePurchaseID, "ril-default", config.ListingStateActive).Return(nil)
			}
			req := marketplaceReq()
			req.Body = tc.body
			_, err := newMarketplaceHandler(store, authSvc, ec2).marketplaceList(context.Background(), req, validMarketplacePurchaseID)
			if tc.allowed {
				require.NoError(t, err)
				assert.Equal(t, 1, ec2.createCallCount)
			} else {
				require.ErrorContains(t, err, "upfront cost is unknown")
				assert.Zero(t, ec2.createCallCount)
				store.AssertNotCalled(t, "ClaimMarketplaceListingSlot", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			}
			store.AssertExpectations(t)
		})
	}
}
