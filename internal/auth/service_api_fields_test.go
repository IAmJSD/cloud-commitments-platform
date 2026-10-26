package auth

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// Regression guard for the API adapters (CreateUserAPI, UpdateUserAPI,
// CreateGroupAPI, UpdateGroupAPI): every exported field of the request struct
// must reach the store, or be listed here with a reason. A field added to a
// request struct and forgotten in the adapter fails here (the #893 Role drop,
// issue #19).

// requireFieldsCovered fails when req has an exported field without an entry in
// checks (or an entry for a missing field), and when req leaves a field zero.
func requireFieldsCovered[C any](t *testing.T, req any, checks map[string]C) {
	t.Helper()
	v := reflect.ValueOf(req)
	var fields []string
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		fields = append(fields, f.Name)
		_, ok := checks[f.Name]
		require.Truef(t, ok, "unmapped field %s.%s: add it to the adapter and to this table", v.Type().Name(), f.Name)
		require.Falsef(t, v.Field(i).IsZero(), "%s.%s must be set to a non-zero value in this test", v.Type().Name(), f.Name)
	}
	sort.Strings(fields)
	tracked := make([]string, 0, len(checks))
	for name := range checks {
		tracked = append(tracked, name)
	}
	sort.Strings(tracked)
	require.Equal(t, fields, tracked, "table lists fields that %s no longer has", v.Type().Name())
}

// constrainedPermissions is what constrainedAPIPermissions must convert to.
var constrainedPermissions = []Permission{{
	Action: ActionView, Resource: ResourceRecommendations,
	Constraints: &PermissionConstraints{
		AccountIDs: []string{"acct-1"}, Providers: []string{"aws"}, Services: []string{"ec2"},
		Regions: []string{"us-east-1"}, MaxPurchaseAmount: 1234.5,
	},
}}

// constrainedAPIPermissions sets every APIPermission and constraint field; the
// constraints are the tenant boundary, so a dropped one must fail.
func constrainedAPIPermissions(t *testing.T) []APIPermission {
	t.Helper()
	perm := APIPermission{
		Action: ActionView, Resource: ResourceRecommendations,
		Constraints: &APIPermissionConstraint{
			Accounts: []string{"acct-1"}, Providers: []string{"aws"}, Services: []string{"ec2"},
			Regions: []string{"us-east-1"}, MaxAmount: 1234.5,
		},
	}
	requireFieldsCovered(t, perm, map[string]struct{}{"Action": {}, "Resource": {}, "Constraints": {}})
	requireFieldsCovered(t, *perm.Constraints, map[string]struct{}{
		"Accounts": {}, "Providers": {}, "Services": {}, "Regions": {}, "MaxAmount": {},
	})
	return []APIPermission{perm}
}

func TestAPIAdapters_CreateUser_AllFieldsReachStore(t *testing.T) {
	ctx := context.Background()
	req := APICreateUserRequest{Email: "new@example.com", Password: "SecurePass@123", Groups: []string{"group-1", "group-2"}}
	checks := map[string]func(*testing.T, *User){
		"Email":  func(t *testing.T, u *User) { assert.Equal(t, req.Email, u.Email) },
		"Groups": func(t *testing.T, u *User) { assert.Equal(t, req.Groups, u.GroupIDs) },
		// Never stored verbatim; persisted only as a bcrypt hash.
		"Password": func(t *testing.T, u *User) {
			assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)))
		},
	}
	requireFieldsCovered(t, req, checks)

	store := new(MockStore)
	svc := createTestService(store, new(MockEmailSender))
	var got *User
	store.On("GetUserByEmail", ctx, req.Email).Return(nil, nil).Once()
	// The issue #78 combination check reads each group; unknown groups are
	// skipped, so returning none keeps this test about field plumbing.
	store.On("GetGroup", ctx, mock.Anything).Return(nil, nil).Maybe()
	store.On("CreateUser", ctx, mock.AnythingOfType("*auth.User")).
		Run(func(args mock.Arguments) { got = args.Get(1).(*User) }).Return(nil).Once()
	_, err := svc.CreateUserAPI(ctx, "", req)
	require.NoError(t, err)
	require.NotNil(t, got)
	for name, check := range checks {
		t.Run(name, func(t *testing.T) { check(t, got) })
	}
}

