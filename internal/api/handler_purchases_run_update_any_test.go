package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #1129: run (execute now) let an update-any holder run another user's
// scheduled purchase without evaluating update-any's Constraints. The
// execute:purchases grant in these tests is unbounded, so only the update-any
// bound can refuse. The runRouteRec row is azure/compute/westeurope on account
// "acct-1".

func runRouteRec() config.RecommendationRecord {
	acct := "acct-1"
	return config.RecommendationRecord{ID: "r1", Provider: "azure", Service: "compute", Region: "westeurope", CloudAccountID: &acct, Count: 1, UpfrontCost: 100}
}

var unboundedExecute = auth.Permission{Action: auth.ActionExecute, Resource: auth.ResourcePurchases}

// runPlanned runs the route through mp as revokeConstraintsUser against a
// pending row created by creator. mp.ClaimedRow, when set, is the row the
// claim CAS returns.
func runPlanned(t *testing.T, mp *MockPurchaseManager, creator *string, recs []config.RecommendationRecord, perms ...auth.Permission) error {
	t.Helper()
	exec := cancelRouteExec("pending")
	exec.CreatedByUserID = creator
	exec.Recommendations = []config.RecommendationRecord{runRouteRec()}
	if recs != nil {
		exec.Recommendations = recs
	}
	h, _ := cancelRouteHandler(exec, append(perms, unboundedExecute)...)
	mp.On("RunPlannedPurchaseNow", mock.Anything, cancelExecID, mock.Anything, mock.Anything).
		Return(&config.PurchaseExecution{ExecutionID: cancelExecID, Status: "completed"}, "", nil).Maybe()
	h.purchase = mp
	_, err := h.runPlannedPurchase(context.Background(), deletePlannedReq(), cancelExecID)
	return err
}

