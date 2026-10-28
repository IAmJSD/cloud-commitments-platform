package api

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Cost Explorer GetReservationUtilization called from a payer (management)
// account returns the RIs of every linked member account, keyed only by RI
// id. EC2 DescribeReservedInstances returns only the calling account's RIs.
const (
	utilOwnRI    = "ri-payer-own"
	utilMemberB  = "ri-member-b-registered"
	utilMemberC  = "ri-member-c-unregistered"
	utilStaleOwn = "ri-payer-own-2"
)

type countingUtilEC2 struct {
	instances []ec2svc.ConvertibleRI
	err       error
	calls     int
}

func (f *countingUtilEC2) ListConvertibleReservedInstances(_ context.Context) ([]ec2svc.ConvertibleRI, error) {
	f.calls++
	return f.instances, f.err
}

func (f *countingUtilEC2) GetValidResourceTypes(_ context.Context) ([]string, error) {
	return nil, nil
}

type countingUtilRecs struct {
	rows  []recommendations.RIUtilization
	calls int
}

func (f *countingUtilRecs) GetRIUtilization(_ context.Context, _ int, _ string) ([]recommendations.RIUtilization, error) {
	f.calls++
	return f.rows, nil
}

func payerUtilizationRows() []recommendations.RIUtilization {
	return []recommendations.RIUtilization{
		{ReservedInstanceID: utilOwnRI, UtilizationPercent: 80},
		{ReservedInstanceID: utilMemberB, UtilizationPercent: 10},
		{ReservedInstanceID: utilMemberC, UtilizationPercent: 20},
		{ReservedInstanceID: utilStaleOwn, UtilizationPercent: 30},
	}
}

func newUtilizationScopeStore() *MockConfigStore {
	store := new(MockConfigStore)
	store.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return permsAccountList(), nil
	}
	store.On("GetRIUtilizationCache", mock.Anything, mock.Anything).
		Return((*config.RIUtilizationCacheEntry)(nil), nil).Maybe()
	store.On("UpsertRIUtilizationCache", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()
	return store
}

func newUtilizationScopeHandler(auth *MockAuthService, store *MockConfigStore, ec2 *countingUtilEC2, recs *countingUtilRecs, deploymentAccount string) *Handler {
	h := &Handler{
		auth:   auth,
		config: store,
		reshapeEC2Factory: func(_ aws.Config) reshapeEC2Client {
			return ec2
		},
		reshapeRecsFactory: func(_ aws.Config) reshapeRecsClient {
			return recs
		},
		reshapeAccountResolver: func(_ context.Context) (string, error) {
			return deploymentAccount, nil
		},
	}
	h.awsCfgOnce.Do(func() {
		h.awsCfg = aws.Config{Region: "us-east-1"}
	})
	return h
}

func utilIDs(t *testing.T, resp any) []string {
	t.Helper()
	typed, ok := resp.(*RIUtilizationResponse)
	require.True(t, ok, "expected *RIUtilizationResponse, got %T", resp)
	ids := make([]string, 0, len(typed.Utilization))
	for _, u := range typed.Utilization {
		ids = append(ids, u.ReservedInstanceID)
	}
	return ids
}

func ownUtilEC2() *countingUtilEC2 {
	return &countingUtilEC2{instances: []ec2svc.ConvertibleRI{
		{ReservedInstanceID: utilOwnRI},
		{ReservedInstanceID: utilStaleOwn},
	}}
}

// A scoped user whose scope covers the payer account must not receive the
// utilization of linked members' RIs, registered (B) or not (C).
func TestGetRIUtilization_ScopedUserDoesNotSeeLinkedMemberRIs(t *testing.T) {
	ctx := context.Background()
	ec2 := ownUtilEC2()
	recs := &countingUtilRecs{rows: payerUtilizationRows()}
	h := newUtilizationScopeHandler(scopedAuthMock(ctx), newUtilizationScopeStore(), ec2, recs, permsAccA)

	resp, err := h.getRIUtilization(ctx, scopedReq())
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{utilOwnRI, utilStaleOwn}, utilIDs(t, resp))
	assert.Equal(t, 1, ec2.calls, "the deployment account's own RIs are listed exactly once")
}

