package purchase

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestAzurePricingRefusal(t *testing.T) {
	base := config.RecommendationRecord{ID: "azure-current", Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: "Standard_D2s_v3", Count: 2, Term: 3, UpfrontCost: 200, Selected: true}
	cases := []struct {
		name     string
		change   func(*config.RecommendationRecord, *config.RecommendationRecord)
		missing  bool
		storeErr error
	}{
		{"invalidated", func(_, r *config.RecommendationRecord) { r.UpfrontCost = 0 }, false, nil},
		{"repriced", func(_, r *config.RecommendationRecord) { r.UpfrontCost = 201 }, false, nil},
		{"missing", nil, true, nil},
		{"zero count", func(_, r *config.RecommendationRecord) { r.Count = 0 }, false, nil},
		{"negative price", func(_, r *config.RecommendationRecord) { r.UpfrontCost = -1 }, false, nil},
		{"nan price", func(_, r *config.RecommendationRecord) { r.UpfrontCost = math.NaN() }, false, nil},
		{"infinite price", func(_, r *config.RecommendationRecord) { r.UpfrontCost = math.Inf(1) }, false, nil},
		{"monthly nil differs from zero", func(_, r *config.RecommendationRecord) { r.MonthlyCost = new(float64) }, false, nil},
		{"persisted negative price", func(r, _ *config.RecommendationRecord) { r.UpfrontCost = -1 }, false, nil},
		{"persisted nan price", func(r, _ *config.RecommendationRecord) { r.UpfrontCost = math.NaN() }, false, nil},
		{"persisted infinite price", func(r, _ *config.RecommendationRecord) { r.UpfrontCost = math.Inf(1) }, false, nil},
		{"persisted zero count", func(r, _ *config.RecommendationRecord) { r.Count = 0 }, false, nil},
		{"persisted negative count", func(r, _ *config.RecommendationRecord) { r.Count = -1 }, false, nil},
		{"negative subnormal upfront", func(req, stored *config.RecommendationRecord) {
			req.Count, req.UpfrontCost, req.MonthlyCost, stored.UpfrontCost, stored.MonthlyCost = 1, 0, new(50.0), -math.SmallestNonzeroFloat64, new(100.0)
		}, false, nil},
		{"negative subnormal monthly", func(req, stored *config.RecommendationRecord) {
			req.Count, req.UpfrontCost, req.MonthlyCost, stored.MonthlyCost = 1, 100, new(0.0), new(-math.SmallestNonzeroFloat64)
		}, false, nil},
		{"store error", nil, false, errors.New("database unavailable")},
		{"migration not ready", nil, false, fmt.Errorf("readiness: %w", config.ErrAzurePricingNotReady)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := armedExecution("azure", "compute", 0, "approved")
			exec.Recommendations = []config.RecommendationRecord{base}
			mgr, store, recorder := armedHarness(t, common.ServiceCompute)
			stored := base
			if tc.change != nil {
				tc.change(&exec.Recommendations[0], &stored)
			}
			rows := []config.RecommendationRecord{stored}
			if tc.missing {
				rows = nil
			}
			store.On("ListStoredRecommendations", mock.Anything, config.RecommendationFilter{Provider: "azure", RequireAzurePricingMigration: true}).Return(rows, tc.storeErr).Maybe()
			err := mgr.executeAndFinalize(context.Background(), exec)
			assert.Zero(t, recorder.count())
			assert.Equal(t, "failed", exec.Status)
			if tc.storeErr != nil {
				assert.ErrorIs(t, err, tc.storeErr)
				if errors.Is(tc.storeErr, config.ErrAzurePricingNotReady) {
					assert.ErrorIs(t, err, ErrStaleAzurePricing)
				} else {
					assert.NotErrorIs(t, err, ErrStaleAzurePricing)
				}
			} else {
				assert.ErrorIs(t, err, ErrStaleAzurePricing)
			}
		})
	}
}

