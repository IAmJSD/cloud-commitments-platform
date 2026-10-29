package auth

// Issue #1409: the view-only/write-capable combination rule (issue #78)
// applied to a group update. Changing a group's permissions changes the
// combination of every member without any user write.
//
// Every refusal case stubs NO UpdateGroup expectation on the mock store, so a
// removed guard fails loudly (testify panics on an un-stubbed call).

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const changeTargetID = "t1"

var (
	viewPerm  = APIPermission{Action: ActionView, Resource: ResourcePlans}
	writePerm = APIPermission{Action: ActionCreate, Resource: ResourcePlans}
)

func changeTarget(perms ...Permission) *Group {
	return &Group{ID: changeTargetID, Name: "Team", Permissions: perms}
}

// newChangeService stubs the target group and the combo fixtures on m. users
// nil means ListUsers must not be called at all. The caller creates m so the
// mocks package's assertion guard can resolve its type.
func newChangeService(t *testing.T, m *MockStore, target *Group, users []User) *Service {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() { m.AssertExpectations(t) })
	m.On("GetGroup", ctx, changeTargetID).Return(target, nil)
	stubComboGroups(ctx, m)
	if users != nil {
		m.On("ListUsers", ctx).Return(users, nil).Once()
	}
	return createTestService(m, new(MockEmailSender))
}

func update(svc *Service, perms ...APIPermission) (*APIGroup, error) {
	return svc.UpdateGroupAPI(context.Background(), AdminAPIKeyActorID, changeTargetID,
		APIUpdateGroupRequest{Name: "Team", Permissions: perms})
}

func expectSave(m *MockStore) {
	m.On("UpdateGroup", mock.Anything, mock.AnythingOfType("*auth.Group")).Return(nil).Once()
}

func TestUpdateGroupAPI_RefusesWritePermissionOnGroupWithViewOnlyMembers(t *testing.T) {
	m := new(MockStore)
	svc := newChangeService(t, m, changeTarget(Permission{Action: ActionView, Resource: ResourcePlans}), []User{
		{ID: "u1", GroupIDs: []string{changeTargetID, "v1"}},
		{ID: "u2", GroupIDs: []string{"v2", changeTargetID}},
		{ID: "u3", GroupIDs: []string{changeTargetID}},       // alone: unaffected
		{ID: "u4", GroupIDs: []string{changeTargetID, "e1"}}, // empty group is neutral
		{ID: "u5", GroupIDs: []string{"v1", "w1"}},           // not a member
	})

	_, err := update(svc, viewPerm, writePerm)

	require.ErrorIs(t, err, ErrContradictoryGroups)
	assert.Contains(t, err.Error(), `group "Team" would grant write permissions, but 2 of its members also belong to a view-only group`)
	m.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
}

func TestUpdateGroupAPI_RefusesViewOnlyGroupWithWriteCapableMembers(t *testing.T) {
	m := new(MockStore)
	svc := newChangeService(t, m, changeTarget(Permission{Action: ActionCreate, Resource: ResourcePlans}), []User{
		{ID: "u1", GroupIDs: []string{changeTargetID, "m1"}}, // mixed counts as write-capable
		{ID: "u2", GroupIDs: []string{changeTargetID, "v1"}},
	})

	_, err := update(svc, viewPerm)

	require.ErrorIs(t, err, ErrContradictoryGroups)
	assert.Contains(t, err.Error(), `group "Team" would become view-only, but 1 of its members also belongs to a write-capable group`)
	m.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
}

func TestUpdateGroupAPI_FirstPermissionsOnEmptyGroupAreChecked(t *testing.T) {
	// A group with no permissions takes no part in the rule; its first
	// permissions can still contradict a member.
	m := new(MockStore)
	svc := newChangeService(t, m, changeTarget(), []User{{ID: "u1", GroupIDs: []string{changeTargetID, "w1"}}})

	_, err := update(svc, viewPerm)

	require.ErrorIs(t, err, ErrContradictoryGroups)
	m.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
}