func TestAPIAdapters_UpdateUser_AllFieldsReachStore(t *testing.T) {
	ctx := context.Background()
	inactive := false
	existing := func() *User {
		return &User{ID: "user-123", Email: "old@example.com", GroupIDs: []string{"old-group"}, Active: true}
	}
	run := func(t *testing.T, req APIUpdateUserRequest) *User {
		store := new(MockStore)
		svc := createTestService(store, new(MockEmailSender))
		var got *User
		store.On("GetUserByID", ctx, "user-123").Return(existing(), nil).Once()
		store.On("GetUserByEmail", ctx, mock.Anything).Return(nil, nil).Maybe()
		store.On("DeleteUserSessions", ctx, "user-123").Return(nil).Maybe()
		store.On("UpdateUserAdminFields", ctx, mock.AnythingOfType("*auth.User"), mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) { got = args.Get(1).(*User) }).Return(nil).Once()
		_, err := svc.UpdateUserAPI(ctx, "", "user-123", req)
		require.NoError(t, err)
		require.NotNil(t, got)
		return got
	}

	req := APIUpdateUserRequest{Email: "new@example.com", Groups: []string{"group-2"}, Active: &inactive}
	checks := map[string]func(*testing.T, *User){
		"Email":  func(t *testing.T, u *User) { assert.Equal(t, req.Email, u.Email) },
		"Groups": func(t *testing.T, u *User) { assert.Equal(t, req.Groups, u.GroupIDs) },
		"Active": func(t *testing.T, u *User) { assert.False(t, u.Active) },
	}
	requireFieldsCovered(t, req, checks)

	t.Run("set", func(t *testing.T) {
		got := run(t, req)
		for name, check := range checks {
			t.Run(name, func(t *testing.T) { check(t, got) })
		}
	})
	t.Run("unset fields do not overwrite", func(t *testing.T) {
		got := run(t, APIUpdateUserRequest{})
		assert.Equal(t, existing().Email, got.Email)
		assert.Equal(t, existing().GroupIDs, got.GroupIDs)
		assert.True(t, got.Active)
	})
}

func TestAPIAdapters_CreateGroup_AllFieldsReachStore(t *testing.T) {
	ctx := context.Background()
	req := APICreateGroupRequest{
		Name:            "Test Group",
		Description:     "Test description",
		Permissions:     constrainedAPIPermissions(t),
		AllowedAccounts: []string{"acct-1"},
	}
	checks := map[string]func(*testing.T, *Group){
		"Name":        func(t *testing.T, g *Group) { assert.Equal(t, req.Name, g.Name) },
		"Description": func(t *testing.T, g *Group) { assert.Equal(t, req.Description, g.Description) },
		"Permissions": func(t *testing.T, g *Group) {
			assert.Equal(t, constrainedPermissions, g.Permissions)
		},
		"AllowedAccounts": func(t *testing.T, g *Group) { assert.Equal(t, req.AllowedAccounts, g.AllowedAccounts) },
	}
	requireFieldsCovered(t, req, checks)

	store := new(MockStore)
	svc := createTestService(store, new(MockEmailSender))
	var got *Group
	store.On("CreateGroup", ctx, mock.AnythingOfType("*auth.Group")).
		Run(func(args mock.Arguments) { got = args.Get(1).(*Group) }).Return(nil).Once()
	_, err := svc.CreateGroupAPI(ctx, AdminAPIKeyActorID, req)
	require.NoError(t, err)
	require.NotNil(t, got)
	for name, check := range checks {
		t.Run(name, func(t *testing.T) { check(t, got) })
	}
}

