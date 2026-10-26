package auth

// Membership grant-ceiling tests (issue #226): checkMembershipGrantCeiling
// applies the same ceiling to group-MEMBERSHIP writes that checkGrantCeiling
// (group_ceiling_permissions_test.go) applies to group-PERMISSION writes.
//
// Fixtures (ceilingActorID, ceilingActorGroupID, stubActorPermissions,
// newCeilingService) are shared with the permission-ceiling tests; see
// group_ceiling_fixtures_test.go.
//
// Every refusal case stubs NO CreateUser / UpdateUser expectation on the
// mock store, so a removed guard fails the test loudly (testify panics on an
// un-stubbed call) rather than passing silently.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var updateUsersOnly = []Permission{{Action: ActionUpdate, Resource: ResourceUsers}}

// Vector (a) from issue #226: an admin who by policy cannot add themself to
// Purchaser instead CREATES a second user directly into Purchaser -- a
// puppet account. Before the fix, CreateUser never saw an actor at all, so
// nothing stopped this.
func TestMembershipCeiling_CreateUser_AdminCannotPuppetIntoPurchaser(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	stubActorPermissions(ctx, mockStore, adminOnly)
	mockStore.On("GetUserByEmail", ctx, "puppet@example.com").Return(nil, nil)
	mockStore.On("GetGroup", ctx, DefaultPurchaserGroupID).Return(&Group{
		ID:   DefaultPurchaserGroupID,
		Name: GroupPurchaser,
		Permissions: []Permission{
			{Action: ActionExecute, Resource: ResourcePurchases},
		},
	}, nil)

	_, err := svc.CreateUser(ctx, ceilingActorID, CreateUserRequest{
		Email:    "puppet@example.com",
		Password: "Sup3rSecretP@ssw0rd!",
		GroupIDs: []string{DefaultPurchaserGroupID},
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionNotGrantable)
	assert.Contains(t, err.Error(), ActionExecute+":"+ResourcePurchases)
	mockStore.AssertNotCalled(t, "CreateUser", mock.Anything, mock.Anything)
}

// Vector (b) from issue #226: a non-admin custom group holding only
// update:users can move any user into Administrators. Before the fix,
// guardGroupChange returned nil unconditionally for a non-self edit.
func TestMembershipCeiling_UpdateUser_UpdateUsersOnlyCannotPromoteToAdmin(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	stubActorPermissions(ctx, mockStore, updateUsersOnly)
	mockStore.On("GetUserByID", ctx, ceilingTargetID).
		Return(&User{ID: ceilingTargetID, Active: true, GroupIDs: nil}, nil)
	mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
		ID:          DefaultAdminGroupID,
		Name:        "Administrators",
		Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
	}, nil)

	_, err := svc.UpdateUser(ctx, ceilingActorID, ceilingTargetID, UpdateUserRequest{
		GroupIDs: []string{DefaultAdminGroupID},
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionCeiling)
	assert.Contains(t, err.Error(), ActionAdmin+":"+ResourceAll)
	mockStore.AssertNotCalled(t, "UpdateUserAdminFields", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Self-edit variant of the test above (issue #226 review follow-up): an actor
// holding only update:users adds Administrators to their OWN membership.
// guardSelfEscalation alone admits this because it checks only that the actor
// holds update:users.
func TestMembershipCeiling_UpdateUser_UpdateUsersOnlyCannotSelfPromoteToAdmin(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	stubActorPermissions(ctx, mockStore, updateUsersOnly)
	mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
		ID:          DefaultAdminGroupID,
		Name:        "Administrators",
		Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
	}, nil)

	_, err := svc.UpdateUser(ctx, ceilingActorID, ceilingActorID, UpdateUserRequest{
		GroupIDs: []string{ceilingActorGroupID, DefaultAdminGroupID},
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionCeiling)
	assert.Contains(t, err.Error(), ActionAdmin+":"+ResourceAll)
	mockStore.AssertNotCalled(t, "UpdateUserAdminFields", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Non-admin variant of the self-promotion test above (issue #419): the
// ceiling is not an Administrators special case. An actor holding only
// update:users adds to their OWN membership a group carrying an ordinary,
// non-carved-out permission they do not hold. guardSelfEscalation admits it
// (update:users is held, nothing carved out, no account restriction), so
// only the #226 grant ceiling, measured against the actor's prior
// permissions, stands between the actor and view:recommendations.
func TestMembershipCeiling_UpdateUser_SelfEditNonAdminPermissionAboveCeilingRefused(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	recommendationViewers := &Group{
		ID:          "44444444-4444-4444-8444-000000000002",
		Name:        "Recommendation Viewers",
		Permissions: []Permission{{Action: ActionView, Resource: ResourceRecommendations}},
	}
	stubActorPermissions(ctx, mockStore, updateUsersOnly)
	mockStore.On("GetGroup", ctx, recommendationViewers.ID).Return(recommendationViewers, nil)

	_, err := svc.UpdateUser(ctx, ceilingActorID, ceilingActorID, UpdateUserRequest{
		GroupIDs: []string{ceilingActorGroupID, recommendationViewers.ID},
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionCeiling)
	assert.NotErrorIs(t, err, ErrSelfEscalation)
	assert.Contains(t, err.Error(), ActionView+":"+ResourceRecommendations)
	mockStore.AssertNotCalled(t, "UpdateUserAdminFields", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Negative control: an actor may still create a user in a group whose
// permissions their own effective set already covers.
func TestMembershipCeiling_CreateUser_AllowedWithinCeiling(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	ordinary := &Group{
		ID:          "44444444-4444-4444-8444-000000000001",
		Name:        "Viewers",
		Permissions: []Permission{{Action: ActionView, Resource: ResourcePlans}},
	}
	stubActorPermissions(ctx, mockStore, adminOnly)
	mockStore.On("GetUserByEmail", ctx, "viewer@example.com").Return(nil, nil)
	mockStore.On("GetGroup", ctx, ordinary.ID).Return(ordinary, nil)
	mockStore.On("CreateUser", ctx, mock.AnythingOfType("*auth.User")).Return(nil).Once()

	_, err := svc.CreateUser(ctx, ceilingActorID, CreateUserRequest{
		Email:    "viewer@example.com",
		Password: "Sup3rSecretP@ssw0rd!",
		GroupIDs: []string{ordinary.ID},
	})
	require.NoError(t, err)
}

// Trusted internal callers (actorUserID == "") bypass the ceiling entirely,
// so bootstrap/seeding paths are unaffected.
func TestMembershipCeiling_CreateUser_InternalCallerUnaffected(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	mockStore.On("GetUserByEmail", ctx, "seed@example.com").Return(nil, nil)
	mockStore.On("CreateUser", ctx, mock.AnythingOfType("*auth.User")).Return(nil).Once()

	_, err := svc.CreateUser(ctx, "", CreateUserRequest{
		Email:    "seed@example.com",
		Password: "Sup3rSecretP@ssw0rd!",
		GroupIDs: []string{DefaultPurchaserGroupID},
	})
	require.NoError(t, err)
}

// stubAdminTarget registers the target as an Administrators member with the
// given active state, plus the Administrators group the ceiling measures.
func stubAdminTarget(ctx context.Context, mockStore *MockStore, active bool) {
	target := &User{ID: ceilingTargetID, Active: active, GroupIDs: []string{DefaultAdminGroupID}}
	if !active {
		deactivatedAt := time.Now()
		target.DeactivatedAt = &deactivatedAt
	}
	mockStore.On("GetUserByID", ctx, ceilingTargetID).Return(target, nil)
	mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
		ID:          DefaultAdminGroupID,
		Name:        "Administrators",
		Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
	}, nil).Maybe()
}

// Issue #89 review: flipping Active restores or revokes the target's whole
// membership, so it is a grant like adding the groups. Before the fix the
// ceiling only ran when GroupIDs changed, and an update:users holder could
// bring a deactivated administrator back.
func TestMembershipCeiling_UpdateUser_UpdateUsersOnlyCannotReactivateAdmin(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	stubActorPermissions(ctx, mockStore, updateUsersOnly)
	stubAdminTarget(ctx, mockStore, false)

	active := true
	_, err := svc.UpdateUser(ctx, ceilingActorID, ceilingTargetID, UpdateUserRequest{Active: &active})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionCeiling)
	assert.Contains(t, err.Error(), ActionAdmin+":"+ResourceAll)
	mockStore.AssertNotCalled(t, "UpdateUserAdminFields", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// stubPurchaserTarget registers the target as a Purchaser member with the
// given active state, plus the Purchaser group holding the carved-out verb.
func stubPurchaserTarget(ctx context.Context, mockStore *MockStore, active bool) {
	target := &User{ID: ceilingTargetID, Active: active, GroupIDs: []string{DefaultPurchaserGroupID}}
	if !active {
		deactivatedAt := time.Now()
		target.DeactivatedAt = &deactivatedAt
	}
	mockStore.On("GetUserByID", ctx, ceilingTargetID).Return(target, nil)
	mockStore.On("GetGroup", ctx, DefaultPurchaserGroupID).Return(&Group{
		ID:          DefaultPurchaserGroupID,
		Name:        GroupPurchaser,
		Permissions: []Permission{{Action: ActionExecute, Resource: ResourcePurchases}},
	}, nil).Maybe()
}

// Deactivation is a revocation, so the ceiling must not run: the #923 carve-out
// would otherwise stop an administrator from locking out a Purchaser member
// during an incident, while protecting nothing (removals are not ceiling-checked).
func TestMembershipCeiling_UpdateUser_AdminCanDeactivatePurchaser(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	stubActorPermissionsMaybe(ctx, mockStore, adminOnly)
	stubPurchaserTarget(ctx, mockStore, true)
	mockStore.On("UpdateUserAdminFields", ctx, mock.AnythingOfType("*auth.User"), mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	mockStore.On("DeleteUserSessions", ctx, ceilingTargetID).Return(nil).Once()

	inactive := false
	user, err := svc.UpdateUser(ctx, ceilingActorID, ceilingTargetID, UpdateUserRequest{Active: &inactive})

	require.NoError(t, err)
	assert.False(t, user.Active)
	assert.NotNil(t, user.DeactivatedAt)
}

// Reactivation restores the carved-out money verb, which even an administrator
// cannot grant (issue #923).
func TestMembershipCeiling_UpdateUser_AdminCannotReactivatePurchaser(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	stubActorPermissions(ctx, mockStore, adminOnly)
	stubPurchaserTarget(ctx, mockStore, false)

	active := true
	_, err := svc.UpdateUser(ctx, ceilingActorID, ceilingTargetID, UpdateUserRequest{Active: &active})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionNotGrantable)
	assert.Contains(t, err.Error(), ActionExecute+":"+ResourcePurchases)
	mockStore.AssertNotCalled(t, "UpdateUserAdminFields", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Negative control: an admin actor may still reactivate and deactivate an
// administrator, and the last-admin guard still applies after the ceiling.
func TestMembershipCeiling_UpdateUser_AdminCanToggleAdminActive(t *testing.T) {
	ctx := context.Background()

	t.Run("reactivate", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		svc := newCeilingService(t, mockStore)
		stubActorPermissions(ctx, mockStore, adminOnly)
		stubAdminTarget(ctx, mockStore, false)
		mockStore.On("UpdateUserAdminFields", ctx, mock.AnythingOfType("*auth.User"), mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

		active := true
		user, err := svc.UpdateUser(ctx, ceilingActorID, ceilingTargetID, UpdateUserRequest{Active: &active})
		require.NoError(t, err)
		assert.True(t, user.Active)
		assert.Nil(t, user.DeactivatedAt)
	})

	t.Run("deactivate", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		svc := newCeilingService(t, mockStore)
		stubActorPermissionsMaybe(ctx, mockStore, adminOnly)
		stubAdminTarget(ctx, mockStore, true)
		mockStore.On("CountGroupMembers", ctx, DefaultAdminGroupID).Return(2, nil).Once()
		mockStore.On("UpdateUserAdminFields", ctx, mock.AnythingOfType("*auth.User"), mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
		mockStore.On("DeleteUserSessions", ctx, ceilingTargetID).Return(nil).Once()

		inactive := false
		user, err := svc.UpdateUser(ctx, ceilingActorID, ceilingTargetID, UpdateUserRequest{Active: &inactive})
		require.NoError(t, err)
		assert.False(t, user.Active)
		assert.NotNil(t, user.DeactivatedAt)
	})

	t.Run("deactivate last admin refused", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		svc := newCeilingService(t, mockStore)
		stubActorPermissionsMaybe(ctx, mockStore, adminOnly)
		stubAdminTarget(ctx, mockStore, true)
		mockStore.On("CountGroupMembers", ctx, DefaultAdminGroupID).Return(1, nil).Once()

		inactive := false
		_, err := svc.UpdateUser(ctx, ceilingActorID, ceilingTargetID, UpdateUserRequest{Active: &inactive})
		require.ErrorIs(t, err, ErrLastAdmin)
		mockStore.AssertNotCalled(t, "UpdateUserAdminFields", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}