func TestAzurePricingRefusalEntryPoints(t *testing.T) {
	for _, entry := range []string{"approve", "direct", "empty ID", "audit save failure"} {
		t.Run(entry, func(t *testing.T) {
			exec := armedExecution("azure", "compute", 0, "pending")
			exec.Recommendations[0].ID = "azure-current"
			if entry == "empty ID" {
				exec.Recommendations[0].ID = ""
			}
			mgr, store, recorder := armedHarness(t, common.ServiceCompute)
			store.On("ListStoredRecommendations", mock.Anything, config.RecommendationFilter{Provider: "azure", RequireAzurePricingMigration: true}).Return([]config.RecommendationRecord{}, nil).Maybe()
			store.On("GetExecutionByID", mock.Anything, exec.ExecutionID).Return(exec, nil).Maybe()
			var saved *config.PurchaseExecution
			store.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
				copy := *e
				saved = &copy
				if entry == "audit save failure" {
					return errors.New("save failed")
				}
				return nil
			}
			expectClaim(store, exec, []string{"pending", "notified"})
			var err error
			if entry == "approve" {
				_, _, err = mgr.ApproveAndExecute(context.Background(), exec.ExecutionID, "operator@example.com", nil, nil)
			} else {
				_, _, err = mgr.DirectExecute(context.Background(), exec.ExecutionID, "operator@example.com", nil, nil)
			}
			assert.Zero(t, recorder.count())
			require.Error(t, err)
			require.NotNil(t, saved)
			assert.Equal(t, "failed", saved.Status)
			if entry == "audit save failure" {
				assert.ErrorIs(t, err, config.ErrAuditLoss)
			}
		})
	}
}

func TestAzurePricingUnchangedStillPurchases(t *testing.T) {
	for _, variant := range []struct {
		name, provider string
		upfront        float64
		monthly        *float64
	}{
		{"azure", "azure", 200, nil}, {"aws", "aws", 200, nil}, {"inexact Azure upfront", "azure", 39.8, nil},
		{"monthly", "azure", 0, new(200.0)}, {"upfront zero monthly", "azure", 200, new(0.0)},
	} {
		for _, count := range []int{1, 2, 3, 4, 5} {
			t.Run(fmt.Sprintf("%s/collected-%d", variant.name, count), func(t *testing.T) {
				exec := armedExecution(variant.provider, "compute", 0, "approved")
				exec.PlanID = ""
				exec.Recommendations[0].ID = "current"
				exec.Recommendations[0].Count = 2
				exec.Recommendations[0].UpfrontCost = variant.upfront
				exec.Recommendations[0].MonthlyCost = variant.monthly
				mgr, store, recorder := armedHarness(t, common.ServiceCompute)
				stored := exec.Recommendations[0]
				stored.Count = count
				stored.UpfrontCost = variant.upfront * float64(count) / 2
				if variant.monthly != nil {
					stored.MonthlyCost = new(*variant.monthly * float64(count) / 2)
				}
				if variant.provider == "azure" {
					store.On("ListStoredRecommendations", mock.Anything, config.RecommendationFilter{Provider: "azure", RequireAzurePricingMigration: true}).Return([]config.RecommendationRecord{stored}, nil).Once()
				}
				require.NoError(t, mgr.executeAndFinalize(context.Background(), exec))
				assert.Equal(t, 1, recorder.count())
				if variant.provider == "aws" {
					store.AssertNotCalled(t, "ListStoredRecommendations", mock.Anything, mock.Anything)
				}
				store.AssertExpectations(t)
			})
		}
	}
}

func TestAzureCostEqualRoundingBound(t *testing.T) {
	within := 100.0
	for i := 0; i < 8; i++ {
		within = math.Nextafter(within, math.Inf(1))
	}
	beyond := math.Nextafter(within, math.Inf(1))
	assert.True(t, azureCostEqual(100, within, 1))
	assert.False(t, azureCostEqual(100, beyond, 1))
}
