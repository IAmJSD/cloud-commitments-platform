package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #62: GET /api/auth/me/permissions describes a user API key with
// GetAPIKeyEffectivePermissionsAPI. It must return the set
// HasAPIKeyPermissionAPI enforces (key ∩ owner, key's constraints) and fail
// closed on every lookup problem.

const (
	effKeyID   = "key-62"
	effOwnerID = "owner-62"
	effGroupID = "group-62"
)

func effStore(t *testing.T, key *UserAPIKey, owner *User, ownerPerms []Permission) *MockStore {
	t.Helper()
	store := new(MockStore)
	store.On("GetAPIKeyByID", mock.Anything, effKeyID).Return(key, nil).Maybe()
	store.On("GetUserByID", mock.Anything, effOwnerID).Return(owner, nil).Maybe()
	store.On("GetGroup", mock.Anything, effGroupID).Return(&Group{ID: effGroupID, Permissions: ownerPerms}, nil).Maybe()
	return store
}

func effOwner() *User {
	return &User{ID: effOwnerID, Email: "owner@example.com", Active: true, GroupIDs: []string{effGroupID}}
}

func effKey(perms ...Permission) *UserAPIKey {
	return &UserAPIKey{ID: effKeyID, UserID: effOwnerID, IsActive: true, Permissions: perms}
}

func TestGetAPIKeyEffectivePermissionsAPI_AdminOwnerViewOnlyKey(t *testing.T) {
	key := effKey(Permission{Action: ActionView, Resource: ResourceRecommendations})
	s := &Service{store: effStore(t, key, effOwner(), []Permission{{Action: ActionAdmin, Resource: ResourceAll}})}

	got, err := s.GetAPIKeyEffectivePermissionsAPI(context.Background(), effKeyID, effOwnerID)
	require.NoError(t, err)
	assert.Equal(t, []APIPermission{{Action: ActionView, Resource: ResourceRecommendations}}, got)
}

// The intersection drops key permissions the owner lacks and keeps the KEY's
// constraints on what survives, not the owner's.
func TestGetAPIKeyEffectivePermissionsAPI_KeyConstraintsAndIntersection(t *testing.T) {
	key := effKey(
		Permission{Action: ActionExecute, Resource: ResourcePurchases,
			Constraints: &PermissionConstraints{MaxPurchaseAmount: 100, Providers: []string{"aws"}}},
		Permission{Action: ActionDelete, Resource: ResourceUsers},
	)
	ownerPerms := []Permission{
		{Action: ActionExecute, Resource: ResourcePurchases, Constraints: &PermissionConstraints{MaxPurchaseAmount: 1000}},
		{Action: ActionView, Resource: ResourceRecommendations},
	}
	s := &Service{store: effStore(t, key, effOwner(), ownerPerms)}

	got, err := s.GetAPIKeyEffectivePermissionsAPI(context.Background(), effKeyID, effOwnerID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, ActionExecute, got[0].Action)
	assert.Equal(t, ResourcePurchases, got[0].Resource)
	require.NotNil(t, got[0].Constraints)
	assert.InDelta(t, 100, got[0].Constraints.MaxAmount, 0)
	assert.Equal(t, []string{"aws"}, got[0].Constraints.Providers)
}

func TestGetAPIKeyEffectivePermissionsAPI_UnscopedKeyGrantsNothing(t *testing.T) {
	s := &Service{store: effStore(t, effKey(), effOwner(), []Permission{{Action: ActionAdmin, Resource: ResourceAll}})}

	got, err := s.GetAPIKeyEffectivePermissionsAPI(context.Background(), effKeyID, effOwnerID)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGetAPIKeyEffectivePermissionsAPI_FailsClosed(t *testing.T) {
	view := Permission{Action: ActionView, Resource: ResourceRecommendations}
	past := time.Now().Add(-time.Hour)
	admin := []Permission{{Action: ActionAdmin, Resource: ResourceAll}}
	cases := []struct {
		store func(t *testing.T) *MockStore
		name  string
	}{
		{name: "key lookup error", store: func(t *testing.T) *MockStore {
			s := new(MockStore)
			s.On("GetAPIKeyByID", mock.Anything, effKeyID).Return((*UserAPIKey)(nil), errors.New("db down"))
			return s
		}},
		{name: "key not found", store: func(t *testing.T) *MockStore {
			s := new(MockStore)
			s.On("GetAPIKeyByID", mock.Anything, effKeyID).Return((*UserAPIKey)(nil), nil)
			return s
		}},
		{name: "key owned by someone else", store: func(t *testing.T) *MockStore {
			k := effKey(view)
			k.UserID = "someone-else"
			return effStore(t, k, effOwner(), admin)
		}},
		{name: "revoked key", store: func(t *testing.T) *MockStore {
			k := effKey(view)
			k.IsActive = false
			return effStore(t, k, effOwner(), admin)
		}},
		{name: "expired key", store: func(t *testing.T) *MockStore {
			k := effKey(view)
			k.ExpiresAt = &past
			return effStore(t, k, effOwner(), admin)
		}},
		{name: "inactive owner", store: func(t *testing.T) *MockStore {
			o := effOwner()
			o.Active = false
			return effStore(t, effKey(view), o, admin)
		}},
		{name: "owner password rotated", store: func(t *testing.T) *MockStore {
			o := effOwner()
			o.PasswordVersion = 7
			return effStore(t, effKey(view), o, admin)
		}},
		{name: "owner group lookup error", store: func(t *testing.T) *MockStore {
			s := new(MockStore)
			s.On("GetAPIKeyByID", mock.Anything, effKeyID).Return(effKey(view), nil)
			s.On("GetUserByID", mock.Anything, effOwnerID).Return(effOwner(), nil)
			s.On("GetGroup", mock.Anything, effGroupID).Return((*Group)(nil), errors.New("db down"))
			return s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{store: tc.store(t)}
			got, err := s.GetAPIKeyEffectivePermissionsAPI(context.Background(), effKeyID, effOwnerID)
			require.Error(t, err)
			assert.Nil(t, got)
		})
	}
}
