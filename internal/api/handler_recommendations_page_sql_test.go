package api

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
)

// TestGetRecommendations_PagedReadUsesSQLPage checks the issue #981 path: a
// paginated request takes its page and summary from PageRecommendations,
// passes the handler-side filters down (enabled providers and the scoped
// reader's exact account set), asks for one row past the page, and never
// scans the cache.
func TestGetRecommendations_PagedReadUsesSQLPage(t *testing.T) {
	h, sched := scanTestHandler(scanFilterRecs(), 4, true)
	in := scopedInAccount
	rows := []config.RecommendationRecord{
		{ID: "a", Provider: "aws", CloudAccountID: &in, Region: "us-east-1"},
		{ID: "b", Provider: "aws", CloudAccountID: &in, Region: "us-east-1"},
		{ID: "c", Provider: "aws", CloudAccountID: &in, Region: "us-east-1"},
	}
	sched.On("PageRecommendations", mock.Anything, mock.Anything, "Z", 3).Return(&config.RecommendationPage{
		TotalCount: 42, TotalSavings: 10, TotalUpfront: 25, Regions: []string{"us-east-1", "eu-west-1"}, Rows: rows,
	}, nil)

	resp, err := h.getRecommendations(context.Background(), scopedRequest(""),
		map[string]string{"limit": "2", "cursor": encodeRecommendationsCursor("Z"), "provider": "aws"})
	require.NoError(t, err)

	assert.Empty(t, sched.ctxs, "the paginated read must not scan")
	assert.Equal(t, RecommendationsSummary{TotalCount: 42, TotalMonthlySavings: 10, TotalUpfrontCost: 25, AvgPaybackMonths: 2.5}, resp.Summary)
	assert.ElementsMatch(t, []string{"us-east-1", "eu-west-1"}, resp.Regions)
	require.Len(t, resp.Recommendations, 2)
	assert.Equal(t, encodeRecommendationsCursor("b"), resp.NextCursor)

	call := sched.Calls[len(sched.Calls)-1]
	assert.False(t, scheduler.BackgroundCollectionAllowed(call.Arguments.Get(0).(context.Context)), "a scoped reader never kicks a collection")
	scope := call.Arguments.Get(1).(config.RecommendationListScope)
	assert.Equal(t, "aws", scope.Filter.Provider)
	assert.Equal(t, []string{"aws"}, scope.EnabledProviders)
	assert.True(t, scope.Restricted)
	assert.Equal(t, []string{scopedInAccount}, scope.AllowedAccountIDs)
}

// TestGetRecommendations_SQLPageFallsBackToScan checks every way the SQL
// page hands the request to the scan: the scheduler says so, a row the
// handler's own filters reject, or rows out of cursor order. The response is
// then exactly the scan's.
func TestGetRecommendations_SQLPageFallsBackToScan(t *testing.T) {
	in, out := scopedInAccount, "22222222-2222-4222-8222-222222222222"
	cases := map[string]struct {
		page *config.RecommendationPage
		err  error
	}{
		"scheduler needs scan": {nil, scheduler.ErrRecommendationsPageNeedsScan},
		"row out of scope": {&config.RecommendationPage{TotalCount: 1, Rows: []config.RecommendationRecord{
			{ID: "a", Provider: "aws", CloudAccountID: &out}}}, nil},
		"row of a disabled provider": {&config.RecommendationPage{TotalCount: 1, Rows: []config.RecommendationRecord{
			{ID: "a", Provider: "azure", CloudAccountID: &in}}}, nil},
		"rows out of order": {&config.RecommendationPage{TotalCount: 2, Rows: []config.RecommendationRecord{
			{ID: "b", Provider: "aws", CloudAccountID: &in}, {ID: "a", Provider: "aws", CloudAccountID: &in}}}, nil},
		"rows not after the cursor": {&config.RecommendationPage{TotalCount: 1, Rows: []config.RecommendationRecord{
			{ID: "", Provider: "aws", CloudAccountID: &in}}}, nil},
	}
	params := map[string]string{"limit": "3"}
	scanOnly, _ := scanTestHandler(scanFilterRecs(), 4, true)
	want, err := scanOnly.getRecommendations(context.Background(), scopedRequest(""), params)
	require.NoError(t, err)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, sched := scanTestHandler(scanFilterRecs(), 4, true)
			sched.On("PageRecommendations", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(tc.page, tc.err)
			if name == "rows not after the cursor" {
				params = map[string]string{"limit": "3", "cursor": encodeRecommendationsCursor("A")}
				var err error
				want, err = scanOnly.getRecommendations(context.Background(), scopedRequest(""), params)
				require.NoError(t, err)
			}
			got, err := h.getRecommendations(context.Background(), scopedRequest(""), params)
			require.NoError(t, err)
			assert.Len(t, sched.ctxs, 1, "the request is served by the scan")
			sort.Strings(got.Regions)
			sort.Strings(want.Regions)
			assert.Equal(t, want, got)
		})
	}
}

// TestGetRecommendations_SQLPageErrorFails keeps a real read failure a
// failure: only ErrRecommendationsPageNeedsScan falls back.
func TestGetRecommendations_SQLPageErrorFails(t *testing.T) {
	h, sched := scanTestHandler(scanFilterRecs(), 4, false)
	sched.On("PageRecommendations", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil, errors.New("apply suppressions: db down"))
	_, err := h.getRecommendations(context.Background(), scopedRequest(""), map[string]string{"limit": "3"})
	require.ErrorContains(t, err, "db down")
	assert.Empty(t, sched.ctxs)
}

// TestScopeAllowedAccountIDs checks that the SQL account set is exactly what
// AccountScope.Allows grants a rec's account ID: every non-empty allow-list
// entry taken literally (registered or not), and no registered account the
// list only names by external ID or display name.
func TestScopeAllowedAccountIDs(t *testing.T) {
	registered := []auth.RegisteredAccount{
		{ID: "id-a", Name: "alpha", ExternalID: "ext-a"},
		{ID: "id-b", Name: "beta", ExternalID: "ext-b"},
	}
	nameByID := map[string]string{"id-a": "alpha", "id-b": "beta"}
	scope := auth.ScopeForAccounts([]string{"id-a", "unregistered", "ext-b", "beta", ""}).WithRegisteredAccounts(registered)
	got := scopeAllowedAccountIDs(scope, nameByID)
	assert.Equal(t, []string{"beta", "ext-b", "id-a", "unregistered"}, got)
	for _, id := range []string{"id-a", "id-b", "unregistered", "ext-b", "beta", ""} {
		assert.Equal(t, scope.Allows(id, nameByID[id]), contains(got, id), id)
	}
	assert.Empty(t, scopeAllowedAccountIDs(auth.ScopeForAccounts(nil), nameByID))
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
