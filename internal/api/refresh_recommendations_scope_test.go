package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// POST /api/recommendations/refresh takes no account parameter: it starts a
// collection for every enabled provider and every registered account, using
// every account's credentials, and rewrites the shared recommendation cache
// (issue #520). A caller whose allowed_accounts is a subset must therefore be
// refused before the in-flight marker is taken, a collection runs or the
// scheduler Lambda is invoked.

const schedulerARNForRefreshTests = "arn:aws:lambda:us-east-1:123456789012:function:cudly"

type refreshFixture struct {
	handler    *Handler
	store      *MockConfigStore
	scheduler  *MockScheduler
	lambdaCall *atomic.Int32
}

// newRefreshFixture builds a handler whose principal holds perms and is
// restricted to accounts (nil means unrestricted).
func newRefreshFixture(t *testing.T, perms []auth.Permission, accounts []string) *refreshFixture {
	t.Helper()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).
		Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.grantPermissionsScoped(perms, accounts)
	return newRefreshFixtureWithAuth(t, mockAuth)
}

func newRefreshFixtureWithAuth(t *testing.T, mockAuth *MockAuthService) *refreshFixture {
	t.Helper()
	store := new(MockConfigStore)
	store.On("GetRecommendationsFreshness", mock.Anything).
		Return(&config.RecommendationsFreshness{}, nil).Maybe()
	store.On("MarkCollectionStarted", mock.Anything).Return("tok-1", true, nil).Maybe()
	store.On("ClearCollectionStarted", mock.Anything, mock.Anything).Return(nil).Maybe()

	sched := new(MockScheduler)
	sched.On("CollectRecommendations", mock.Anything, mock.Anything).
		Return(&scheduler.CollectResult{}, nil).Maybe()

	var invokes atomic.Int32
	invoker := &stubLambdaInvoker{
		invokeFn: func(_ context.Context, _ *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
			invokes.Add(1)
			return &lambda.InvokeOutput{}, nil
		},
	}
	return &refreshFixture{
		handler:    &Handler{config: store, scheduler: sched, auth: mockAuth, lambdaInvoker: invoker},
		store:      store,
		scheduler:  sched,
		lambdaCall: &invokes,
	}
}

func (f *refreshFixture) refresh(req *events.LambdaFunctionURLRequest) (any, error) {
	return NewRouter(f.handler).Route(context.Background(), "POST", "/api/recommendations/refresh", req)
}

func (f *refreshFixture) assertNothingStarted(t *testing.T) {
	t.Helper()
	f.store.AssertNumberOfCalls(t, "MarkCollectionStarted", 0)
	f.store.AssertNumberOfCalls(t, "GetRecommendationsFreshness", 0)
	f.scheduler.AssertNumberOfCalls(t, "CollectRecommendations", 0)
	assert.Zero(t, f.lambdaCall.Load(), "scheduler Lambda must not be invoked")
}

func viewRecommendationsOnly() []auth.Permission {
	return []auth.Permission{{Action: auth.ActionView, Resource: auth.ResourceRecommendations}}
}

func requireForbiddenUnrestricted(t *testing.T, res any, err error) {
	t.Helper()
	require.Error(t, err, "got response %v", res)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.message, "unrestricted account access")
}

func TestRouterDispatch_RefreshRecommendations_ScopedUserSync(t *testing.T) {
	t.Setenv("SCHEDULER_LAMBDA_ARN", "")
	f := newRefreshFixture(t, viewRecommendationsOnly(), []string{scopedInAccount})

	res, err := f.refresh(scopedRequest(""))

	requireForbiddenUnrestricted(t, res, err)
	f.assertNothingStarted(t)
}

func TestRouterDispatch_RefreshRecommendations_ScopedUserAsync(t *testing.T) {
	t.Setenv("SCHEDULER_LAMBDA_ARN", schedulerARNForRefreshTests)
	f := newRefreshFixture(t, viewRecommendationsOnly(), []string{scopedInAccount})

	res, err := f.refresh(scopedRequest(""))

	requireForbiddenUnrestricted(t, res, err)
	f.assertNothingStarted(t)
}

func TestRouterDispatch_RefreshRecommendations_UnrestrictedUserStillRefreshesSync(t *testing.T) {
	t.Setenv("SCHEDULER_LAMBDA_ARN", "")
	f := newRefreshFixture(t, viewRecommendationsOnly(), nil)

	res, err := f.refresh(scopedRequest(""))

	require.NoError(t, err)
	assert.IsType(t, &RefreshResponse{}, res)
	f.store.AssertNumberOfCalls(t, "MarkCollectionStarted", 1)
	f.scheduler.AssertNumberOfCalls(t, "CollectRecommendations", 1)
	assert.Zero(t, f.lambdaCall.Load())
}

func TestRouterDispatch_RefreshRecommendations_UnrestrictedUserStillRefreshesAsync(t *testing.T) {
	t.Setenv("SCHEDULER_LAMBDA_ARN", schedulerARNForRefreshTests)
	f := newRefreshFixture(t, viewRecommendationsOnly(), nil)

	res, err := f.refresh(scopedRequest(""))

	require.NoError(t, err)
	assert.IsType(t, &RefreshResponse{}, res)
	f.store.AssertNumberOfCalls(t, "MarkCollectionStarted", 1)
	f.scheduler.AssertNumberOfCalls(t, "CollectRecommendations", 0)
	assert.EqualValues(t, 1, f.lambdaCall.Load())
}

// The permission verb is checked before the scope: a scoped caller without
// view:recommendations gets the plain permission refusal, not the scope one.
func TestRouterDispatch_RefreshRecommendations_PermissionPrecedesScope(t *testing.T) {
	t.Setenv("SCHEDULER_LAMBDA_ARN", "")
	f := newRefreshFixture(t,
		[]auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePlans}},
		[]string{scopedInAccount})

	res, err := f.refresh(scopedRequest(""))

	require.Error(t, err, "got response %v", res)
	assert.Contains(t, err.Error(), "permission denied")
	assert.NotContains(t, err.Error(), "unrestricted account access")
	f.assertNothingStarted(t)
}

// A scope that cannot be established fails closed with the underlying error
// rather than falling through to a collection.
func TestRouterDispatch_RefreshRecommendations_ScopeLookupErrorFailsClosed(t *testing.T) {
	t.Setenv("SCHEDULER_LAMBDA_ARN", "")
	lookupErr := errors.New("scope lookup failed")
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).
		Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.On("HasPermissionAPI", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(true, nil).Maybe()
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return(nil, lookupErr)
	f := newRefreshFixtureWithAuth(t, mockAuth)

	res, err := f.refresh(scopedRequest(""))

	require.Error(t, err, "got response %v", res)
	assert.ErrorIs(t, err, lookupErr)
	f.assertNothingStarted(t)
}

// The stateless admin API key carries no user row and is unrestricted.
func TestRouterDispatch_RefreshRecommendations_AdminAPIKeyRefreshes(t *testing.T) {
	t.Setenv("SCHEDULER_LAMBDA_ARN", "")
	f := newRefreshFixture(t, viewRecommendationsOnly(), []string{scopedInAccount})
	f.handler.apiKey = "test-key"
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"X-API-Key": "test-key"}}

	res, err := f.refresh(req)

	require.NoError(t, err)
	assert.IsType(t, &RefreshResponse{}, res)
	f.store.AssertNumberOfCalls(t, "MarkCollectionStarted", 1)
	f.scheduler.AssertNumberOfCalls(t, "CollectRecommendations", 1)
}
