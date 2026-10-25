package api

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Account-scope regression tests for GET /api/plans, the inventory commitment
// read and the RI-exchange quote (issues #29, #384).

const (
	planInScope    = "a1111111-1111-4111-8111-111111111111"
	planOutOfScope = "a2222222-2222-4222-8222-222222222222"
	planUnassigned = "a3333333-3333-4333-8333-333333333333"
	planBothScopes = "a4444444-4444-4444-8444-444444444444"
)

// planListStore applies buildListPlansQuery's predicate, so a wrong filter
// fails by the plans returned rather than by an unstubbed-call panic.
type planListStore struct {
	*MockConfigStore
	plans    []config.PurchasePlan
	accounts map[string][]string
	filters  []config.PurchasePlanFilter
}

func (s *planListStore) ListPurchasePlans(_ context.Context, f config.PurchasePlanFilter) ([]config.PurchasePlan, error) {
	s.filters = append(s.filters, f)
	out := []config.PurchasePlan{}
	for _, p := range s.plans {
		accts := s.accounts[p.ID]
		matches := slices.ContainsFunc(accts, func(a string) bool { return slices.Contains(f.AccountIDs, a) })
		if len(f.AccountIDs) == 0 || matches || (f.IncludeUnassigned && len(accts) == 0) {
			out = append(out, p)
		}
	}
	return out, nil
}

func newPlanListStore(base *MockConfigStore) *planListStore {
	base.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{inScopeAccount(), outOfScopeAccount()}, nil
	}
	base.On("CountExecutionsByPlanAndStatus", mock.Anything, mock.Anything, mock.Anything).
		Return(map[string]config.ExecutionStatusCounts{}, nil).Maybe()
	return &planListStore{
		MockConfigStore: base,
		plans: []config.PurchasePlan{
			{ID: planInScope, Name: "in scope"},
			{ID: planOutOfScope, Name: "other tenant"},
			{ID: planUnassigned, Name: "unassigned"},
			{ID: planBothScopes, Name: "shared"},
		},
		accounts: map[string][]string{
			planInScope:    {scopedInAccount},
			planOutOfScope: {scopedOutAccount},
			planBothScopes: {scopedInAccount, scopedOutAccount},
		},
	}
}

