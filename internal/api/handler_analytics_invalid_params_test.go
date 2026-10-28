package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Regression tests for issue #1102 item 1: an unsupported analytics interval
// or dimension used to reach the Postgres client, fail its allowlist and come
// back as a 500. It is a bad request, so the handler now answers 400 before
// any query runs.

func TestHandler_getHistoryAnalytics_UnsupportedIntervalIs400(t *testing.T) {
	for _, interval := range []string{"yearly", "minute", "HOURLY", "hour"} {
		t.Run(interval, func(t *testing.T) {
			ctx := context.Background()
			mockClient := new(MockAnalyticsClient)
			// What the Postgres client answers for an interval outside its
			// allowlist, so the pre-fix handler fails this test with a 500
			// instead of panicking on an unexpected call.
			mockClient.On("QueryHistory", ctx, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, interval).
				Return(nil, nil, fmt.Errorf("unsupported interval %q", interval)).Maybe()
			mockAuth, req := adminAnalyticsReq(ctx)
			handler := &Handler{auth: mockAuth, analyticsClient: mockClient, config: new(MockConfigStore)}

			_, err := handler.getHistoryAnalytics(ctx, req, map[string]string{"interval": interval})
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "want a client error, got %v", err)
			assert.Equal(t, 400, ce.code)
			assert.Contains(t, ce.message, "invalid interval")
			mockClient.AssertNotCalled(t, "QueryHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestHandler_getHistoryAnalytics_SupportedIntervalsReachStore(t *testing.T) {
	for _, interval := range []string{"hourly", "daily", "weekly", "monthly"} {
		t.Run(interval, func(t *testing.T) {
			ctx := context.Background()
			mockClient := new(MockAnalyticsClient)
			mockClient.On("QueryHistory", ctx, []string(nil), map[string][]string(nil), "", mock.Anything, mock.Anything, interval).
				Return([]HistoryDataPoint{}, (*HistorySummary)(nil), nil)
			mockAuth, req := adminAnalyticsReq(ctx)
			handler := &Handler{auth: mockAuth, analyticsClient: mockClient, config: new(MockConfigStore)}

			result, err := handler.getHistoryAnalytics(ctx, req, map[string]string{"interval": interval})
			require.NoError(t, err)
			resp, ok := result.(*AnalyticsResponse)
			require.True(t, ok)
			assert.Equal(t, interval, resp.Interval)
			mockClient.AssertExpectations(t)
		})
	}
}

func TestHandler_getHistoryBreakdown_UnsupportedDimensionIs400(t *testing.T) {
	for _, dimension := range []string{"tenant", "account_id", "SERVICE", "service_type"} {
		t.Run(dimension, func(t *testing.T) {
			ctx := context.Background()
			mockClient := new(MockAnalyticsClient)
			// The Postgres client's answer for an unknown dimension; see above.
			mockClient.On("QueryBreakdown", ctx, mock.Anything, mock.Anything, mock.Anything, mock.Anything, dimension).
				Return(nil, fmt.Errorf("unsupported dimension %q", dimension)).Maybe()
			mockAuth, req := adminAnalyticsReq(ctx)
			handler := &Handler{auth: mockAuth, analyticsClient: mockClient, config: new(MockConfigStore)}

			_, err := handler.getHistoryBreakdown(ctx, req, map[string]string{"dimension": dimension})
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "want a client error, got %v", err)
			assert.Equal(t, 400, ce.code)
			assert.Contains(t, ce.message, "invalid dimension")
			mockClient.AssertNotCalled(t, "QueryBreakdown", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestHandler_getHistoryBreakdown_SupportedDimensionsReachStore(t *testing.T) {
	for _, dimension := range []string{"service", "provider", "region", "account"} {
		t.Run(dimension, func(t *testing.T) {
			ctx := context.Background()
			mockClient := new(MockAnalyticsClient)
			mockClient.On("QueryBreakdown", ctx, []string(nil), map[string][]string(nil), mock.Anything, mock.Anything, dimension).
				Return(map[string]BreakdownValue{}, nil)
			mockAuth, req := adminAnalyticsReq(ctx)
			handler := &Handler{auth: mockAuth, analyticsClient: mockClient, config: new(MockConfigStore)}

			result, err := handler.getHistoryBreakdown(ctx, req, map[string]string{"dimension": dimension})
			require.NoError(t, err)
			resp, ok := result.(*BreakdownResponse)
			require.True(t, ok)
			assert.Equal(t, dimension, resp.Dimension)
			mockClient.AssertExpectations(t)
		})
	}
}
