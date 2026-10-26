package auth

// Backend mirror of the frontend's validateGroupCombination (issue #78).
//
// The classification cases are a one-to-one port of the jest suite in
// frontend/src/__tests__/users.test.ts ("validateGroupCombination (issue
// #1405)") so the two implementations cannot drift without a test noticing.
//
// Every refusal case stubs NO CreateUser / UpdateUserAdminFields expectation
// on the mock store, so a removed guard fails loudly (testify panics on an
// un-stubbed call) instead of passing silently.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Fixtures mirroring the jest suite: v1 view-only, w1 write-capable, m1 mixed
// (view + create, so write-capable), e1 with no permissions (neutral).
var (
	comboViewOnly = &Group{ID: "v1", Name: "Viewers", Permissions: []Permission{{Action: ActionView, Resource: "aws"}}}
	comboViewTwo  = &Group{ID: "v2", Name: "Readers", Permissions: []Permission{{Action: ActionView, Resource: "gcp"}}}
	comboWrite    = &Group{ID: "w1", Name: "Editors", Permissions: []Permission{{Action: ActionCreate, Resource: "aws"}}}
	comboWriteTwo = &Group{ID: "w2", Name: "Admins", Permissions: []Permission{{Action: ActionDelete, Resource: "aws"}}}
	comboMixed    = &Group{ID: "m1", Name: "Mixed", Permissions: []Permission{
		{Action: ActionView, Resource: "a"}, {Action: ActionCreate, Resource: "b"},
	}}
	comboEmpty = &Group{ID: "e1", Name: "Empty", Permissions: []Permission{}}
)

// stubComboGroups registers GetGroup for every fixture plus an unknown ID
// that resolves to pgx.ErrNoRows (a deleted / never-existing group).
func stubComboGroups(ctx context.Context, m *MockStore) {
	for _, g := range []*Group{comboViewOnly, comboViewTwo, comboWrite, comboWriteTwo, comboMixed, comboEmpty} {
		m.On("GetGroup", ctx, g.ID).Return(g, nil).Maybe()
	}
	m.On("GetGroup", ctx, "unknown-uuid").Return(nil, pgx.ErrNoRows).Maybe()
}

// stubViewOnlyGroups registers each id as a distinct view-only group, for
// tests that send several groups but are not about their combination.
func stubViewOnlyGroups(ctx context.Context, m *MockStore, ids ...string) {
	for _, id := range ids {
		m.On("GetGroup", ctx, id).Return(&Group{
			ID: id, Name: id, Permissions: []Permission{{Action: ActionView, Resource: ResourcePlans}},
		}, nil).Once()
	}
}

func TestCheckGroupCombination_MirrorsFrontend(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		ids      []string
		wantErr  bool
		contains []string
	}{
		{name: "empty selection", ids: nil},
		{name: "single view-only group", ids: []string{"v1"}},
		{name: "single write-capable group", ids: []string{"w1"}},
		{name: "empty-permission group alone", ids: []string{"e1"}},
		{name: "view-only + empty-permission group (empty is neutral)", ids: []string{"v1", "e1"}},
		// Not in the jest suite, but follows from the frontend's
		// permissions.length > 0 guard: an empty group is not view-only.
		{name: "write-capable + empty-permission group (empty is neutral)", ids: []string{"w1", "e1"}},
		{name: "two write-capable groups", ids: []string{"w1", "w2"}},
		{name: "two view-only groups", ids: []string{"v1", "v2"}},
		{name: "unknown group ID", ids: []string{"unknown-uuid"}},
		{name: "unknown group ID next to a view-only group", ids: []string{"v1", "unknown-uuid"}},
		{
			name: "view-only + write-capable", ids: []string{"v1", "w1"}, wantErr: true,
			contains: []string{"contradictory", `"Viewers"`, `"Editors"`},
		},
		{name: "singular is for one view-only group", ids: []string{"v1", "w1"}, wantErr: true, contains: []string{" is view-only"}},
		{name: "plural are for several view-only groups", ids: []string{"v1", "v2", "w1"}, wantErr: true, contains: []string{" are view-only", `"Readers"`}},
		{name: "singular grants for one write-capable group", ids: []string{"v1", "w1"}, wantErr: true, contains: []string{"grants write"}},
		{name: "plural grant for several write-capable groups", ids: []string{"v1", "w1", "w2"}, wantErr: true, contains: []string{"grant write", `"Admins"`}},
		{name: "mixed group counts as write-capable", ids: []string{"v1", "m1"}, wantErr: true, contains: []string{`"Mixed"`}},
		{name: "order does not matter", ids: []string{"w1", "e1", "v1"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := new(MockStore)
			stubComboGroups(ctx, m)
			svc := createTestService(m, new(MockEmailSender))

			err := svc.checkGroupCombination(ctx, tc.ids)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrContradictoryGroups)
			for _, s := range tc.contains {
				assert.Contains(t, err.Error(), s)
			}
		})
	}
}

