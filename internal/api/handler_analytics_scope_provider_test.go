package api

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/analytics"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// analyticsScopeCall drives GET /api/history/analytics through HandleRequest
// as a session user whose allow-list is `scope`, requesting `accountID`. It
// returns the HTTP status and the (uuid, provider-bucketed external id) filter
// that reached the analytics query, or nil filters when no query ran.
func analyticsScopeCall(t *testing.T, scope []string, accountID string, accounts []config.CloudAccount) (status int, uuids []string, externals map[string][]string) {
	t.Helper()
	return analyticsScopeCallPath(t, "/api/history/analytics", scope, accountID, accounts)
}

// analyticsScopeCallPath is analyticsScopeCall for any of the analytics
// endpoints (history/analytics, history/breakdown, analytics/trends). For
// trends it checks that all three snapshot queries got the same filter.
func analyticsScopeCallPath(t *testing.T, path string, scope []string, accountID string, accounts []config.CloudAccount) (status int, uuids []string, externals map[string][]string) {
	t.Helper()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1", Email: "u@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return(scope, nil)

	client := new(MockAnalyticsClient)
	client.On("QueryHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]HistoryDataPoint{}, &HistorySummary{}, nil)
	client.On("QueryBreakdown", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(map[string]BreakdownValue{}, nil)
	snaps := new(MockAnalyticsSnapshotStore)
	snaps.On("QueryMonthlyTotals", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]analytics.MonthlySummary{}, nil)
	snaps.On("QueryByProvider", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]analytics.ProviderBreakdown{}, nil)
	snaps.On("QueryByService", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]analytics.ServiceBreakdown{}, nil)

	store := new(MockConfigStore)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return accounts, nil
	}

	h := &Handler{auth: mockAuth, analyticsClient: client, analyticsSnapshots: snaps, config: store}
	resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer tok"},
		QueryStringParameters: map[string]string{"account_id": accountID},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: path},
		},
	})
	require.NoError(t, err)
	calls := append(append([]mock.Call{}, client.Calls...), snaps.Calls...)
	for i, c := range calls {
		u, _ := c.Arguments.Get(1).([]string)
		e, _ := c.Arguments.Get(2).(map[string][]string)
		if i > 0 {
			assert.Equal(t, uuids, u, "%s got a different uuid filter", c.Method)
			assert.Equal(t, externals, e, "%s got a different external-id filter", c.Method)
		}
		uuids, externals = u, e
	}
	return resp.StatusCode, uuids, externals
}

// analyticsPaths are the endpoints that run resolveAnalyticsAccountFilter.
var analyticsPaths = []string{"/api/history/analytics", "/api/history/breakdown", "/api/analytics/trends"}

// Issue #520: a raw external id shared by accounts of different providers
// cannot be attributed to one of them. The scope decision used to read the
// account name from a map keyed by external id alone (last listed account
// wins), so it depended on list order, and an allowed request then queried
// every provider's rows for that id.
func TestHandleRequest_Analytics_RawExternalIDSharedAcrossProviders(t *testing.T) {
	t.Parallel()
	awsDev := config.CloudAccount{ID: "u-aws", Name: "dev", Provider: "aws", ExternalID: "123"}
	azProd := config.CloudAccount{ID: "u-az", Name: "prod", Provider: "azure", ExternalID: "123"}
	bothOrders := map[string][]config.CloudAccount{
		"aws listed first":   {awsDev, azProd},
		"azure listed first": {azProd, awsDev},
	}

	for order, accounts := range bothOrders {
		for _, scope := range [][]string{{"dev"}, {"prod"}, {"123"}} {
			t.Run(scope[0]+" scope, "+order, func(t *testing.T) {
				t.Parallel()
				status, uuids, externals := analyticsScopeCall(t, scope, "123", accounts)
				assert.Equal(t, 404, status, "ambiguous raw id must fail closed")
				assert.Nil(t, uuids)
				assert.Nil(t, externals, "no query may run for an ambiguous raw id")
			})
		}

		t.Run("account UUID resolves to its own provider, "+order, func(t *testing.T) {
			t.Parallel()
			status, uuids, externals := analyticsScopeCall(t, []string{"dev"}, "u-aws", accounts)
			assert.Equal(t, 200, status)
			assert.Equal(t, []string{"u-aws"}, uuids)
			assert.Equal(t, map[string][]string{"aws": {"123"}}, externals)

			status, _, _ = analyticsScopeCall(t, []string{"dev"}, "u-az", accounts)
			assert.Equal(t, 404, status, "the other provider's account stays out of scope")
		})
	}
}

// A raw external id that exactly one account owns still works for a scoped
// user, by name or by id, and an out-of-scope account stays hidden.
func TestHandleRequest_Analytics_UniqueRawExternalID(t *testing.T) {
	t.Parallel()
	in := config.CloudAccount{ID: "u-in", Name: "in", Provider: "aws", ExternalID: "111"}
	out := config.CloudAccount{ID: "u-out", Name: "out", Provider: "aws", ExternalID: "222"}
	accounts := []config.CloudAccount{in, out}

	for _, scope := range []string{"in", "111"} {
		status, uuids, externals := analyticsScopeCall(t, []string{scope}, "111", accounts)
		assert.Equal(t, 200, status, scope)
		assert.Nil(t, uuids, scope)
		assert.Equal(t, map[string][]string{"aws": {"111"}}, externals, "%s: raw id is scoped to its account's provider (#624)", scope)
	}

	status, _, _ := analyticsScopeCall(t, []string{"in"}, "222", accounts)

	assert.Equal(t, 404, status)
}

