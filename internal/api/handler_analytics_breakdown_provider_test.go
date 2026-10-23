package api

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #25: GET /history/breakdown read no provider parameter, so the global
// Provider filter could not narrow it the way it narrows /history/analytics
// (issue #498). These tests pin the handler plumbing and the SQL shape; the
// real-Postgres behavior is in analytics_provider_filter_integration_test.go.

// TestHandler_getHistoryBreakdown_ProviderParam asserts the exact provider
// argument QueryBreakdown receives for each form of the query parameter:
// a known provider passes through, while "all" and an absent parameter both
// mean no provider filter.
func TestHandler_getHistoryBreakdown_ProviderParam(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"aws", map[string]string{"provider": "aws"}, "aws"},
		{"azure", map[string]string{"provider": "azure"}, "azure"},
		{"gcp", map[string]string{"provider": "gcp", "dimension": "region"}, "gcp"},
		{"all is no filter", map[string]string{"provider": "all"}, ""},
		{"empty is no filter", map[string]string{"provider": ""}, ""},
		{"absent is no filter", map[string]string{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mockClient := new(MockAnalyticsClient)
			mockClient.On("QueryBreakdown", ctx, []string(nil), map[string][]string(nil), tc.want, mock.Anything, mock.Anything, mock.Anything).
				Return(map[string]BreakdownValue{}, nil)

			mockAuth, req := adminAnalyticsReq(ctx)
			handler := &Handler{auth: mockAuth, analyticsClient: mockClient, config: new(MockConfigStore)}

			_, err := handler.getHistoryBreakdown(ctx, req, tc.params)
			require.NoError(t, err)
			mockClient.AssertNumberOfCalls(t, "QueryBreakdown", 1)
			mockClient.AssertCalled(t, "QueryBreakdown", ctx, []string(nil), map[string][]string(nil), tc.want, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// TestHandler_getHistoryBreakdown_InvalidProvider rejects an unknown provider
// with a 400 before any query runs, as getHistoryAnalytics does, rather than
// silently returning every provider's rows.
func TestHandler_getHistoryBreakdown_InvalidProvider(t *testing.T) {
	for _, provider := range []string{"oracle", "AWS", "aws "} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			mockClient := new(MockAnalyticsClient)

			mockAuth, req := adminAnalyticsReq(ctx)
			handler := &Handler{auth: mockAuth, analyticsClient: mockClient, config: new(MockConfigStore)}

			_, err := handler.getHistoryBreakdown(ctx, req, map[string]string{"provider": provider})
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "want a client error, got %v", err)
			assert.Equal(t, 400, ce.code)
			assert.Contains(t, err.Error(), "invalid provider")
			mockClient.AssertNotCalled(t, "QueryBreakdown", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// TestHandleRequest_HistoryBreakdown_ProviderQueryParam drives the routed
// request so the provider query-string parameter is shown to reach the store.
func TestHandleRequest_HistoryBreakdown_ProviderQueryParam(t *testing.T) {
	ctx := context.Background()
	mockClient := new(MockAnalyticsClient)
	mockClient.On("QueryBreakdown", mock.Anything, []string(nil), map[string][]string(nil), "azure", mock.Anything, mock.Anything, "provider").
		Return(map[string]BreakdownValue{"azure": {TotalSavings: 1}}, nil)

	mockAuth, req := adminAnalyticsReq(ctx)
	req.RequestContext.HTTP.Method = "GET"
	req.RequestContext.HTTP.Path = "/api/history/breakdown"
	req.QueryStringParameters = map[string]string{"provider": "azure", "dimension": "provider"}
	handler := &Handler{auth: mockAuth, analyticsClient: mockClient, config: new(MockConfigStore)}

	resp, err := handler.HandleRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, resp.Body)
	mockClient.AssertCalled(t, "QueryBreakdown", mock.Anything, []string(nil), map[string][]string(nil), "azure", mock.Anything, mock.Anything, "provider")
}

// TestQueryBreakdown_ProviderFilter pins the SQL: a provider appends a
// parameter-bound `AND provider = $3` after the (here unconstrained) account
// clause.
func TestQueryBreakdown_ProviderFilter(t *testing.T) {
	client, pgMock := newMockAnalyticsClient(t)
	rows := pgMock.NewRows([]string{"bucket", "savings", "upfront", "purchases"}).
		AddRow("ec2", 30.0, 10.0, 1)

	pgMock.ExpectQuery(`(?s)SELECT service AS bucket.*AND TRUE AND provider = \$3\s+GROUP BY bucket`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "aws").
		WillReturnRows(rows)

	out, err := client.QueryBreakdown(context.Background(), nil, nil, "aws",
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		"service")
	require.NoError(t, err)
	assert.InDelta(t, 100.0, out["ec2"].Percentage, 1e-9)
	assert.NoError(t, pgMock.ExpectationsWereMet())
}

// TestQueryBreakdown_ProviderFilterWithAccount verifies the provider bind comes
// after the dual-column account binds ($6 here), so account and provider
// filtering compose, as in TestQueryHistory_ProviderFilterWithAccount.
func TestQueryBreakdown_ProviderFilterWithAccount(t *testing.T) {
	client, pgMock := newMockAnalyticsClient(t)
	uuid := "aabbccdd-1234-5678-abcd-aabbccddee00"
	external := "123456789012"
	rows := pgMock.NewRows([]string{"bucket", "savings", "upfront", "purchases"}).
		AddRow("rds", 200.0, 80.0, 3)

	pgMock.ExpectQuery(`(?s)SELECT service AS bucket.*AND \(cloud_account_id = ANY\(\$3\) OR \(provider = \$4 AND account_id = ANY\(\$5\)\)\) AND provider = \$6\s+GROUP BY bucket`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), []string{uuid}, "aws", []string{external}, "aws").
		WillReturnRows(rows)

	_, err := client.QueryBreakdown(context.Background(), []string{uuid}, map[string][]string{"aws": {external}}, "aws",
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		"service")
	require.NoError(t, err)
	assert.NoError(t, pgMock.ExpectationsWereMet())
}

// TestQueryBreakdown_NoProviderNoClause verifies an empty provider adds neither
// a predicate nor a bind, so the all-providers query is unchanged.
func TestQueryBreakdown_NoProviderNoClause(t *testing.T) {
	client, pgMock := newMockAnalyticsClient(t)
	rows := pgMock.NewRows([]string{"bucket", "savings", "upfront", "purchases"})

	pgMock.ExpectQuery(`(?s)SELECT service AS bucket.*AND TRUE\s+GROUP BY bucket`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	_, err := client.QueryBreakdown(context.Background(), nil, nil, "", time.Now().Add(-time.Hour), time.Now(), "service")
	require.NoError(t, err)
	assert.NoError(t, pgMock.ExpectationsWereMet())
}