// The classification is read from the groups' stored permissions, not their
// names: a group called "Viewers" that grants a write verb is write-capable,
// and an oddly named view-only group is still view-only.
func TestCheckGroupCombination_UsesPermissionsNotNames(t *testing.T) {
	ctx := context.Background()
	m := new(MockStore)
	svc := createTestService(m, new(MockEmailSender))
	m.On("GetGroup", ctx, "a").Return(&Group{ID: "a", Name: "Viewers", Permissions: []Permission{{Action: ActionUpdate, Resource: "plans"}}}, nil)
	m.On("GetGroup", ctx, "b").Return(&Group{ID: "b", Name: "Administrators", Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}}}, nil)
	m.On("GetGroup", ctx, "c").Return(&Group{ID: "c", Name: "Power Users", Permissions: []Permission{{Action: ActionView, Resource: ResourceAll}}}, nil)

	require.NoError(t, svc.checkGroupCombination(ctx, []string{"a", "b"}), "two write-capable groups, whatever they are called")
	err := svc.checkGroupCombination(ctx, []string{"c", "b"})
	require.ErrorIs(t, err, ErrContradictoryGroups)
	assert.Contains(t, err.Error(), `"Power Users" is view-only`)
}

// A store failure is not "group missing": the check fails closed rather than
// letting an unclassified membership through.
func TestCheckGroupCombination_StoreErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	m := new(MockStore)
	svc := createTestService(m, new(MockEmailSender))
	m.On("GetGroup", ctx, "v1").Return(comboViewOnly, nil).Maybe()
	m.On("GetGroup", ctx, "w1").Return(nil, errors.New("connection reset"))

	err := svc.checkGroupCombination(ctx, []string{"v1", "w1"})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrContradictoryGroups)
	assert.Contains(t, err.Error(), "connection reset")
}

func TestCreateUser_RejectsContradictoryGroups(t *testing.T) {
	ctx := context.Background()
	m := new(MockStore)
	t.Cleanup(func() { m.AssertExpectations(t) })
	svc := createTestService(m, new(MockEmailSender))
	stubComboGroups(ctx, m)
	m.On("GetUserByEmail", ctx, "new@example.com").Return(nil, nil)

	// actorUserID "" is the trusted internal caller: the contradiction check
	// is about the membership itself, so it applies to every caller.
	_, err := svc.CreateUser(ctx, "", CreateUserRequest{
		Email:    "new@example.com",
		Password: "Sup3rSecretP@ssw0rd!",
		GroupIDs: []string{"v1", "w1"},
	})

	require.ErrorIs(t, err, ErrContradictoryGroups)
	m.AssertNotCalled(t, "CreateUser", mock.Anything, mock.Anything)
}

func TestCreateUser_AllowsConsistentGroups(t *testing.T) {
	ctx := context.Background()
	m := new(MockStore)
	t.Cleanup(func() { m.AssertExpectations(t) })
	svc := createTestService(m, new(MockEmailSender))
	stubComboGroups(ctx, m)
	m.On("GetUserByEmail", ctx, "new@example.com").Return(nil, nil)
	m.On("CreateUser", ctx, mock.AnythingOfType("*auth.User")).Return(nil).Once()

	res, err := svc.CreateUser(ctx, "", CreateUserRequest{
		Email:    "new@example.com",
		Password: "Sup3rSecretP@ssw0rd!",
		GroupIDs: []string{"v1", "v2", "e1"},
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"v1", "v2", "e1"}, res.User.GroupIDs)
}