// A scope entry that names the account instead of listing its UUID is not
// bound to that account, so it reaches none of its rows (#1034).
func TestGetRIUtilization_ScopeByAccountNameReachesNothing(t *testing.T) {
	ctx := context.Background()
	auth := new(MockAuthService)
	auth.On("ValidateSession", ctx, permsScopedToken).Return(&Session{UserID: permsScopedUserID}, nil)
	auth.On("HasPermissionAPI", ctx, permsScopedUserID, mock.Anything, mock.Anything).Return(true, nil)
	auth.On("GetAllowedAccountsAPI", ctx, permsScopedUserID).Return([]string{permsAccAName}, nil)
	ec2 := ownUtilEC2()
	recs := &countingUtilRecs{rows: payerUtilizationRows()}
	h := newUtilizationScopeHandler(auth, newUtilizationScopeStore(), ec2, recs, permsAccA)

	resp, err := h.getRIUtilization(ctx, scopedReq())
	require.NoError(t, err)

	assert.Empty(t, utilIDs(t, resp))
}

// Unrestricted users keep every row and trigger no EC2 listing.
func TestGetRIUtilization_UnrestrictedSeesAllRowsWithoutEC2Call(t *testing.T) {
	ctx := context.Background()
	auth, req := adminDashboardReq(ctx)
	ec2 := ownUtilEC2()
	recs := &countingUtilRecs{rows: payerUtilizationRows()}
	h := newUtilizationScopeHandler(auth, newUtilizationScopeStore(), ec2, recs, permsAccA)

	resp, err := h.getRIUtilization(ctx, req)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{utilOwnRI, utilMemberB, utilMemberC, utilStaleOwn}, utilIDs(t, resp))
	assert.Zero(t, ec2.calls)
}

// A listing failure fails closed: the error is returned, no rows.
func TestGetRIUtilization_ScopedEC2ListFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	ec2 := &countingUtilEC2{err: errors.New("describe failed")}
	recs := &countingUtilRecs{rows: payerUtilizationRows()}
	h := newUtilizationScopeHandler(scopedAuthMock(ctx), newUtilizationScopeStore(), ec2, recs, permsAccA)

	resp, err := h.getRIUtilization(ctx, scopedReq())

	require.Error(t, err)
	assert.ErrorContains(t, err, "describe failed")
	assert.Nil(t, resp)
}

// No own RIs means no visible rows, never "everything".
func TestGetRIUtilization_ScopedWithNoOwnRIsSeesNothing(t *testing.T) {
	ctx := context.Background()
	ec2 := &countingUtilEC2{}
	recs := &countingUtilRecs{rows: payerUtilizationRows()}
	h := newUtilizationScopeHandler(scopedAuthMock(ctx), newUtilizationScopeStore(), ec2, recs, permsAccA)

	resp, err := h.getRIUtilization(ctx, scopedReq())
	require.NoError(t, err)

	assert.Empty(t, utilIDs(t, resp))
}

// Out-of-scope deployment account: empty list, and neither Cost Explorer nor
// EC2 is called.
func TestGetRIUtilization_OutOfScopeDeploymentMakesNoAWSCalls(t *testing.T) {
	ctx := context.Background()
	ec2 := ownUtilEC2()
	recs := &countingUtilRecs{rows: payerUtilizationRows()}
	h := newUtilizationScopeHandler(scopedAuthMock(ctx), newUtilizationScopeStore(), ec2, recs, permsAccB)

	resp, err := h.getRIUtilization(ctx, scopedReq())
	require.NoError(t, err)

	assert.Empty(t, utilIDs(t, resp))
	assert.Zero(t, ec2.calls)
	assert.Zero(t, recs.calls)
}