func TestUpdateGroupAPI_RoleChangeWithoutConflictingMembersSucceeds(t *testing.T) {
	m := new(MockStore)
	svc := newChangeService(t, m, changeTarget(Permission{Action: ActionView, Resource: ResourcePlans}), []User{
		{ID: "u1", GroupIDs: []string{changeTargetID, "w1"}},
		{ID: "u2", GroupIDs: []string{changeTargetID}},
		{ID: "u3", GroupIDs: []string{"v1"}},
	})
	expectSave(m)

	got, err := update(svc, viewPerm, writePerm)

	require.NoError(t, err)
	assert.Len(t, got.Permissions, 2)
}

// A member who was already contradictory (assigned before #1205) does not
// block the update, as an unchanged membership does not block a user edit.
func TestUpdateGroupAPI_AlreadyContradictoryMemberDoesNotBlock(t *testing.T) {
	m := new(MockStore)
	svc := newChangeService(t, m, changeTarget(Permission{Action: ActionView, Resource: ResourcePlans}), []User{
		{ID: "u1", GroupIDs: []string{changeTargetID, "w1", "v1"}},
	})
	expectSave(m)

	_, err := update(svc, writePerm)

	require.NoError(t, err)
}

// The fast path: when the group keeps its role in the rule, no member's
// combination can change, so no user is read. newChangeService registers no
// ListUsers, so reading users would panic.
func TestUpdateGroupAPI_SameRoleSkipsMemberCheck(t *testing.T) {
	cases := []struct {
		name   string
		target *Group
		perms  []APIPermission
	}{
		{"permissions omitted", changeTarget(Permission{Action: ActionView, Resource: ResourcePlans}), nil},
		{"permissions unchanged", changeTarget(Permission{Action: ActionView, Resource: ResourcePlans}), []APIPermission{viewPerm}},
		{"view-only stays view-only", changeTarget(Permission{Action: ActionView, Resource: ResourcePlans}),
			[]APIPermission{viewPerm, {Action: ActionView, Resource: ResourceRecommendations}}},
		{"write-capable stays write-capable", changeTarget(Permission{Action: ActionCreate, Resource: ResourcePlans}),
			[]APIPermission{viewPerm, {Action: ActionUpdate, Resource: ResourcePlans}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := new(MockStore)
			svc := newChangeService(t, m, tc.target, nil)
			expectSave(m)

			_, err := update(svc, tc.perms...)

			require.NoError(t, err)
			m.AssertNotCalled(t, "ListUsers", mock.Anything)
		})
	}
}

// A store failure fails closed: the update is refused, and not as a
// contradiction.
func TestUpdateGroupAPI_MemberCheckStoreErrorFailsClosed(t *testing.T) {
	t.Run("listing users fails", func(t *testing.T) {
		ctx := context.Background()
		m := new(MockStore)
		t.Cleanup(func() { m.AssertExpectations(t) })
		m.On("GetGroup", ctx, changeTargetID).Return(changeTarget(), nil)
		m.On("ListUsers", ctx).Return(nil, errors.New("connection reset"))
		svc := createTestService(m, new(MockEmailSender))

		_, err := update(svc, writePerm)

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrContradictoryGroups)
		assert.Contains(t, err.Error(), "connection reset")
		m.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
	})
	t.Run("loading a member's other group fails", func(t *testing.T) {
		ctx := context.Background()
		m := new(MockStore)
		t.Cleanup(func() { m.AssertExpectations(t) })
		m.On("GetGroup", ctx, changeTargetID).Return(changeTarget(), nil)
		m.On("GetGroup", ctx, "x1").Return(nil, errors.New("connection reset"))
		m.On("ListUsers", ctx).Return([]User{{ID: "u1", GroupIDs: []string{changeTargetID, "x1"}}}, nil)
		svc := createTestService(m, new(MockEmailSender))

		_, err := update(svc, writePerm)

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrContradictoryGroups)
		m.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
	})
}