// updateComboUser runs UpdateUser against a target whose stored membership is
// prior, with the given store expectations already registered by the caller.
func updateComboUser(t *testing.T, m *MockStore, prior []string, req UpdateUserRequest) (*User, error) {
	t.Helper()
	ctx := context.Background()
	m.On("GetUserByID", ctx, "user-1").Return(&User{
		ID: "user-1", Email: "user@example.com", GroupIDs: append([]string(nil), prior...), Active: true,
	}, nil).Once()
	svc := createTestService(m, new(MockEmailSender))
	return svc.UpdateUser(ctx, "", "user-1", req)
}

func TestUpdateUser_RejectsChangeToContradictoryGroups(t *testing.T) {
	cases := map[string]struct{ prior, next []string }{
		"add a write group to a view-only user":     {prior: []string{"v1"}, next: []string{"v1", "w1"}},
		"add a view-only group to a write user":     {prior: []string{"w1"}, next: []string{"w1", "v1"}},
		"replace with a contradictory set":          {prior: []string{"e1"}, next: []string{"v1", "m1"}},
		"legacy contradictory user, partial change": {prior: []string{"v1", "w1", "w2"}, next: []string{"v1", "w1"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m := new(MockStore)
			t.Cleanup(func() { m.AssertExpectations(t) })
			stubComboGroups(ctx, m)

			_, err := updateComboUser(t, m, tc.prior, UpdateUserRequest{GroupIDs: tc.next})

			require.ErrorIs(t, err, ErrContradictoryGroups)
			m.AssertNotCalled(t, "UpdateUserAdminFields", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// A user who already holds a contradictory membership (created before this
// check existed, or made contradictory later by a group edit) must not be
// locked out of edits that leave that membership alone.
func TestUpdateUser_LegacyContradictoryUserKeepsUnrelatedEdits(t *testing.T) {
	legacy := []string{"v1", "w1"}
	newEmail := "renamed@example.com"
	inactive := false

	cases := map[string]UpdateUserRequest{
		"rename without sending groups":    {Email: &newEmail},
		"rename resending the same groups": {Email: &newEmail, GroupIDs: []string{"v1", "w1"}},
		"same groups in another order":     {GroupIDs: []string{"w1", "v1"}},
		"same groups with a duplicate":     {GroupIDs: []string{"w1", "v1", "w1"}},
		"deactivate":                       {Active: &inactive},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m := new(MockStore)
			t.Cleanup(func() { m.AssertExpectations(t) })
			if req.Email != nil {
				m.On("GetUserByEmail", ctx, newEmail).Return(nil, pgx.ErrNoRows).Once()
			}
			if req.Active != nil {
				m.On("DeleteUserSessions", ctx, "user-1").Return(nil).Once()
			}
			m.On("UpdateUserAdminFields", ctx, mock.AnythingOfType("*auth.User"), mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

			_, err := updateComboUser(t, m, legacy, req)

			require.NoError(t, err)
			// An unchanged membership is not re-classified at all.
			m.AssertNotCalled(t, "GetGroup", mock.Anything, mock.Anything)
		})
	}
}

// The way out of a contradictory membership (dropping one side of it) must be
// accepted, or a legacy user could never be repaired through the API.
func TestUpdateUser_LegacyContradictoryUserCanBeResolved(t *testing.T) {
	for name, next := range map[string][]string{
		"drop the write group":     {"v1"},
		"drop the view-only group": {"w1"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m := new(MockStore)
			t.Cleanup(func() { m.AssertExpectations(t) })
			stubComboGroups(ctx, m)
			m.On("UpdateUserAdminFields", ctx, mock.AnythingOfType("*auth.User"), mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

			user, err := updateComboUser(t, m, []string{"v1", "w1"}, UpdateUserRequest{GroupIDs: next})

			require.NoError(t, err)
			assert.Equal(t, next, user.GroupIDs)
		})
	}
}