func listPlanIDs(t *testing.T, h *Handler, params map[string]string) []string {
	t.Helper()
	resp, err := h.listPlans(context.Background(), scopedRequest(""), params)
	require.NoError(t, err)
	ids := make([]string, 0, len(resp.Plans))
	for _, p := range resp.Plans {
		ids = append(ids, p.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestListPlans_ScopedCallerSeesOnlyInScopePlans(t *testing.T) {
	h, base := scopedHandler(t, scopedInAccount)
	h.config = newPlanListStore(base)

	assert.Equal(t, []string{planInScope, planBothScopes}, listPlanIDs(t, h, map[string]string{}),
		"a scoped caller must see neither another tenant's plans nor unassigned plans")
}

func TestListPlans_ScopedCallerOutOfScopeFilterReturnsNothing(t *testing.T) {
	h, base := scopedHandler(t, scopedInAccount)
	store := newPlanListStore(base)
	h.config = store

	ids := listPlanIDs(t, h, map[string]string{"account_ids": scopedOutAccount})

	assert.Empty(t, ids, "account_ids naming an out-of-scope account must not narrow the list to that tenant")
	assert.Empty(t, store.filters, "an empty intersection must not reach the store, where no filter means all plans")
}

func TestListPlans_UnrestrictedCallerStillSeesUnassignedPlans(t *testing.T) {
	h, base := scopedHandler(t)
	h.config = newPlanListStore(base)

	assert.Equal(t, []string{planInScope, planUnassigned, planBothScopes},
		listPlanIDs(t, h, map[string]string{"account_ids": scopedInAccount}))
}

func TestListPlans_ScopeLookupErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, scopedToken).Return(&Session{UserID: scopedUserID}, nil)
	mockAuth.On("HasPermissionAPI", ctx, scopedUserID, "view", "plans").Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", ctx, scopedUserID).Return(nil, errors.New("auth store unavailable"))
	store := newPlanListStore(new(MockConfigStore))
	h := &Handler{auth: mockAuth, config: store}

	_, err := h.listPlans(ctx, scopedRequest(""), map[string]string{})

	require.Error(t, err)
	_, isClient := IsClientError(err)
	assert.False(t, isClient, "a scope-lookup failure must surface as a server error, got %v", err)
	assert.Empty(t, store.filters)
}

func TestListActiveCommitments_ScopedCallerOutOfScopeAccountFilterSkipsStore(t *testing.T) {
	h, store := scopedHandler(t, scopedInAccount)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{inScopeAccount(), outOfScopeAccount()}, nil
	}
	store.On("GetActivePurchaseHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]config.PurchaseHistoryRecord{}, nil).Maybe()

	_, err := h.listActiveCommitments(context.Background(), scopedRequest(""), map[string]string{"account_id": scopedOutAccount})
	require.NoError(t, err)

	store.AssertNotCalled(t, "GetActivePurchaseHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestListActiveCommitments_ScopedCallerInScopeAccountFilterReachesStore(t *testing.T) {
	h, store := scopedHandler(t, scopedInAccount)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{inScopeAccount(), outOfScopeAccount()}, nil
	}
	store.On("GetActivePurchaseHistory", mock.Anything, mock.Anything, []string{scopedInAccount}, map[string][]string(nil)).
		Return([]config.PurchaseHistoryRecord{}, nil).Once()

	_, err := h.listActiveCommitments(context.Background(), scopedRequest(""), map[string]string{"account_id": scopedInAccount})
	require.NoError(t, err)
	store.AssertExpectations(t)
}

// quoteHandler scopes the caller to scopedInAccount. The injected quote stub
// records every request and fails, so a request that slips past the gate is
// counted instead of reaching EC2, and the test never resolves AWS credentials
// or probes IMDS (#855).
func quoteHandler(t *testing.T, deploymentAccount string) (*Handler, *[]exchange.ExchangeQuoteRequest) {
	h, store := scopedHandler(t, scopedInAccount)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{inScopeAccount(), outOfScopeAccount()}, nil
	}
	h.reshapeAccountResolver = func(_ context.Context) (string, error) { return deploymentAccount, nil }
	// Sealing the once keeps getBaseAWSConfig off LoadDefaultConfig; only the
	// region is read on this path.
	h.awsCfgOnce.Do(func() { h.awsCfg = aws.Config{Region: "eu-west-1"} })
	var quotes []exchange.ExchangeQuoteRequest
	h.exchangeQuoteFn = func(_ context.Context, req exchange.ExchangeQuoteRequest) (*exchange.ExchangeQuoteSummary, error) {
		quotes = append(quotes, req)
		return nil, errors.New("test: quote stub")
	}
	return h, &quotes
}

const quoteBody = `{"ri_ids":["ri-123"],"target_offering_id":"4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91","target_count":1}`

func TestGetExchangeQuote_OutOfScopeDeploymentAccountReturns403(t *testing.T) {
	h, quotes := quoteHandler(t, scopedOutAccount)

	_, err := h.getExchangeQuote(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: scopedRequest("").Headers, Body: quoteBody,
	})

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a ClientError, got: %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.Error(), "allowed accounts")
	assert.Empty(t, *quotes, "an out-of-scope caller must not reach the AWS quote call")
}

func TestGetExchangeQuote_InScopeDeploymentAccountPassesGate(t *testing.T) {
	h, quotes := quoteHandler(t, scopedInAccount)

	_, err := h.getExchangeQuote(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: scopedRequest("").Headers, Body: quoteBody,
	})

	require.Error(t, err, "the quote stub fails, so the quote itself cannot succeed")
	assert.Contains(t, err.Error(), "exchange quote failed", "an in-scope caller must reach the AWS quote call")
	require.Len(t, *quotes, 1, "an in-scope caller must reach the AWS quote call exactly once")
	assert.Equal(t, "eu-west-1", (*quotes)[0].Region, "the quote must use the region resolved from the AWS config")
	assert.Equal(t, []string{"ri-123"}, (*quotes)[0].ReservedIDs)
}