func TestAPIAdapters_UpdateGroup_AllFieldsReachStore(t *testing.T) {
	ctx := context.Background()
	existing := func() *Group {
		return &Group{
			ID: "group-123", Name: "Old", Description: "Old description",
			Permissions:     []Permission{{Action: ActionView, Resource: ResourceAll}},
			AllowedAccounts: []string{"old-acct"},
		}
	}
	run := func(t *testing.T, req APIUpdateGroupRequest) *Group {
		store := new(MockStore)
		svc := createTestService(store, new(MockEmailSender))
		var got *Group
		store.On("GetGroup", ctx, "group-123").Return(existing(), nil).Once()
		store.On("UpdateGroup", ctx, mock.AnythingOfType("*auth.Group")).
			Run(func(args mock.Arguments) { got = args.Get(1).(*Group) }).Return(nil).Once()
		_, err := svc.UpdateGroupAPI(ctx, AdminAPIKeyActorID, "group-123", req)
		require.NoError(t, err)
		require.NotNil(t, got)
		return got
	}

	req := APIUpdateGroupRequest{
		Name:            "New",
		Description:     "New description",
		Permissions:     constrainedAPIPermissions(t),
		AllowedAccounts: []string{"acct-1"},
	}
	checks := map[string]func(*testing.T, *Group){
		"Name":        func(t *testing.T, g *Group) { assert.Equal(t, req.Name, g.Name) },
		"Description": func(t *testing.T, g *Group) { assert.Equal(t, req.Description, g.Description) },
		"Permissions": func(t *testing.T, g *Group) {
			assert.Equal(t, constrainedPermissions, g.Permissions)
		},
		"AllowedAccounts": func(t *testing.T, g *Group) { assert.Equal(t, req.AllowedAccounts, g.AllowedAccounts) },
	}
	requireFieldsCovered(t, req, checks)

	t.Run("set", func(t *testing.T) {
		got := run(t, req)
		for name, check := range checks {
			t.Run(name, func(t *testing.T) { check(t, got) })
		}
	})
	t.Run("unset fields do not overwrite", func(t *testing.T) {
		got := run(t, APIUpdateGroupRequest{})
		assert.Equal(t, existing().Name, got.Name)
		assert.Equal(t, existing().Description, got.Description)
		assert.Equal(t, existing().Permissions, got.Permissions)
		assert.Equal(t, existing().AllowedAccounts, got.AllowedAccounts)
	})
}

func TestAPIAdapters_CreateAPIKey_AllFieldsReachStore(t *testing.T) {
	ctx := context.Background()
	expiresAt := time.Now().Add(24 * time.Hour)
	req := APICreateAPIKeyRequest{
		Name:        "scoped",
		Password:    testAPIKeyPassword,
		Permissions: constrainedAPIPermissions(t),
		ExpiresAt:   &expiresAt,
	}
	checks := map[string]func(*testing.T, *UserAPIKey){
		"Name":        func(t *testing.T, k *UserAPIKey) { assert.Equal(t, req.Name, k.Name) },
		"Password":    func(t *testing.T, k *UserAPIKey) {}, // re-verified against the user's hash, never stored
		"Permissions": func(t *testing.T, k *UserAPIKey) { assert.Equal(t, constrainedPermissions, k.Permissions) },
		"ExpiresAt":   func(t *testing.T, k *UserAPIKey) { assert.True(t, req.ExpiresAt.Equal(*k.ExpiresAt)) },
	}
	requireFieldsCovered(t, req, checks)

	store := new(MockStore)
	svc := &Service{store: store}
	store.On("GetUserByID", ctx, "user-1").Return(&User{
		ID: "user-1", Email: "test@example.com", Active: true,
		PasswordHash: testAPIKeyPasswordHash, GroupIDs: []string{DefaultAdminGroupID},
	}, nil)
	store.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
		ID: DefaultAdminGroupID, Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
	}, nil)
	var got *UserAPIKey
	store.On("CreateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey")).
		Run(func(args mock.Arguments) { got = args.Get(1).(*UserAPIKey) }).Return(nil)
	_, err := svc.CreateAPIKeyAPI(ctx, "user-1", req)
	require.NoError(t, err)
	require.NotNil(t, got)
	for name, check := range checks {
		t.Run(name, func(t *testing.T) { check(t, got) })
	}
}
