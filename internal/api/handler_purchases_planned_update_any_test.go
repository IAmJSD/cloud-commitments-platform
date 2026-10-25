package api

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #961: on DELETE /api/purchases/planned/{id}, update-any:purchases
// granted full scope on the verb alone and never evaluated the granting
// permission's Constraints, so a session whose update-any is bounded to one
// provider could cancel any provider's scheduled purchase. Answers come from
// the real matcher via grantPermissionsScoped (bearer sessions) or a real
// auth.Service (API keys); the cancelRouteExec row is azure/compute/
// westeurope on account "acct-1".

const plannedOtherUser = "someone-else"

// runUpdateAnyDelete drives deletePlannedPurchase for a row created by creator
// (nil = legacy NULL creator), optionally replacing its recommendations.
func runUpdateAnyDelete(creator *string, recs []config.RecommendationRecord, perms ...auth.Permission) (*DeletePlannedPurchaseResponse, *MockConfigStore, error) {
	exec := cancelRouteExec("pending")
	exec.CreatedByUserID = creator
	if recs != nil {
		exec.Recommendations = recs
	}
	h, store := cancelRouteHandler(exec, perms...)
	resp, err := h.deletePlannedPurchase(context.Background(), deletePlannedReq(), cancelExecID)
	return resp, store, err
}

func requireNotCanceled(t *testing.T, store *MockConfigStore) {
	t.Helper()
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func requireCanceled(t *testing.T, resp *DeletePlannedPurchaseResponse, err error) {
	t.Helper()
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "canceled", resp.Status)
}

func TestDeletePlannedPurchase_UpdateAnyConstraintDimensions(t *testing.T) {
	t.Parallel()
	other := plannedOtherUser
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
			_, store, err := runUpdateAnyDelete(&other, nil, revokePerm(auth.ActionUpdateAny, c))
			requireRevokeForbidden(t, err, constraintDenied)
			requireNotCanceled(t, store)
		})
	}
	for dim, c := range allowing {
		t.Run("allow/"+dim, func(t *testing.T) {
			t.Parallel()
			resp, _, err := runUpdateAnyDelete(&other, nil, revokePerm(auth.ActionUpdateAny, c))
			requireCanceled(t, resp, err)
		})
	}
}

func TestDeletePlannedPurchase_UpdateAnyStrictScopeAndRecommendations(t *testing.T) {
	t.Parallel()
	other := plannedOtherUser
	azureOnly := revokePerm(auth.ActionUpdateAny, &auth.PermissionConstraints{Providers: []string{"azure"}})
	t.Run("unknown region is refused by a region-bounded grant", func(t *testing.T) {
		t.Parallel()
		recs := []config.RecommendationRecord{recFor("acct-1", "azure", "compute", "")}
		_, store, err := runUpdateAnyDelete(&other, recs, revokePerm(auth.ActionUpdateAny, &auth.PermissionConstraints{Regions: []string{"westeurope"}}))
		requireRevokeForbidden(t, err, constraintDenied)
		requireNotCanceled(t, store)
	})
	t.Run("every recommendation must be in scope", func(t *testing.T) {
		t.Parallel()
		recs := []config.RecommendationRecord{azureRec(), recFor("acct-uuid", "aws", "ec2", "us-east-1")}
		_, store, err := runUpdateAnyDelete(&other, recs, azureOnly)
		requireRevokeForbidden(t, err, constraintDenied)
		requireNotCanceled(t, store)
	})
	t.Run("a row with no recommendations is refused by a bounded grant", func(t *testing.T) {
		t.Parallel()
		_, store, err := runUpdateAnyDelete(&other, []config.RecommendationRecord{}, azureOnly)
		requireRevokeForbidden(t, err, constraintDenied)
		requireNotCanceled(t, store)
	})
	t.Run("a row with no recommendations is allowed by an unbounded grant", func(t *testing.T) {
		t.Parallel()
		resp, _, err := runUpdateAnyDelete(&other, []config.RecommendationRecord{}, revokePerm(auth.ActionUpdateAny, nil))
		requireCanceled(t, resp, err)
	})
}

