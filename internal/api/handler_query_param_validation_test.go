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

// Issue #312 (audit A02-021): the dashboard summary, the inventory reads and
// the account lists accepted any provider (and the account lists any
// "enabled") and answered 200 with an empty or unfiltered result, where the
// recommendations, history, analytics and config endpoints answer 400 for the
// same parameter. "all" is the documented "every provider" value of
// validateProvider, so these reads now treat it like an absent provider
// instead of filtering on the literal string "all".

func requireBadRequest(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "must be a 400 client error, got: %v", err)
	assert.Equal(t, 400, ce.code)
}

func TestHandler_getDashboardSummary_RejectsUnknownProvider(t *testing.T) {
	ctx := context.Background()
	for _, provider := range []string{"AWS", "ibm", "aws "} {
		t.Run(provider, func(t *testing.T) {
			mockScheduler := new(MockScheduler)
			mockStore := new(MockConfigStore)
			mockAuth, req := adminDashboardReq(ctx)
			h := &Handler{auth: mockAuth, scheduler: mockScheduler, config: mockStore}

			_, err := h.getDashboardSummary(ctx, req, map[string]string{"provider": provider})

			requireBadRequest(t, err)
			mockScheduler.AssertNotCalled(t, "ListRecommendations", mock.Anything, mock.Anything)
			mockStore.AssertNotCalled(t, "GetActivePurchaseHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestHandler_getDashboardSummary_AllProviderMeansNoFilter(t *testing.T) {
	ctx := context.Background()
	mockScheduler := new(MockScheduler)
	mockStore := new(MockConfigStore)

	mockScheduler.On("ListRecommendations", mock.Anything, config.RecommendationFilter{Provider: ""}).
		Return([]config.RecommendationRecord{{Provider: "aws", Service: "ec2", ResourceType: "m5.large", Savings: 40}}, nil)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{DefaultCoverage: 80}, nil)
	mockStore.On("GetServiceConfig", ctx, mock.Anything, mock.Anything).Return(nil, config.ErrNotFound)
	mockStore.On("GetActivePurchaseHistory", ctx, mock.Anything, mock.Anything, mock.Anything).
		Return([]config.PurchaseHistoryRecord{{
			Provider: "azure", Service: "compute", AccountID: "acc-1", PurchaseID: "p-1",
			Term: 1, Count: 1, Timestamp: time.Now().AddDate(0, -1, 0), MonthlyCost: float64Ptr(10),
			UpfrontCost: new(float64),
		}}, nil)

	mockAuth, req := adminDashboardReq(ctx)
	h := &Handler{auth: mockAuth, scheduler: mockScheduler, config: mockStore}

	result, err := h.getDashboardSummary(ctx, req, map[string]string{"provider": "all"})

	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalRecommendations)
	assert.Equal(t, 1, result.ActiveCommitments, "provider=all must not drop every commitment")
	mockScheduler.AssertExpectations(t)
}

func inventoryRowsForProviderTests() []config.PurchaseHistoryRecord {
	now := time.Now()
	return []config.PurchaseHistoryRecord{
		{UpfrontCost: new(float64), AccountID: "acc-1", PurchaseID: "p-aws", Provider: "aws", Service: "ec2",
			Timestamp: now.AddDate(0, -3, 0), Term: 1, Count: 1, MonthlyCost: float64Ptr(80)},
		{UpfrontCost: new(float64), AccountID: "acc-1", PurchaseID: "p-azure", Provider: "azure", Service: "compute",
			Timestamp: now.AddDate(0, -3, 0), Term: 1, Count: 1, MonthlyCost: float64Ptr(120)},
	}
}

func TestHandler_inventory_RejectsUnknownProvider(t *testing.T) {
	ctx := context.Background()
	reads := map[string]func(*Handler, map[string]string) error{
		"commitments": func(h *Handler, p map[string]string) error {
			_, req := adminInventoryReq(ctx)
			_, err := h.listActiveCommitments(ctx, req, p)
			return err
		},
		"coverage": func(h *Handler, p map[string]string) error {
			_, req := adminInventoryReq(ctx)
			_, err := h.getCoverageBreakdown(ctx, req, p)
			return err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			mockStore := new(MockConfigStore)
			mockAuth, _ := adminInventoryReq(ctx)
			h := &Handler{auth: mockAuth, config: mockStore, scheduler: new(MockScheduler)}

			err := read(h, map[string]string{"provider": "AWS"})

			requireBadRequest(t, err)
			mockStore.AssertNotCalled(t, "GetActivePurchaseHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestHandler_listActiveCommitments_AllProviderMeansNoFilter(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockStore.On("GetActivePurchaseHistory", ctx, mock.AnythingOfType("time.Time"), []string(nil), map[string][]string(nil)).
		Return(inventoryRowsForProviderTests(), nil)
	mockStore.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{{ID: "acc-1", Name: "Account One"}}, nil
	}
	mockAuth, req := adminInventoryReq(ctx)
	h := &Handler{auth: mockAuth, config: mockStore}

	result, err := h.listActiveCommitments(ctx, req, map[string]string{"provider": "all"})

	require.NoError(t, err)
	assert.Len(t, result.(InventoryCommitmentsResponse).Commitments, 2)
}

func TestHandler_getCoverageBreakdown_AllProviderMeansNoFilter(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockStore.On("GetActivePurchaseHistory", ctx, mock.AnythingOfType("time.Time"), []string(nil), map[string][]string(nil)).
		Return(inventoryRowsForProviderTests(), nil)
	mockStore.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{{ID: "acc-1", Name: "Account One"}}, nil
	}
	mockScheduler := new(MockScheduler)
	mockScheduler.On("ListRecommendations", mock.Anything, mock.Anything).
		Return([]config.RecommendationRecord{{Provider: "aws", Service: "ec2", Savings: 20}}, nil)
	mockAuth, req := adminInventoryReq(ctx)
	h := &Handler{auth: mockAuth, config: mockStore, scheduler: mockScheduler}

	result, err := h.getCoverageBreakdown(ctx, req, map[string]string{"provider": "all"})
	require.NoError(t, err)

	want, err := h.getCoverageBreakdown(ctx, req, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, want, result, "provider=all must read the same as no provider")
}

func TestListAccounts_RejectsUnknownEnumParams(t *testing.T) {
	ctx := context.Background()
	cases := map[string]map[string]string{
		"enabled=1":     {"enabled": "1"},
		"enabled=yes":   {"enabled": "yes"},
		"enabled=TRUE":  {"enabled": "TRUE"},
		"provider=AWS":  {"provider": "AWS"},
		"provider=ibm":  {"provider": "ibm"},
		"both invalid":  {"provider": "x", "enabled": "x"},
		"provider=aws ": {"provider": "aws "},
	}
	lists := map[string]func(*Handler, map[string]string) error{
		"full": func(h *Handler, p map[string]string) error {
			req := adminRequest("")
			req.QueryStringParameters = p
			_, err := h.listAccounts(ctx, req)
			return err
		},
		"minimal": func(h *Handler, p map[string]string) error {
			req := adminRequest("")
			req.QueryStringParameters = p
			_, err := h.listAccountsMinimal(ctx, req)
			return err
		},
	}
	for listName, list := range lists {
		for name, params := range cases {
			t.Run(listName+" "+name, func(t *testing.T) {
				mockAuth := new(MockAuthService)
				setupAdminAuth(ctx, mockAuth)
				store := new(MockConfigStore)
				listed := false
				store.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
					listed = true
					return nil, nil
				}
				h := &Handler{auth: mockAuth, config: store}
				h.sourceIdentityOnce.Do(func() {}) // no STS lookup from markSelfAccount

				requireBadRequest(t, list(h, params))
				assert.False(t, listed, "an invalid filter must be refused before the store is read")
			})
		}
	}
}

func TestListAccounts_AcceptedFilterValues(t *testing.T) {
	ctx := context.Background()
	listWith := func(t *testing.T, params map[string]string) config.CloudAccountFilter {
		t.Helper()
		mockAuth := new(MockAuthService)
		setupAdminAuth(ctx, mockAuth)
		store := new(MockConfigStore)
		var got config.CloudAccountFilter
		store.ListCloudAccountsFn = func(_ context.Context, f config.CloudAccountFilter) ([]config.CloudAccount, error) {
			got = f
			return nil, nil
		}
		req := adminRequest("")
		req.QueryStringParameters = params
		h := &Handler{auth: mockAuth, config: store}
		h.sourceIdentityOnce.Do(func() {}) // no STS lookup from markSelfAccount
		_, err := h.listAccounts(ctx, req)
		require.NoError(t, err)
		return got
	}

	t.Run("provider=all is no provider filter", func(t *testing.T) {
		assert.Nil(t, listWith(t, map[string]string{"provider": "all"}).Provider)
	})

	t.Run("empty enabled is no enabled filter", func(t *testing.T) {
		assert.Nil(t, listWith(t, map[string]string{"enabled": ""}).Enabled)
	})

	t.Run("valid provider and enabled pass through", func(t *testing.T) {
		f := listWith(t, map[string]string{"provider": "gcp", "enabled": "false", "search": "prod"})
		require.NotNil(t, f.Provider)
		assert.Equal(t, "gcp", *f.Provider)
		require.NotNil(t, f.Enabled)
		assert.False(t, *f.Enabled)
		assert.Equal(t, "prod", f.Search)
	})
}