func TestRunPlanned_UpdateAnyConstraintDimensions(t *testing.T) {
	t.Parallel()
	other := pauseResumeOtherUser
	denying := map[string]*auth.PermissionConstraints{
		"providers":   {Providers: []string{"aws"}},
		"services":    {Services: []string{"rds"}},
		"regions":     {Regions: []string{"eastus"}},
		"account ids": {AccountIDs: []string{"other-acct"}},
	}
	allowing := map[string]*auth.PermissionConstraints{
		"providers":   {Providers: []string{"azure"}},
		"services":    {Services: []string{"compute"}},
		"regions":     {Regions: []string{"westeurope"}},
		"account ids": {AccountIDs: []string{"acct-1"}},
		"none":        nil,
	}
	for dim, c := range denying {
		t.Run("deny/"+dim, func(t *testing.T) {
			t.Parallel()
			mp := &MockPurchaseManager{}
			err := runPlanned(t, mp, &other, nil, revokePerm(auth.ActionUpdateAny, c))
			requireRevokeForbidden(t, err, constraintDenied)
			mp.AssertNotCalled(t, "RunPlannedPurchaseNow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
	for dim, c := range allowing {
		t.Run("allow/"+dim, func(t *testing.T) {
			t.Parallel()
			mp := &MockPurchaseManager{}
			err := runPlanned(t, mp, &other, nil, revokePerm(auth.ActionUpdateAny, c))
			require.NoError(t, err)
			mp.AssertNumberOfCalls(t, "RunPlannedPurchaseNow", 1)
		})
	}
}

func TestRunPlanned_UpdateAnyStrictScope(t *testing.T) {
	t.Parallel()
	other := pauseResumeOtherUser
	azureOnly := revokePerm(auth.ActionUpdateAny, &auth.PermissionConstraints{Providers: []string{"azure"}})
	t.Run("every recommendation must be in scope", func(t *testing.T) {
		t.Parallel()
		aws := recFor("acct-1", "aws", "ec2", "us-east-1")
		aws.UpfrontCost = 100
		mp := &MockPurchaseManager{}
		err := runPlanned(t, mp, &other, []config.RecommendationRecord{runRouteRec(), aws}, azureOnly)
		requireRevokeForbidden(t, err, constraintDenied)
		mp.AssertNotCalled(t, "RunPlannedPurchaseNow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
	t.Run("admin is unaffected", func(t *testing.T) {
		t.Parallel()
		mp := &MockPurchaseManager{}
		err := runPlanned(t, mp, &other, nil, auth.Permission{Action: auth.ActionAdmin, Resource: auth.ResourceAll})
		require.NoError(t, err)
		mp.AssertNumberOfCalls(t, "RunPlannedPurchaseNow", 1)
	})
}

// A bounded update-any denial still lets the row's creator run their own
// scheduled purchase, and keeps the constraint message for anyone else.
func TestRunPlanned_UpdateAnyDenialFallsBackToCreatorOnly(t *testing.T) {
	t.Parallel()
	creator, other := revokeConstraintsUser, pauseResumeOtherUser
	t.Run("creator allowed", func(t *testing.T) {
		t.Parallel()
		mp := &MockPurchaseManager{}
		err := runPlanned(t, mp, &creator, nil, awsOnly(auth.ActionUpdateAny))
		require.NoError(t, err)
		mp.AssertNumberOfCalls(t, "RunPlannedPurchaseNow", 1)
	})
	t.Run("non-creator denied", func(t *testing.T) {
		t.Parallel()
		mp := &MockPurchaseManager{}
		err := runPlanned(t, mp, &other, nil, awsOnly(auth.ActionUpdateAny))
		requireRevokeForbidden(t, err, constraintDenied)
		mp.AssertNotCalled(t, "RunPlannedPurchaseNow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}

// The funnel re-runs the update-any bound on the row its claim returns: a row
// edited out of scope between the pre-claim read and the claim is not bought.
func TestRunPlanned_UpdateAnyBoundRecheckedOnClaimedRow(t *testing.T) {
	t.Parallel()
	other := pauseResumeOtherUser
	azureOnly := revokePerm(auth.ActionUpdateAny, &auth.PermissionConstraints{Providers: []string{"azure"}})
	aws := recFor("acct-1", "aws", "ec2", "us-east-1")
	aws.UpfrontCost = 100
	claimed := cancelRouteExec("running")
	claimed.CreatedByUserID = &other
	claimed.Recommendations = []config.RecommendationRecord{aws}

	mp := &MockPurchaseManager{ClaimedRow: claimed}
	err := runPlanned(t, mp, &other, nil, azureOnly)
	requireClaimedRowRefused(t, mp, err)
	mp.AssertNotCalled(t, "RunPlannedPurchaseNow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// An error evaluating update-any's Constraints is not a denial: it must not
// fall back to the creator branch (the session created the row, so that branch
// would allow) and must not run the purchase.
func TestRunPlanned_UpdateAnyConstraintErrorDoesNotFallBack(t *testing.T) {
	t.Parallel()
	boom := errors.New("constraint store down")
	owner := revokeConstraintsUser
	exec := cancelRouteExec("pending")
	exec.CreatedByUserID = &owner
	exec.Recommendations = []config.RecommendationRecord{runRouteRec()}
	store := new(MockConfigStore)
	store.On("GetExecutionByID", mock.Anything, cancelExecID).Return(exec, nil)
	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, "sess-tok").Return(&Session{UserID: owner}, nil)
	m.On("GetAllowedAccountsAPI", mock.Anything, owner).Return([]string{}, nil)
	m.On("HasPermissionAPI", mock.Anything, owner, mock.Anything, auth.ResourcePurchases).Return(true, nil)
	m.On("HasPermissionForConstraintsAPI", mock.Anything, owner, auth.ActionUpdateAny, auth.ResourcePurchases, mock.Anything).Return(false, boom)
	m.On("HasPermissionForConstraintsAPI", mock.Anything, owner, auth.ActionExecute, auth.ResourcePurchases, mock.Anything).Return(true, nil).Maybe()
	mp := new(MockPurchaseManager)
	mp.On("RunPlannedPurchaseNow", mock.Anything, cancelExecID, mock.Anything, mock.Anything).
		Return(&config.PurchaseExecution{ExecutionID: cancelExecID, Status: "completed"}, "", nil).Maybe()

	h := &Handler{config: store, auth: m, purchase: mp}
	resp, err := h.runPlannedPurchase(context.Background(), deletePlannedReq(), cancelExecID)
	require.ErrorIs(t, err, boom)
	assert.Nil(t, resp)
	mp.AssertNotCalled(t, "RunPlannedPurchaseNow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// update-any Constraints on run follow the API key, not its owner (whose
// group holds an unbounded update-any). The keyScopeExec row is aws/ec2/
// us-east-1, created by another user.
func TestRunPlanned_APIKeyUpdateAnyFollowsKeyConstraints(t *testing.T) {
	var run keyScopeRoute
	for _, rt := range keyScopeRoutes {
		if rt.name == "run" {
			run = rt
		}
	}
	require.Equal(t, "run", run.name)
	keyPerms := func(provider string) []auth.Permission {
		return []auth.Permission{
			unboundedExecute,
			revokePerm(auth.ActionUpdateAny, &auth.PermissionConstraints{Providers: []string{provider}}),
		}
	}
	t.Run("key bounded to another provider is refused", func(t *testing.T) {
		code, reached := serveKeyScope(t, run, newKeyScopeAuth(keyPerms("azure")), keyScopeVictim, keyHeaders())
		assert.Equal(t, http.StatusForbidden, code)
		assert.False(t, reached, "a refused request must not reach the purchase")
	})
	t.Run("key bounded to the row's provider is allowed", func(t *testing.T) {
		_, reached := serveKeyScope(t, run, newKeyScopeAuth(keyPerms("aws")), keyScopeVictim, keyHeaders())
		assert.True(t, reached)
	})
}

// authorizeClaimedRowManagement's branches outside update-any: the admin key
// passes, a session without update-any must be the claimed row's creator, and
// a failed verb probe is returned rather than read as either answer.
func TestAuthorizeClaimedRowManagement_WithoutUpdateAny(t *testing.T) {
	t.Parallel()
	creator, other := revokeConstraintsUser, pauseResumeOtherUser
	row := func(by *string) *config.PurchaseExecution {
		exec := cancelRouteExec("running")
		exec.CreatedByUserID = by
		return exec
	}
	h, _ := cancelRouteHandler(row(&creator), unboundedExecute)
	ctx := context.Background()

	require.NoError(t, h.authorizeClaimedRowManagement(ctx, &Session{UserID: creator}, row(&creator)))
	require.NoError(t, h.authorizeClaimedRowManagement(ctx, &Session{UserID: apiKeyAdminUserID}, row(&other)))
	requireRevokeForbidden(t, h.authorizeClaimedRowManagement(ctx, &Session{UserID: creator}, row(&other)), "another user's scheduled purchase")
	requireRevokeForbidden(t, h.authorizeClaimedRowManagement(ctx, &Session{UserID: creator}, row(nil)), "another user's scheduled purchase")

	boom := errors.New("auth store down")
	m := new(MockAuthService)
	m.On("HasPermissionAPI", mock.Anything, creator, auth.ActionUpdateAny, auth.ResourcePurchases).Return(false, boom)
	err := (&Handler{auth: m}).authorizeClaimedRowManagement(ctx, &Session{UserID: creator}, row(&creator))
	require.ErrorIs(t, err, boom)
}