// Two accounts of one provider sharing an external id are ambiguous too.
func TestHandleRequest_Analytics_SameProviderAmbiguousRawExternalID(t *testing.T) {
	t.Parallel()
	a := config.CloudAccount{ID: "u-a", Name: "dev", Provider: "aws", ExternalID: "123"}
	b := config.CloudAccount{ID: "u-b", Name: "prod", Provider: "aws", ExternalID: "123"}
	for _, accounts := range [][]config.CloudAccount{{a, b}, {b, a}} {
		for _, scope := range []string{"dev", "prod"} {
			status, _, externals := analyticsScopeCall(t, []string{scope}, "123", accounts)
			assert.Equal(t, 404, status, scope)
			assert.Nil(t, externals)
		}
	}
}

// Unrestricted sessions are not narrowed: any account_id passes through.
func TestHandleRequest_Analytics_UnrestrictedRawExternalID(t *testing.T) {
	t.Parallel()
	accounts := []config.CloudAccount{
		{ID: "u-aws", Name: "dev", Provider: "aws", ExternalID: "123"},
		{ID: "u-az", Name: "prod", Provider: "azure", ExternalID: "123"},
	}
	status, _, externals := analyticsScopeCall(t, nil, "123", accounts)
	assert.Equal(t, 200, status)
	assert.Equal(t, map[string][]string{"": {"123"}}, externals)
}

// An id that matches no registered account is judged on the literal allow-list
// entry alone.
func TestHandleRequest_Analytics_UnregisteredRawID(t *testing.T) {
	t.Parallel()
	accounts := []config.CloudAccount{{ID: "u-solo", Name: "solo", Provider: "aws", ExternalID: "111"}}

	status, uuids, externals := analyticsScopeCall(t, []string{"999"}, "999", accounts)
	assert.Equal(t, 200, status)
	assert.Nil(t, uuids)
	// No registered account names a provider for it, and the grant is the bare
	// id, so the filter keeps the provider-unconstrained "" bucket.
	assert.Equal(t, map[string][]string{"": {"999"}}, externals)

	status, _, _ = analyticsScopeCall(t, []string{"solo"}, "999", accounts)
	assert.Equal(t, 404, status)
}

// A failed account load must not fall back to literal-id matching.
func TestHandleRequest_Analytics_AccountListFailureIsAnError(t *testing.T) {
	t.Parallel()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1", Email: "u@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return([]string{"999"}, nil)
	store := new(MockConfigStore)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return nil, assert.AnError
	}
	client := new(MockAnalyticsClient)

	h := &Handler{auth: mockAuth, analyticsClient: client, config: store}
	resp, _ := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer tok"},
		QueryStringParameters: map[string]string{"account_id": "999"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: "/api/history/analytics"},
		},
	})
	assert.GreaterOrEqual(t, resp.StatusCode, 500)
	client.AssertNotCalled(t, "QueryHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Issue #624: a scoped user's unique raw external id used to reach every
// analytics query as the provider-unconstrained {"": {id}}, so another
// provider's unregistered account sharing the number leaked its rows. Every
// analytics endpoint now gets the id under the registered account's provider.
func TestHandleRequest_Analytics_RawExternalIDProviderScopedOnEveryEndpoint(t *testing.T) {
	t.Parallel()
	accounts := []config.CloudAccount{
		{ID: "u-gcp", Name: "data", Provider: "gcp", ExternalID: "424242"},
		{ID: "u-aws", Name: "dev", Provider: "aws", ExternalID: "777"},
	}
	for _, path := range analyticsPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			for _, scope := range []string{"data", "424242"} {
				status, uuids, externals := analyticsScopeCallPath(t, path, []string{scope}, "424242", accounts)
				assert.Equal(t, 200, status, scope)
				assert.Nil(t, uuids, scope)
				assert.Equal(t, map[string][]string{"gcp": {"424242"}}, externals, scope)
			}

			status, uuids, externals := analyticsScopeCallPath(t, path, []string{"data"}, "u-gcp", accounts)
			assert.Equal(t, 200, status, "UUID request")
			assert.Equal(t, []string{"u-gcp"}, uuids)
			assert.Equal(t, map[string][]string{"gcp": {"424242"}}, externals)

			status, _, externals = analyticsScopeCallPath(t, path, nil, "424242", accounts)
			assert.Equal(t, 200, status, "unrestricted")
			assert.Equal(t, map[string][]string{"": {"424242"}}, externals, "unrestricted raw id is unchanged")

			status, _, externals = analyticsScopeCallPath(t, path, []string{"data"}, "777", accounts)
			assert.Equal(t, 404, status, "out-of-scope raw id")
			assert.Nil(t, externals)
		})
	}
}

// A registered account with no provider cannot occur on Postgres (CHECK
// constraint), but if one reaches the handler its external id must not fall
// into the provider-unconstrained "" bucket: the raw id is refused and a UUID
// request matches by UUID only.
func TestHandleRequest_Analytics_ProviderlessAccountFailsClosed(t *testing.T) {
	t.Parallel()
	accounts := []config.CloudAccount{{ID: "u-x", Name: "x", ExternalID: "555"}}

	status, _, externals := analyticsScopeCall(t, []string{"x"}, "555", accounts)
	assert.Equal(t, 404, status)
	assert.Nil(t, externals)

	status, uuids, externals := analyticsScopeCall(t, []string{"x"}, "u-x", accounts)
	assert.Equal(t, 200, status)
	assert.Equal(t, []string{"u-x"}, uuids)
	assert.Nil(t, externals)
}