// Permission is checked before scope: a caller without view:purchases gets
// the permission error and no AWS call is made.
func TestGetRIUtilization_PermissionCheckedBeforeScope(t *testing.T) {
	ctx := context.Background()
	auth := new(MockAuthService)
	auth.On("ValidateSession", ctx, permsScopedToken).Return(&Session{UserID: permsScopedUserID}, nil)
	auth.On("HasPermissionAPI", ctx, permsScopedUserID, mock.Anything, mock.Anything).Return(false, nil)
	ec2 := ownUtilEC2()
	recs := &countingUtilRecs{rows: payerUtilizationRows()}
	h := newUtilizationScopeHandler(auth, newUtilizationScopeStore(), ec2, recs, permsAccA)

	resp, err := h.getRIUtilization(ctx, scopedReq())

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Zero(t, ec2.calls)
	assert.Zero(t, recs.calls)
	auth.AssertNotCalled(t, "GetAllowedAccountsAPI", mock.Anything, mock.Anything)
}

// Two deployment accounts reading the same region and lookback must each get
// their own Cost Explorer result: the resolved cloud account is part of the
// cache key (issue #120). With the old (region, lookback_days) key the second
// account's request was served the first account's cached rows.
func TestGetRIUtilization_AccountsInSameRegionDoNotShareCache(t *testing.T) {
	ctx := context.Background()
	auth, req := adminDashboardReq(ctx)
	req.QueryStringParameters = map[string]string{"region": "us-east-1", "lookback_days": "30"}
	recs := &countingUtilRecs{}
	h := newUtilizationScopeHandler(auth, newUtilizationScopeStore(), ownUtilEC2(), recs, permsAccA)
	cacheStore := newFakeRIUtilCacheStore()
	h.riUtilizationCacheOnce.Do(func() { h.riUtilizationCache = newRIUtilizationCache(cacheStore, false) })
	account := permsAccA
	h.reshapeAccountResolver = func(_ context.Context) (string, error) { return account, nil }

	recs.rows = []recommendations.RIUtilization{{ReservedInstanceID: "ri-account-a"}}
	resp, err := h.getRIUtilization(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, []string{"ri-account-a"}, utilIDs(t, resp))

	account = permsAccB
	recs.rows = []recommendations.RIUtilization{{ReservedInstanceID: "ri-account-b"}}
	resp, err = h.getRIUtilization(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, []string{"ri-account-b"}, utilIDs(t, resp), "account B must not be served account A's cached utilization")
	assert.Equal(t, 2, recs.calls, "account B's read is a cache miss and calls Cost Explorer")

	account = permsAccA
	resp, err = h.getRIUtilization(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, []string{"ri-account-a"}, utilIDs(t, resp), "account A still reads its own cached row")
	assert.Equal(t, 2, recs.calls, "account A's second read is a cache hit")

	_, ok := cacheStore.latest(config.RIUtilizationCacheKey{CloudAccountID: permsAccA, Region: "us-east-1", LookbackDays: 30})
	assert.True(t, ok, "account A's row is stored under its own key")
}

// A failure to resolve the deployment account fails the utilization read
// closed for every session, with no Cost Explorer call: without the account
// the cache row cannot be keyed (issue #120).
func TestGetRIUtilization_AccountResolutionErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	auth, req := adminDashboardReq(ctx)
	recs := &countingUtilRecs{rows: payerUtilizationRows()}
	h := newUtilizationScopeHandler(auth, newUtilizationScopeStore(), ownUtilEC2(), recs, permsAccA)
	h.reshapeAccountResolver = func(_ context.Context) (string, error) {
		return "", errors.New("sts get-caller-identity denied")
	}

	resp, err := h.getRIUtilization(ctx, req)

	require.ErrorContains(t, err, "resolve cloud account scope")
	assert.Nil(t, resp)
	assert.Zero(t, recs.calls)
}
