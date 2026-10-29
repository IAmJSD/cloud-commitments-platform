package api

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #627: on a non-Lambda runtime, Scheduler.ListRecommendations can kick
// a cold-start collection (taking the global in-flight marker) or a
// stale-cache refresh. Both collect for every registered account, so only a
// reader with unrestricted account scope may let its read kick them. Every
// handler that reads recommendations marks the ctx it passes accordingly.

type recsReadEndpoint struct {
	name string
	call func(h *Handler, ctx context.Context, req *events.LambdaFunctionURLRequest) error
}

var recsReadEndpoints = []recsReadEndpoint{
	{"GET /api/recommendations", func(h *Handler, ctx context.Context, req *events.LambdaFunctionURLRequest) error {
		_, err := h.getRecommendations(ctx, req, map[string]string{})
		return err
	}},
	{"GET /api/dashboard/summary", func(h *Handler, ctx context.Context, req *events.LambdaFunctionURLRequest) error {
		_, err := h.getDashboardSummary(ctx, req, map[string]string{})
		return err
	}},
	{"GET /api/inventory/coverage", func(h *Handler, ctx context.Context, req *events.LambdaFunctionURLRequest) error {
		_, err := h.getCoverageBreakdown(ctx, req, map[string]string{})
		return err
	}},
}

func viewRecommendationsAndPurchases() []auth.Permission {
	return []auth.Permission{
		{Action: auth.ActionView, Resource: auth.ResourceRecommendations},
		{Action: auth.ActionView, Resource: auth.ResourcePurchases},
	}
}

// recsReadStore stubs the store reads the three handlers make besides the
// scheduler call.
func recsReadStore() *MockConfigStore {
	store := new(MockConfigStore)
	store.On("GetGlobalConfig", mock.Anything).
		Return(&config.GlobalConfig{EnabledProviders: []string{"aws"}, DefaultCoverage: 80}, nil).Maybe()
	store.On("GetActivePurchaseHistory", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]config.PurchaseHistoryRecord{}, nil).Maybe()
	store.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return []config.CloudAccount{{ID: scopedInAccount, Name: "In"}}, nil
	}
	return store
}

// capturingScheduler records whether each ListRecommendations ctx was allowed
// to kick a background collection.
func capturingScheduler(t *testing.T, allowed *[]bool) *MockScheduler {
	t.Helper()
	sched := new(MockScheduler)
	t.Cleanup(func() { sched.AssertExpectations(t) })
	sched.On("ListRecommendations", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			*allowed = append(*allowed, scheduler.BackgroundCollectionAllowed(args.Get(0).(context.Context)))
		}).
		Return([]config.RecommendationRecord{}, nil)
	return sched
}

func scopedAuth(accounts []string) *MockAuthService {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).
		Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.grantPermissionsScoped(viewRecommendationsAndPurchases(), accounts)
	return mockAuth
}

func TestRecommendationsRead_BackgroundCollectionOnlyForUnrestrictedReaders(t *testing.T) {
	cases := []struct {
		name    string
		auth    func() *MockAuthService
		apiKey  bool
		allowed bool
	}{
		{name: "scoped user", auth: func() *MockAuthService { return scopedAuth([]string{scopedInAccount}) }, allowed: false},
		{name: "unrestricted user", auth: func() *MockAuthService { return scopedAuth(nil) }, allowed: true},
		{name: "admin API key", auth: func() *MockAuthService { return scopedAuth([]string{scopedInAccount}) }, apiKey: true, allowed: true},
	}
	for _, ep := range recsReadEndpoints {
		for _, tc := range cases {
			t.Run(ep.name+"/"+tc.name, func(t *testing.T) {
				var allowed []bool
				h := &Handler{config: recsReadStore(), scheduler: capturingScheduler(t, &allowed), auth: tc.auth()}
				req := scopedRequest("")
				if tc.apiKey {
					h.apiKey = "test-key"
					req = &events.LambdaFunctionURLRequest{Headers: map[string]string{"X-API-Key": "test-key"}}
				}

				require.NoError(t, ep.call(h, context.Background(), req))
				require.Len(t, allowed, 1, "the handler must read recommendations exactly once")
				assert.Equal(t, tc.allowed, allowed[0])
			})
		}
	}
}

// A scope that cannot be established fails closed: the handler returns the
// lookup error before reading, so nothing can kick a collection.
func TestRecommendationsRead_ScopeLookupErrorNeverAllowsBackgroundCollection(t *testing.T) {
	for _, ep := range recsReadEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			mockAuth := new(MockAuthService)
			t.Cleanup(func() { mockAuth.AssertExpectations(t) })
			mockAuth.On("ValidateSession", mock.Anything, scopedToken).
				Return(&Session{UserID: scopedUserID}, nil).Maybe()
			mockAuth.On("HasPermissionAPI", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Return(true, nil).Maybe()
			mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).
				Return(nil, errors.New("scope lookup failed"))
			// No ListRecommendations expectation: a read without an
			// established scope would hit an unregistered method.
			sched := new(MockScheduler)
			h := &Handler{config: recsReadStore(), scheduler: sched, auth: mockAuth}

			err := ep.call(h, context.Background(), scopedRequest(""))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "scope lookup failed")
			sched.AssertNotCalled(t, "ListRecommendations", mock.Anything, mock.Anything)
		})
	}
}

// End to end through the real scheduler on a non-Lambda runtime with an empty
// cache: a scoped reader's list call must not take the in-flight marker (so it
// cannot start a collection or make other users' refresh return 409), while an
// unrestricted reader's call still attempts it. The marker attempt is made to
// lose, so no collection runs in either case.
func TestGetRecommendations_ColdStartMarkerOnlyForUnrestrictedReaders(t *testing.T) {
	cases := []struct {
		name      string
		accounts  []string
		wantMarks int
	}{
		{name: "scoped reader", accounts: []string{scopedInAccount}, wantMarks: 0},
		{name: "unrestricted reader", accounts: nil, wantMarks: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := recsReadStore()
			t.Cleanup(func() { store.AssertExpectations(t) })
			store.On("GetRecommendationsFreshness", mock.Anything).
				Return(&config.RecommendationsFreshness{LastCollectedAt: nil}, nil)
			store.On("MarkCollectionStarted", mock.Anything).Return("", false, nil).Maybe()
			sched := scheduler.NewScheduler(scheduler.SchedulerConfig{ConfigStore: store, IsLambda: false})
			h := &Handler{config: store, scheduler: sched, auth: scopedAuth(tc.accounts)}

			_, err := h.getRecommendations(context.Background(), scopedRequest(""), map[string]string{})
			require.NoError(t, err)
			store.AssertNumberOfCalls(t, "MarkCollectionStarted", tc.wantMarks)
		})
	}
}

// GET /api/recommendations resolves the account scope once and uses it for both
// the background-collection decision and the post-filter.
func TestGetRecommendations_ResolvesAccountScopeOnce(t *testing.T) {
	for _, accounts := range [][]string{nil, {scopedInAccount}} {
		var allowed []bool
		mockAuth := scopedAuth(accounts)
		h := &Handler{config: recsReadStore(), scheduler: capturingScheduler(t, &allowed), auth: mockAuth}

		_, err := h.getRecommendations(context.Background(), scopedRequest(""), map[string]string{})
		require.NoError(t, err)
		mockAuth.AssertNumberOfCalls(t, "GetAllowedAccountsAPI", 1)
	}
}