// A bounded update-any denial falls back to cancel-any and then to the
// creator branch, as #678 does for cancel-any; a non-creator keeps the
// constraint message.
func TestDeletePlannedPurchase_UpdateAnyFallback(t *testing.T) {
	t.Parallel()
	creator, other := revokeConstraintsUser, plannedOtherUser
	del := auth.Permission{Action: auth.ActionDelete, Resource: auth.ResourcePurchases}
	updateOut := awsOnly(auth.ActionUpdateAny)
	allowed := map[string]struct {
		creator *string
		perms   []auth.Permission
	}{
		"to unbounded cancel-any":          {&other, []auth.Permission{updateOut, revokePerm(auth.ActionCancelAny, nil)}},
		"to in-scope cancel-any":           {&other, []auth.Permission{updateOut, revokePerm(auth.ActionCancelAny, &auth.PermissionConstraints{Providers: []string{"azure"}})}},
		"to the creator with delete":       {&creator, []auth.Permission{updateOut, del}},
		"to the creator with in-scope own": {&creator, []auth.Permission{updateOut, revokePerm(auth.ActionCancelOwn, nil)}},
	}
	for name, tc := range allowed {
		t.Run("falls back "+name, func(t *testing.T) {
			t.Parallel()
			resp, _, err := runUpdateAnyDelete(tc.creator, nil, tc.perms...)
			requireCanceled(t, resp, err)
		})
	}
	denied := map[string]struct {
		creator *string
		perms   []auth.Permission
	}{
		"non-creator with delete":              {&other, []auth.Permission{updateOut, del}},
		"non-creator with bounded cancel-any":  {&other, []auth.Permission{updateOut, awsOnly(auth.ActionCancelAny), del}},
		"legacy row with no creator":           {nil, []auth.Permission{updateOut, del}},
		"creator with out-of-scope cancel-own": {&creator, []auth.Permission{updateOut, awsOnly(auth.ActionCancelOwn)}},
	}
	for name, tc := range denied {
		t.Run("denies "+name, func(t *testing.T) {
			t.Parallel()
			_, store, err := runUpdateAnyDelete(tc.creator, nil, tc.perms...)
			requireRevokeForbidden(t, err, constraintDenied)
			requireNotCanceled(t, store)
		})
	}
}

func TestDeletePlannedPurchase_UpdateAnyAdministratorsGroup(t *testing.T) {
	t.Parallel()
	other := plannedOtherUser
	t.Run("unbounded admin still cancels", func(t *testing.T) {
		t.Parallel()
		resp, _, err := runUpdateAnyDelete(&other, nil, auth.Permission{Action: auth.ActionAdmin, Resource: auth.ResourceAll})
		requireCanceled(t, resp, err)
	})
	t.Run("admin bounded to another provider is refused", func(t *testing.T) {
		t.Parallel()
		admin := auth.Permission{Action: auth.ActionAdmin, Resource: auth.ResourceAll, Constraints: &auth.PermissionConstraints{Providers: []string{"aws"}}}
		_, store, err := runUpdateAnyDelete(&other, nil, admin)
		requireRevokeForbidden(t, err, constraintDenied)
		requireNotCanceled(t, store)
	})
}

// An error evaluating update-any's Constraints is not a denial: it must not
// fall back to cancel-any or to the creator branch, both of which would allow.
func TestDeletePlannedPurchase_UpdateAnyConstraintErrorDoesNotFallBack(t *testing.T) {
	t.Parallel()
	boom := errors.New("constraint store down")
	owner := revokeConstraintsUser
	exec := cancelRouteExec("pending")
	exec.CreatedByUserID = &owner
	store := new(MockConfigStore)
	store.On("GetExecutionByID", mock.Anything, cancelExecID).Return(exec, nil)
	// Reached only if the error were swallowed; requireNotCanceled reports it.
	store.On("TransitionExecutionStatus", mock.Anything, cancelExecID, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errKeyScopeReached).Maybe()
	m := new(MockAuthService)
	m.On("ValidateSession", mock.Anything, "sess-tok").Return(&Session{UserID: owner}, nil)
	m.On("GetAllowedAccountsAPI", mock.Anything, owner).Return([]string{}, nil)
	m.On("HasPermissionAPI", mock.Anything, owner, mock.Anything, auth.ResourcePurchases).Return(true, nil)
	m.On("HasPermissionForConstraintsAPI", mock.Anything, owner, auth.ActionUpdateAny, auth.ResourcePurchases, mock.Anything).Return(false, boom)
	m.On("HasPermissionForConstraintsAPI", mock.Anything, owner, mock.Anything, auth.ResourcePurchases, mock.Anything).Return(true, nil).Maybe()

	h := &Handler{config: store, auth: m}
	_, err := h.deletePlannedPurchase(context.Background(), deletePlannedReq(), cancelExecID)
	require.ErrorIs(t, err, boom)
	requireNotCanceled(t, store)
}

// A user API key is bounded by the KEY's update-any Constraints (key
// intersected with its owner), evaluated by the real auth.Service. The
// keyScopeExec row is aws/ec2/us-east-1, created by another user.
func TestDeletePlannedPurchase_APIKeyUpdateAnyFollowsKeyConstraints(t *testing.T) {
	rt := keyScopeRoutes[0]
	keyPerms := func(provider string) []auth.Permission {
		return []auth.Permission{
			{Action: auth.ActionDelete, Resource: auth.ResourcePurchases},
			revokePerm(auth.ActionUpdateAny, &auth.PermissionConstraints{Providers: []string{provider}}),
		}
	}
	t.Run("key bounded to another provider is refused", func(t *testing.T) {
		code, reached := serveKeyScope(t, rt, newKeyScopeAuth(keyPerms("azure")), keyScopeVictim, keyHeaders())
		assert.Equal(t, 403, code)
		assert.False(t, reached, "a refused request must not reach the mutation")
	})
	t.Run("key bounded to the row's provider is allowed", func(t *testing.T) {
		_, reached := serveKeyScope(t, rt, newKeyScopeAuth(keyPerms("aws")), keyScopeVictim, keyHeaders())
		assert.True(t, reached)
	})
}
