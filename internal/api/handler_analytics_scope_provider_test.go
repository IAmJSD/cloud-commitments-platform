package api

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// analyticsScopeCall drives GET /api/history/analytics through HandleRequest
// as a session user whose allow-list is `scope`, requesting `accountID`. It
// returns the HTTP status and the (uuid, provider-bucketed external id) filter
// that reached the analytics query, or nil filters when no query ran.
func analyticsScopeCall(t *testing.T, scope []string, accountID string, accounts []config.CloudAccount) (status int, uuids []string, externals map[string][]string) {
	t.Helper()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "tok").Return(&Session{UserID: "u-1", Email: "u@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", mock.Anything, "u-1", mock.Anything, mock.Anything).Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u-1").Return(scope, nil)

	client := new(MockAnalyticsClient)
	client.On("QueryHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]HistoryDataPoint{}, &HistorySummary{}, nil)

	store := new(MockConfigStore)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return accounts, nil
	}

	h := &Handler{auth: mockAuth, analyticsClient: client, config: store}
	resp, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer tok"},
		QueryStringParameters: map[string]string{"account_id": accountID},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: "/api/history/analytics"},
		},
	})
	require.NoError(t, err)
	for _, c := range client.Calls {
		if c.Method == "QueryHistory" {
			uuids, _ = c.Arguments.Get(1).([]string)
			externals, _ = c.Arguments.Get(2).(map[string][]string)
		}
	}
	return resp.StatusCode, uuids, externals
}

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
		for _, scope := range [][]string{{"u-aws"}, {"u-az"}, {"123"}} {
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
			status, uuids, externals := analyticsScopeCall(t, []string{"u-aws"}, "u-aws", accounts)
			assert.Equal(t, 200, status)
			assert.Equal(t, []string{"u-aws"}, uuids)
			assert.Equal(t, map[string][]string{"aws": {"123"}}, externals)

			status, _, _ = analyticsScopeCall(t, []string{"u-aws"}, "u-az", accounts)
			assert.Equal(t, 404, status, "the other provider's account stays out of scope")
		})
	}
}

// A raw external id that exactly one account owns still works for a scoped
// user, by account ID or by a raw external-id entry, and an out-of-scope
// account stays hidden.
func TestHandleRequest_Analytics_UniqueRawExternalID(t *testing.T) {
	t.Parallel()
	in := config.CloudAccount{ID: "u-in", Name: "in", Provider: "aws", ExternalID: "111"}
	out := config.CloudAccount{ID: "u-out", Name: "out", Provider: "aws", ExternalID: "222"}
	accounts := []config.CloudAccount{in, out}

	status, _, externals := analyticsScopeCall(t, []string{"u-in"}, "111", accounts)
	assert.Equal(t, 200, status)
	assert.Equal(t, map[string][]string{"": {"111"}}, externals, "legacy raw-id filter shape is unchanged")

	status, _, _ = analyticsScopeCall(t, []string{"111"}, "111", accounts)
	assert.Equal(t, 200, status)

	status, _, _ = analyticsScopeCall(t, []string{"u-in"}, "222", accounts)
	assert.Equal(t, 404, status)
}

// Two accounts of one provider sharing an external id are ambiguous too.
func TestHandleRequest_Analytics_SameProviderAmbiguousRawExternalID(t *testing.T) {
	t.Parallel()
	a := config.CloudAccount{ID: "u-a", Name: "dev", Provider: "aws", ExternalID: "123"}
	b := config.CloudAccount{ID: "u-b", Name: "prod", Provider: "aws", ExternalID: "123"}
	for _, accounts := range [][]config.CloudAccount{{a, b}, {b, a}} {
		for _, scope := range []string{"u-a", "u-b"} {
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

	status, _, _ := analyticsScopeCall(t, []string{"999"}, "999", accounts)
	assert.Equal(t, 200, status)

	status, _, _ = analyticsScopeCall(t, []string{"u-solo"}, "999", accounts)
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
