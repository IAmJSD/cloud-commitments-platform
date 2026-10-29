package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// testAPIKeyPassword and testAPIKeyPasswordHash back every TestService_CreateAPIKey
// fixture: CreateAPIKey now re-verifies the caller's password before minting a
// key (issue #102), so every user fixture in this file needs a matching hash.
// Hashed once at MinCost and reused across subtests to keep bcrypt off the hot
// path of each assertion.
var (
	testAPIKeyPassword     = "CorrectHorse@123"
	testAPIKeyPasswordHash = func() string {
		hash, err := newTestService().hashPassword(testAPIKeyPassword)
		if err != nil {
			panic(err)
		}
		return hash
	}()
)

func TestService_CreateAPIKey(t *testing.T) {
	ctx := context.Background()
	validExpiry := time.Now().Add(24 * time.Hour)

	t.Run("successfully create API key", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
			GroupIDs:     []string{DefaultAdminGroupID},
		}

		permissions := []Permission{
			{Action: ActionView, Resource: ResourceRecommendations},
		}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
			ID:          DefaultAdminGroupID,
			Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
		}, nil)
		mockStore.On("CreateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey")).Return(nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword, permissions, &validExpiry)

		require.NoError(t, err)
		require.NotNil(t, keyInfo)
		assert.NotEmpty(t, apiKey)
		assert.Equal(t, "user-123", keyInfo.UserID)
		assert.Equal(t, "Test Key", keyInfo.Name)
		assert.Equal(t, permissions, keyInfo.Permissions)
		assert.True(t, keyInfo.IsActive)
		assert.Len(t, keyInfo.KeyPrefix, 8)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when user not found", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		mockStore.On("GetUserByID", ctx, "user-123").Return(nil, assert.AnError)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword, []Permission{}, &validExpiry)

		assert.Error(t, err)
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when user is inactive", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:     "user-123",
			Email:  "test@example.com",
			Active: false,
		}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword, []Permission{}, &validExpiry)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not active")
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when password is wrong", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
		}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", "wrong-password", []Permission{}, &validExpiry)

		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrAPIKeyInvalidPassword)
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when name is empty", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
		}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "", testAPIKeyPassword, []Permission{}, &validExpiry)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name is required")
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when expiresAt is nil", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
		}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword,
			[]Permission{{Action: ActionView, Resource: ResourceRecommendations}}, nil)

		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrAPIKeyExpiresAtRequired)
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when expiresAt exceeds MaxAPIKeyLifetime", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
		}
		tooFar := time.Now().Add(MaxAPIKeyLifetime + 24*time.Hour)

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword,
			[]Permission{{Action: ActionView, Resource: ResourceRecommendations}}, &tooFar)

		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrAPIKeyExpiresAtTooFar)
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertExpectations(t)
	})

	// CodeRabbit finding on this PR: a past expiresAt slipped through the
	// pre-fix version of this check (which only rejected values too far in
	// the future), minting a key that was already expired.
	t.Run("fail when expiresAt is in the past", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
		}
		pastExpiry := time.Now().Add(-1 * time.Hour)

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword,
			[]Permission{{Action: ActionView, Resource: ResourceRecommendations}}, &pastExpiry)

		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrAPIKeyExpiresAtInPast)
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertExpectations(t)
	})

	t.Run("successfully create API key with expiration", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
			GroupIDs:     []string{DefaultAdminGroupID},
		}

		expiresAt := time.Now().Add(30 * 24 * time.Hour)
		permissions := []Permission{
			{Action: ActionView, Resource: ResourceRecommendations},
		}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
			ID:          DefaultAdminGroupID,
			Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
		}, nil)
		mockStore.On("CreateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey")).Return(nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword, permissions, &expiresAt)

		require.NoError(t, err)
		require.NotNil(t, keyInfo)
		assert.NotEmpty(t, apiKey)
		assert.NotNil(t, keyInfo.ExpiresAt)
		assert.Equal(t, expiresAt.Unix(), keyInfo.ExpiresAt.Unix())
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when store create fails", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
			GroupIDs:     []string{DefaultAdminGroupID},
		}
		permissions := []Permission{
			{Action: ActionView, Resource: ResourceRecommendations},
		}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
			ID:          DefaultAdminGroupID,
			Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
		}, nil)
		mockStore.On("CreateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey")).Return(assert.AnError)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword, permissions, &validExpiry)

		assert.Error(t, err)
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when permissions is empty", func(t *testing.T) {
		// Regression test for issue #61: an unscoped key used to be accepted
		// and would silently inherit the owner's full permission set at
		// request time. CreateAPIKey must now reject it outright so scoping
		// is a deliberate choice, not a default.
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
		}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword, []Permission{}, &validExpiry)

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEmptyAPIKeyPermissions)
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertNotCalled(t, "CreateAPIKey", mock.Anything, mock.Anything)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when permissions is nil", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{
			ID:           "user-123",
			Email:        "test@example.com",
			Active:       true,
			PasswordHash: testAPIKeyPasswordHash,
		}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		apiKey, keyInfo, err := service.CreateAPIKey(ctx, "user-123", "Test Key", testAPIKeyPassword, nil, &validExpiry)

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEmptyAPIKeyPermissions)
		assert.Empty(t, apiKey)
		assert.Nil(t, keyInfo)
		mockStore.AssertNotCalled(t, "CreateAPIKey", mock.Anything, mock.Anything)
		mockStore.AssertExpectations(t)
	})
}

func TestService_ListUserAPIKeys(t *testing.T) {
	ctx := context.Background()

	t.Run("successfully list API keys", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		now := time.Now()
		expectedKeys := []*UserAPIKey{
			{
				ID:        "key-1",
				UserID:    "user-123",
				Name:      "Key 1",
				KeyPrefix: "prefix1",
				IsActive:  true,
				CreatedAt: now,
			},
			{
				ID:        "key-2",
				UserID:    "user-123",
				Name:      "Key 2",
				KeyPrefix: "prefix2",
				IsActive:  true,
				CreatedAt: now,
			},
		}

		user := &User{ID: "user-123", Email: "test@example.com", Active: true}
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("ListAPIKeysByUser", ctx, "user-123").Return(expectedKeys, nil)

		keys, err := service.ListUserAPIKeys(ctx, "user-123")

		require.NoError(t, err)
		assert.Len(t, keys, 2)
		assert.Equal(t, expectedKeys, keys)
		mockStore.AssertExpectations(t)
	})

	t.Run("return error when user not found", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		mockStore.On("GetUserByID", ctx, "user-123").Return(nil, assert.AnError)

		keys, err := service.ListUserAPIKeys(ctx, "user-123")

		assert.Error(t, err)
		assert.Nil(t, keys)
		mockStore.AssertExpectations(t)
	})

	t.Run("return error when store fails", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		user := &User{ID: "user-123", Email: "test@example.com", Active: true}
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("ListAPIKeysByUser", ctx, "user-123").Return(nil, assert.AnError)

		keys, err := service.ListUserAPIKeys(ctx, "user-123")

		assert.Error(t, err)
		assert.Nil(t, keys)
		mockStore.AssertExpectations(t)
	})
}

func TestService_GetAPIKeyByHash(t *testing.T) {
	ctx := context.Background()

	t.Run("successfully get API key by hash", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		expectedKey := &UserAPIKey{
			ID:        "key-1",
			UserID:    "user-123",
			Name:      "Test Key",
			KeyPrefix: "prefix1",
			KeyHash:   "hash123",
			IsActive:  true,
		}

		mockStore.On("GetAPIKeyByHash", ctx, "hash123").Return(expectedKey, nil)

		key, err := service.GetAPIKeyByHash(ctx, "hash123")

		require.NoError(t, err)
		assert.Equal(t, expectedKey, key)
		mockStore.AssertExpectations(t)
	})

	t.Run("return error when store fails", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		mockStore.On("GetAPIKeyByHash", ctx, "hash123").Return(nil, assert.AnError)

		key, err := service.GetAPIKeyByHash(ctx, "hash123")

		assert.Error(t, err)
		assert.Nil(t, key)
		mockStore.AssertExpectations(t)
	})
}

func TestService_RevokeAPIKey(t *testing.T) {
	ctx := context.Background()

	t.Run("successfully revoke API key", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		existingKey := &UserAPIKey{
			ID:       "key-1",
			UserID:   "user-123",
			IsActive: true,
		}
		user := &User{ID: "user-123"}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(existingKey, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("UpdateAPIKey", ctx, mock.MatchedBy(func(key *UserAPIKey) bool {
			return key.ID == "key-1" && !key.IsActive
		})).Return(nil)

		err := service.RevokeAPIKey(ctx, "user-123", "key-1")

		require.NoError(t, err)
		mockStore.AssertExpectations(t)
	})

	t.Run("return error when key not found", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(nil, assert.AnError)

		err := service.RevokeAPIKey(ctx, "user-123", "key-1")

		assert.Error(t, err)
		mockStore.AssertExpectations(t)
	})

	t.Run("return error when update fails", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		existingKey := &UserAPIKey{
			ID:       "key-1",
			UserID:   "user-123",
			IsActive: true,
		}
		user := &User{ID: "user-123"}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(existingKey, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("UpdateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey")).Return(assert.AnError)

		err := service.RevokeAPIKey(ctx, "user-123", "key-1")

		assert.Error(t, err)
		mockStore.AssertExpectations(t)
	})

	t.Run("non-admin user cannot revoke another user's API key", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		existingKey := &UserAPIKey{
			ID:       "key-1",
			UserID:   "user-456",
			IsActive: true,
		}
		user := &User{ID: "user-123"}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(existingKey, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		err := service.RevokeAPIKey(ctx, "user-123", "key-1")

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unauthorized")
		mockStore.AssertExpectations(t)
	})

	t.Run("admin user can revoke another user's API key", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		existingKey := &UserAPIKey{
			ID:       "key-1",
			UserID:   "user-456",
			IsActive: true,
		}
		// Admin == member of the Administrators group ({admin, *}).
		user := &User{ID: "user-123", GroupIDs: []string{DefaultAdminGroupID}}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(existingKey, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
			ID:          DefaultAdminGroupID,
			Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
		}, nil)
		mockStore.On("UpdateAPIKey", ctx, mock.MatchedBy(func(key *UserAPIKey) bool {
			return key.ID == "key-1" && !key.IsActive
		})).Return(nil)

		err := service.RevokeAPIKey(ctx, "user-123", "key-1")

		require.NoError(t, err)
		mockStore.AssertExpectations(t)
	})
}

func TestService_DeleteAPIKey(t *testing.T) {
	ctx := context.Background()

	t.Run("successfully delete API key", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		existingKey := &UserAPIKey{ID: "key-1", UserID: "user-123"}
		user := &User{ID: "user-123"}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(existingKey, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("DeleteAPIKey", ctx, "key-1").Return(nil)

		err := service.DeleteAPIKey(ctx, "user-123", "key-1")

		require.NoError(t, err)
		mockStore.AssertExpectations(t)
	})

	t.Run("return error when delete fails", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(nil, assert.AnError)

		err := service.DeleteAPIKey(ctx, "user-123", "key-1")

		assert.Error(t, err)
		mockStore.AssertExpectations(t)
	})

	t.Run("non-admin user cannot delete another user's API key", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		existingKey := &UserAPIKey{ID: "key-1", UserID: "user-456"}
		user := &User{ID: "user-123"}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(existingKey, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		err := service.DeleteAPIKey(ctx, "user-123", "key-1")

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unauthorized")
		mockStore.AssertExpectations(t)
	})

	t.Run("admin user can delete another user's API key", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		existingKey := &UserAPIKey{ID: "key-1", UserID: "user-456"}
		// Admin == member of the Administrators group ({admin, *}).
		user := &User{ID: "user-123", GroupIDs: []string{DefaultAdminGroupID}}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(existingKey, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
			ID:          DefaultAdminGroupID,
			Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
		}, nil)
		mockStore.On("DeleteAPIKey", ctx, "key-1").Return(nil)

		err := service.DeleteAPIKey(ctx, "user-123", "key-1")

		require.NoError(t, err)
		mockStore.AssertExpectations(t)
	})

	t.Run("return error when GetUserByID fails", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		existingKey := &UserAPIKey{ID: "key-1", UserID: "user-123"}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(existingKey, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(nil, assert.AnError)

		err := service.DeleteAPIKey(ctx, "user-123", "key-1")

		assert.Error(t, err)
		mockStore.AssertExpectations(t)
	})

	t.Run("return error when DeleteAPIKey store operation fails", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		existingKey := &UserAPIKey{ID: "key-1", UserID: "user-123"}
		user := &User{ID: "user-123"}

		mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(existingKey, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("DeleteAPIKey", ctx, "key-1").Return(assert.AnError)

		err := service.DeleteAPIKey(ctx, "user-123", "key-1")

		assert.Error(t, err)
		mockStore.AssertExpectations(t)
	})
}

// validateBooksNoUsage is the "successfully validate API key" case. It must
// run inside a synctest bubble.
func validateBooksNoUsage(ctx context.Context, t *testing.T) {
	mockStore := new(MockStore)
	service := &Service{store: mockStore}

	apiKey := "test-api-key-123456"
	hash := sha256.Sum256([]byte(apiKey))
	keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

	user := &User{
		ID:     "user-123",
		Email:  "test@example.com",
		Active: true,
	}

	apiKeyRecord := &UserAPIKey{
		ID:          "key-1",
		UserID:      "user-123",
		Name:        "Test Key",
		KeyHash:     keyHash,
		IsActive:    true,
		ExpiresAt:   nil,
		Permissions: []Permission{{Action: ActionView, Resource: ResourceRecommendations}},
	}

	mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(apiKeyRecord, nil)
	mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
	// Validation must book NO usage: one HTTP request validates the same
	// credential several times, so counting here would multiply every
	// reported figure. The API layer books once per request instead --
	// see Handler.validateSecurityContext and the exactly-once tests in
	// internal/server/apikey_usage_booking_test.go.
	//
	// Registered with .Maybe() rather than left unregistered because an
	// unexpected call would panic inside the flush goroutine, where
	// RecordUsageAsync's recover() would swallow it and the test would
	// pass regardless.
	usageRecorded := make(chan struct{}, 1)
	mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", mock.Anything).
		Run(func(mock.Arguments) { usageRecorded <- struct{}{} }).
		Return(nil).Maybe()

	resultKey, resultUser, err := service.ValidateUserAPIKey(ctx, apiKey)

	require.NoError(t, err)
	assert.Equal(t, user, resultUser)
	assert.Equal(t, apiKeyRecord, resultKey)
	// A booking would be flushed by a goroutine started before
	// ValidateUserAPIKey returned. synctest.Wait returns only once that
	// goroutine has finished or is durably blocked, so by then any flush
	// has already reached the store and the non-blocking receive is exact.
	// A fixed quiet window could pass a flusher a loaded runner had not
	// scheduled yet.
	synctest.Wait()
	select {
	case <-usageRecorded:
		t.Fatal("ValidateUserAPIKey must not record usage: booking on the validation path counts validations, not requests")
	default:
	}
	mockStore.AssertExpectations(t)
}

func TestService_ValidateUserAPIKey(t *testing.T) {
	ctx := context.Background()

	t.Run("successfully validate API key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) { validateBooksNoUsage(ctx, t) })
	})

	t.Run("fail when owner's password changed after minting", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		apiKey := "test-api-key-123456"
		hash := sha256.Sum256([]byte(apiKey))
		keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

		mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(&UserAPIKey{
			ID: "key-1", UserID: "user-123", KeyHash: keyHash, IsActive: true, PasswordVersion: 2,
		}, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(&User{ID: "user-123", Active: true, PasswordVersion: 3}, nil)

		resultKey, resultUser, err := service.ValidateUserAPIKey(ctx, apiKey)

		require.ErrorIs(t, err, ErrAPIKeyPasswordRotated)
		assert.Nil(t, resultUser)
		assert.Nil(t, resultKey)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when API key not found", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		apiKey := "test-api-key-123456"
		hash := sha256.Sum256([]byte(apiKey))
		keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

		mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(nil, assert.AnError)

		resultKey, resultUser, err := service.ValidateUserAPIKey(ctx, apiKey)

		assert.Error(t, err)
		assert.Nil(t, resultUser)
		assert.Nil(t, resultKey)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when API key is inactive", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		apiKey := "test-api-key-123456"
		hash := sha256.Sum256([]byte(apiKey))
		keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

		apiKeyRecord := &UserAPIKey{
			ID:       "key-1",
			UserID:   "user-123",
			KeyHash:  keyHash,
			IsActive: false,
		}

		mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(apiKeyRecord, nil)

		resultKey, resultUser, err := service.ValidateUserAPIKey(ctx, apiKey)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "revoked")
		assert.Nil(t, resultUser)
		assert.Nil(t, resultKey)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when API key is expired", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		apiKey := "test-api-key-123456"
		hash := sha256.Sum256([]byte(apiKey))
		keyHash := base64.RawURLEncoding.EncodeToString(hash[:])
		expiredTime := time.Now().Add(-24 * time.Hour)

		apiKeyRecord := &UserAPIKey{
			ID:        "key-1",
			UserID:    "user-123",
			KeyHash:   keyHash,
			IsActive:  true,
			ExpiresAt: &expiredTime,
		}

		mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(apiKeyRecord, nil)

		resultKey, resultUser, err := service.ValidateUserAPIKey(ctx, apiKey)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "expired")
		assert.Nil(t, resultUser)
		assert.Nil(t, resultKey)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when user not found", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		apiKey := "test-api-key-123456"
		hash := sha256.Sum256([]byte(apiKey))
		keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

		apiKeyRecord := &UserAPIKey{
			ID:       "key-1",
			UserID:   "user-123",
			KeyHash:  keyHash,
			IsActive: true,
		}

		mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(apiKeyRecord, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(nil, assert.AnError)

		resultKey, resultUser, err := service.ValidateUserAPIKey(ctx, apiKey)

		assert.Error(t, err)
		assert.Nil(t, resultUser)
		assert.Nil(t, resultKey)
		mockStore.AssertExpectations(t)
	})

	t.Run("fail when user is inactive", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		apiKey := "test-api-key-123456"
		hash := sha256.Sum256([]byte(apiKey))
		keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

		user := &User{
			ID:     "user-123",
			Email:  "test@example.com",
			Active: false,
		}

		apiKeyRecord := &UserAPIKey{
			ID:       "key-1",
			UserID:   "user-123",
			KeyHash:  keyHash,
			IsActive: true,
		}

		mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(apiKeyRecord, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		resultKey, resultUser, err := service.ValidateUserAPIKey(ctx, apiKey)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not active")
		assert.Nil(t, resultUser)
		assert.Nil(t, resultKey)
		mockStore.AssertExpectations(t)
	})

	t.Run("reject when stored hash does not match derived hash", func(t *testing.T) {
		// Simulates a store returning a row whose key_hash belongs to a
		// different key -- the post-fetch store-integrity check must reject it.
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		apiKey := "test-api-key-123456"
		hash := sha256.Sum256([]byte(apiKey))
		keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

		tamperedRecord := &UserAPIKey{
			ID:       "key-1",
			UserID:   "user-123",
			KeyHash:  keyHash + "tampered",
			IsActive: true,
		}

		mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(tamperedRecord, nil)

		resultKey, resultUser, err := service.ValidateUserAPIKey(ctx, apiKey)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid API key")
		assert.Nil(t, resultUser)
		assert.Nil(t, resultKey)
		mockStore.AssertExpectations(t)
	})
}

func TestService_RecordUsage(t *testing.T) {
	ctx := context.Background()

	t.Run("delegates to store RecordAPIKeyUsage", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		mockStore.On("RecordAPIKeyUsage", ctx, "key-1", mock.Anything).Return(nil)

		err := service.RecordUsage(ctx, "key-1", 1)
		require.NoError(t, err)
		mockStore.AssertExpectations(t)
	})

	t.Run("propagates store error", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}

		mockStore.On("RecordAPIKeyUsage", ctx, "key-1", mock.Anything).Return(assert.AnError)

		err := service.RecordUsage(ctx, "key-1", 1)
		assert.Error(t, err)
		mockStore.AssertExpectations(t)
	})
}

// Regression: concurrent requests for the same key share one flush (first a
// singleflight.Group, now a single flusher per key). Writing a fixed "+1"
// inside that flush silently dropped
// every request that arrived while a write was in flight, undercounting the
// busiest keys -- precisely the keys the usage stats exist to surface. The
// pending count is accumulated first and the flush writes the whole delta.
func TestService_FlushPendingUsage(t *testing.T) {
	t.Run("writes the whole coalesced delta in one call", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := &Service{store: mockStore}

		mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", int64(5)).Return(nil).Once()

		for range 5 {
			service.addPendingUsage("key-1")
		}
		service.flushPendingUsage("key-1")
	})

	t.Run("does not write when nothing is pending", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := &Service{store: mockStore}

		service.flushPendingUsage("never-seen-key")
		mockStore.AssertNotCalled(t, "RecordAPIKeyUsage", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("leaves requests booked during the write for the next round", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := &Service{store: mockStore}

		// A request that lands while the write is in flight must still be
		// pending afterwards -- RecordUsageAsync's loop picks it up.
		mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", int64(2)).
			Run(func(mock.Arguments) { service.addPendingUsage("key-1") }).
			Return(nil).Once()

		service.addPendingUsage("key-1")
		service.addPendingUsage("key-1")
		service.flushPendingUsage("key-1")

		assert.Equal(t, int64(1), service.peekPendingUsage("key-1"))
	})

	t.Run("drops the delta on a store error rather than retrying forever", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := &Service{store: mockStore}

		mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", int64(3)).
			Return(assert.AnError).Once()

		for range 3 {
			service.addPendingUsage("key-1")
		}
		service.flushPendingUsage("key-1")

		assert.Equal(t, int64(0), service.peekPendingUsage("key-1"))
	})
}

// Every request must be booked before the flush goroutine starts, so a
// request is never lost to a flush that had already drained the counter.
func TestService_RecordUsageAsync_CountsConcurrentRequests(t *testing.T) {
	mockStore := new(MockStore)
	service := &Service{store: mockStore}

	const requests = 50
	var recorded atomic.Int64
	done := make(chan struct{})
	mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", mock.Anything).
		Run(func(args mock.Arguments) {
			if recorded.Add(args.Get(2).(int64)) == requests {
				close(done)
			}
		}).
		Return(nil).Maybe()

	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service.RecordUsageAsync("key-1")
		}()
	}
	wg.Wait()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("only %d of %d requests were recorded", recorded.Load(), requests)
	}
	assert.Equal(t, int64(requests), recorded.Load())
}

// Issue #104: RecordUsageAsync spawned a goroutine per request before the
// singleflight dedupe, so N concurrent requests on one key parked N goroutines
// behind a slow write. A request whose key already has a flusher must only
// book its count, and that flusher must still write it.
func TestService_RecordUsageAsync_OneFlusherPerKey(t *testing.T) {
	mockStore := new(MockStore)
	service := &Service{store: mockStore}

	const requests = 200
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	var (
		recorded atomic.Int64
		writes   atomic.Int32
	)
	mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", mock.Anything).
		Run(func(args mock.Arguments) {
			if writes.Add(1) == 1 {
				close(writeStarted)
				<-releaseWrite
			}
			recorded.Add(args.Get(2).(int64))
		}).
		Return(nil)

	service.RecordUsageAsync("key-1")
	select {
	case <-writeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the first flush never reached the store")
	}

	before := runtime.NumGoroutine()
	for range requests {
		service.RecordUsageAsync("key-1")
	}
	spawned := runtime.NumGoroutine() - before
	close(releaseWrite)

	assert.Less(t, spawned, 10, "requests booked during an in-flight flush must not each start a goroutine")
	require.Eventually(t, func() bool { return recorded.Load() == requests+1 }, 5*time.Second, time.Millisecond,
		"recorded %d of %d requests", recorded.Load(), requests+1)
	assert.Equal(t, int32(2), writes.Load(), "the requests booked during the first write are carried by one more write")
	require.Eventually(t, func() bool { return !service.pendingEntry("key-1").flushing.Load() }, 5*time.Second, time.Millisecond,
		"the flusher must release the key once nothing is pending")
}

// A request can book its count just after the flusher's last drain and see
// the slot still taken. Releasing the slot must notice that count and keep
// the flusher going rather than leave it stranded until the key is used
// again.
func TestAPIKeyUsageEntry_ReleaseFlusher(t *testing.T) {
	t.Run("keeps the slot when a count was booked after the drain", func(t *testing.T) {
		var entry apiKeyUsageEntry
		entry.flushing.Store(true)
		entry.count.Add(1) // a request that lost the CompareAndSwap

		assert.True(t, entry.releaseFlusher(), "the late request's count must be written by this flusher")
		assert.True(t, entry.flushing.Load(), "the flusher must hold the slot again")
	})

	t.Run("frees the slot when nothing is pending", func(t *testing.T) {
		var entry apiKeyUsageEntry
		entry.flushing.Store(true)

		assert.False(t, entry.releaseFlusher())
		assert.False(t, entry.flushing.Load(), "the next request must be able to start a flusher")
	})
}

// A flush that panics must release the key's flusher slot; otherwise every
// later request on the key would see a flusher that no longer exists and its
// usage would never be written again.
func TestService_RecordUsageAsync_PanicReleasesFlusher(t *testing.T) {
	mockStore := new(MockStore)
	service := &Service{store: mockStore}

	mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", int64(1)).
		Run(func(mock.Arguments) { panic("store exploded") }).
		Return(nil).Once()
	service.RecordUsageAsync("key-1")
	require.Eventually(t, func() bool { return !service.pendingEntry("key-1").flushing.Load() }, 5*time.Second, time.Millisecond)

	done := make(chan struct{})
	mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", int64(1)).
		Run(func(mock.Arguments) { close(done) }).
		Return(nil).Once()
	service.RecordUsageAsync("key-1")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("usage after a panicked flush was never written")
	}
}

func TestService_ComputeEffectivePermissions(t *testing.T) {
	ctx := context.Background()

	// adminGrp / userGrp are reused across sub-cases. Permissions derive
	// purely from group membership now (issue #907).
	adminGrp := func() *Group {
		return &Group{ID: DefaultAdminGroupID, Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}}}
	}
	userGrpID := "00000000-0000-5000-8000-000000000005"
	userGrp := func() *Group {
		return &Group{ID: userGrpID, Permissions: []Permission{
			{Action: ActionView, Resource: ResourceRecommendations},
			{Action: ActionCreate, Resource: ResourcePlans},
		}}
	}

	t.Run("admin with scoped API key returns key permissions (intersection passes)", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		scoped := []Permission{{Action: ActionView, Resource: ResourceRecommendations}}
		apiKey := &UserAPIKey{ID: "key-1", UserID: "user-123", Permissions: scoped}
		user := &User{ID: "user-123", GroupIDs: []string{DefaultAdminGroupID}}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(adminGrp(), nil)

		permissions, err := service.ComputeEffectivePermissions(ctx, apiKey, user)
		require.NoError(t, err)
		// Admin holds {admin, *} so the scoped key permission passes the
		// intersection and is returned.
		assert.Equal(t, scoped, permissions)
	})

	t.Run("admin with unscoped key returns no permissions (fail closed)", func(t *testing.T) {
		// Regression test for issue #61: an unscoped key used to inherit the
		// owner's full permission set, so an admin's unscoped key was
		// silently admin-capable. It must now authorize nothing.
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		apiKey := &UserAPIKey{ID: "key-1", UserID: "user-123", Permissions: []Permission{}}
		user := &User{ID: "user-123", GroupIDs: []string{DefaultAdminGroupID}}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(adminGrp(), nil)

		permissions, err := service.ComputeEffectivePermissions(ctx, apiKey, user)
		require.NoError(t, err)
		assert.Empty(t, permissions)
	})

	t.Run("zero-group user with unscoped key returns no permissions (fail closed)", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		apiKey := &UserAPIKey{ID: "key-1", UserID: "user-123", Permissions: []Permission{}}
		user := &User{ID: "user-123", GroupIDs: nil}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)

		permissions, err := service.ComputeEffectivePermissions(ctx, apiKey, user)
		require.NoError(t, err)
		assert.Empty(t, permissions)
	})

	t.Run("return intersection of API key and group permissions", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		apiKey := &UserAPIKey{
			ID:     "key-1",
			UserID: "user-123",
			Permissions: []Permission{
				{Action: ActionView, Resource: ResourceRecommendations},
				{Action: ActionCreate, Resource: ResourcePlans},
				{Action: ActionAdmin, Resource: ResourceUsers}, // group lacks this
			},
		}
		user := &User{ID: "user-123", GroupIDs: []string{userGrpID}}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, userGrpID).Return(userGrp(), nil)

		permissions, err := service.ComputeEffectivePermissions(ctx, apiKey, user)
		require.NoError(t, err)
		assert.Len(t, permissions, 2)
		assert.Contains(t, permissions, Permission{Action: ActionView, Resource: ResourceRecommendations})
		assert.Contains(t, permissions, Permission{Action: ActionCreate, Resource: ResourcePlans})
	})

	t.Run("return empty when API key permissions not in group permissions", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		apiKey := &UserAPIKey{
			ID:     "key-1",
			UserID: "user-123",
			Permissions: []Permission{
				{Action: ActionAdmin, Resource: ResourceUsers},
				{Action: ActionUpdate, Resource: ResourceConfig},
			},
		}
		user := &User{ID: "user-123", GroupIDs: []string{userGrpID}}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, userGrpID).Return(userGrp(), nil)

		permissions, err := service.ComputeEffectivePermissions(ctx, apiKey, user)
		require.NoError(t, err)
		assert.Empty(t, permissions)
	})
}

// TestService_HasAPIKeyPermissionAPI is the regression test for issue #1142:
// per-key scoped permissions were persisted and UI-exposed but never enforced
// at request time (ComputeEffectivePermissions had no request-path caller).
// HasAPIKeyPermissionAPI is the request-path enforcement point wired into
// requirePermission.
func TestService_HasAPIKeyPermissionAPI(t *testing.T) {
	ctx := context.Background()

	rawKey := "test-api-key-123456"
	hash := sha256.Sum256([]byte(rawKey))
	keyHash := base64.RawURLEncoding.EncodeToString(hash[:])

	userGrpID := "00000000-0000-5000-8000-000000000005"
	userGrp := func() *Group {
		return &Group{ID: userGrpID, Permissions: []Permission{
			{Action: ActionView, Resource: ResourceRecommendations},
			{Action: ActionCreate, Resource: ResourcePlans},
		}}
	}

	// setup returns a service whose store resolves rawKey to a key scoped to
	// keyPerms, owned by an active user whose group grants view:recommendations
	// and create:plans. This is the exact data shape of a scoped key created
	// through POST /api/auth/api-keys.
	setup := func(t *testing.T, keyPerms []Permission) *Service {
		t.Helper()
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		user := &User{ID: "user-123", Email: "test@example.com", Active: true, GroupIDs: []string{userGrpID}}
		keyRecord := &UserAPIKey{
			ID:          "key-1",
			UserID:      "user-123",
			KeyHash:     keyHash,
			IsActive:    true,
			Permissions: keyPerms,
		}

		mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(keyRecord, nil)
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", mock.Anything).Return(nil).Maybe()
		mockStore.On("GetGroup", ctx, userGrpID).Return(userGrp(), nil)
		return service
	}

	t.Run("scoped key grants its in-scope permission and returns key ID", func(t *testing.T) {
		service := setup(t, []Permission{{Action: ActionView, Resource: ResourceRecommendations}})

		userID, keyID, has, err := service.HasAPIKeyPermissionAPI(ctx, rawKey, ActionView, ResourceRecommendations)
		require.NoError(t, err)
		assert.Equal(t, "user-123", userID)
		// Key ID is returned so callers can pass it to
		// HasAPIKeyPermissionForConstraintsAPI without a redundant DB lookup.
		assert.NotEmpty(t, keyID, "key ID must be non-empty so constraint checks can use it")
		assert.True(t, has)
	})

	t.Run("regression #1142: scoped key cannot exceed its scope even when the user holds the permission", func(t *testing.T) {
		// The key is scoped to view:recommendations only; the owning user's
		// group also grants create:plans. The key must NOT inherit it.
		service := setup(t, []Permission{{Action: ActionView, Resource: ResourceRecommendations}})

		userID, _, has, err := service.HasAPIKeyPermissionAPI(ctx, rawKey, ActionCreate, ResourcePlans)
		require.NoError(t, err)
		assert.Equal(t, "user-123", userID)
		assert.False(t, has, "scoped key must not grant permissions outside its scope")
	})

	t.Run("key permission the user lacks is denied (intersection)", func(t *testing.T) {
		// The key claims admin:users but the user's group never granted it.
		service := setup(t, []Permission{{Action: ActionAdmin, Resource: ResourceUsers}})

		userID, _, has, err := service.HasAPIKeyPermissionAPI(ctx, rawKey, ActionAdmin, ResourceUsers)
		require.NoError(t, err)
		assert.Equal(t, "user-123", userID)
		assert.False(t, has, "key must not grant permissions the owning user does not hold")
	})

	t.Run("regression #61: unscoped key is denied (fail closed), not inherited from the owner", func(t *testing.T) {
		service := setup(t, nil)

		userID, _, has, err := service.HasAPIKeyPermissionAPI(ctx, rawKey, ActionCreate, ResourcePlans)
		require.NoError(t, err)
		assert.Equal(t, "user-123", userID)
		assert.False(t, has, "unscoped key must not inherit the owner's group permissions")
	})

	t.Run("invalid key fails closed with an error", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		mockStore.On("GetAPIKeyByHash", ctx, keyHash).Return(nil, nil)

		userID, keyID, has, err := service.HasAPIKeyPermissionAPI(ctx, rawKey, ActionView, ResourceRecommendations)
		assert.Error(t, err)
		assert.Empty(t, userID)
		assert.Empty(t, keyID)
		assert.False(t, has)
	})
}

// TestService_HasAPIKeyPermissionForConstraintsAPI_OwnerCapEnforced is a
// regression guard for adversarial-review finding C2 (CR finding on #1454):
// a key whose MaxPurchaseAmount exceeds the owner's group limit must NOT be
// allowed to spend more than the owner's group allows.
func TestService_HasAPIKeyPermissionForConstraintsAPI_OwnerCapEnforced(t *testing.T) {
	ctx := context.Background()
	grpID := "buyer-group"
	keyID := "key-99"
	userID := "user-88"

	// Owner's group is capped at $100.
	ownerGroup := &Group{
		ID: grpID,
		Permissions: []Permission{
			{Action: ActionExecute, Resource: ResourcePurchases, Constraints: &PermissionConstraints{MaxPurchaseAmount: 100}},
		},
	}
	owner := &User{ID: userID, Email: "buyer@example.com", Active: true, GroupIDs: []string{grpID}}
	// Key is scoped to execute:purchases but with a much higher cap of $1000.
	key := &UserAPIKey{
		ID:     keyID,
		UserID: userID,
		Permissions: []Permission{
			{Action: ActionExecute, Resource: ResourcePurchases, Constraints: &PermissionConstraints{MaxPurchaseAmount: 1000}},
		},
		IsActive: true,
	}

	buildStore := func(t *testing.T) *MockStore {
		t.Helper()
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		mockStore.On("GetAPIKeyByID", ctx, keyID).Return(key, nil)
		// loadActiveUser + GetAuthContext both call GetUserByID.
		mockStore.On("GetUserByID", ctx, userID).Return(owner, nil)
		mockStore.On("GetGroup", ctx, grpID).Return(ownerGroup, nil)
		return mockStore
	}

	t.Run("key cap $1000 cannot exceed owner cap $100 (regression C2)", func(t *testing.T) {
		service := &Service{store: buildStore(t)}
		// Request amount $500: within key cap but over owner cap.
		constraintSets := []PermissionConstraints{{MaxPurchaseAmount: 500}}
		ok, err := service.HasAPIKeyPermissionForConstraintsAPI(ctx, keyID, userID, ActionExecute, ResourcePurchases, constraintSets)
		require.NoError(t, err)
		assert.False(t, ok, "request exceeding owner group cap must be denied even when within key cap")
	})

	t.Run("request within both key and owner cap is allowed", func(t *testing.T) {
		service := &Service{store: buildStore(t)}
		// Request amount $50: within both key cap ($1000) and owner cap ($100).
		constraintSets := []PermissionConstraints{{MaxPurchaseAmount: 50}}
		ok, err := service.HasAPIKeyPermissionForConstraintsAPI(ctx, keyID, userID, ActionExecute, ResourcePurchases, constraintSets)
		require.NoError(t, err)
		assert.True(t, ok, "request within both caps must be allowed")
	})
}

// TestRecordUsageAsync_PanicIsRecovered proves that a panic inside the
// fire-and-forget RecordUsage goroutine is caught by the recover() added in
// issue #672. Without the recover(), the panic would crash the entire test
// binary (an unrecovered goroutine panic terminates the process). The test
// uses a channel -- closed by the mock Run callback just before panicking --
// to wait for goroutine execution without time.Sleep.
//
// Driven through RecordUsageAsync directly rather than through
// ValidateUserAPIKey, which no longer books usage (booking moved to the API
// layer so it happens once per request rather than once per validation).
func TestRecordUsageAsync_PanicIsRecovered(t *testing.T) {
	mockStore := new(MockStore)
	service := &Service{store: mockStore}
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	// done is closed by the Run callback immediately before panicking so the
	// test can wait for goroutine execution without time.Sleep.
	done := make(chan struct{})
	mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-panic", mock.Anything).
		Run(func(args mock.Arguments) {
			close(done)
			panic("injected panic for #672 regression test")
		}).
		Return(nil)

	service.RecordUsageAsync("key-panic")

	// Block until the goroutine has executed (and panicked + recovered).
	// Reaching this line means the process survived -- recover() caught the panic.
	<-done
}

func TestService_validateAPIKeyPermissions(t *testing.T) {
	ctx := context.Background()

	t.Run("admin user can create keys with any permissions", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		user := &User{ID: "user-123", GroupIDs: []string{DefaultAdminGroupID}}
		permissions := []Permission{
			{Action: ActionAdmin, Resource: ResourceUsers},
			{Action: ActionUpdate, Resource: ResourceConfig},
		}

		// validateAPIKeyPermissions resolves the user's group permissions; the
		// admin group's {admin, *} satisfies any requested permission.
		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, DefaultAdminGroupID).Return(&Group{
			ID:          DefaultAdminGroupID,
			Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll}},
		}, nil)

		err := service.validateAPIKeyPermissions(ctx, user, permissions)
		require.NoError(t, err)
	})

	t.Run("non-admin user can create keys with their group permissions", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		grpID := "viewer-group"
		user := &User{ID: "user-123", GroupIDs: []string{grpID}}
		permissions := []Permission{{Action: ActionView, Resource: ResourceRecommendations}}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, grpID).Return(&Group{
			ID:          grpID,
			Permissions: []Permission{{Action: ActionView, Resource: ResourceRecommendations}},
		}, nil)

		err := service.validateAPIKeyPermissions(ctx, user, permissions)
		require.NoError(t, err)
	})

	t.Run("fail when user requests permissions their groups don't grant", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		grpID := "viewer-group"
		user := &User{ID: "user-123", GroupIDs: []string{grpID}}
		permissions := []Permission{{Action: ActionAdmin, Resource: ResourceUsers}}

		mockStore.On("GetUserByID", ctx, "user-123").Return(user, nil)
		mockStore.On("GetGroup", ctx, grpID).Return(&Group{
			ID:          grpID,
			Permissions: []Permission{{Action: ActionView, Resource: ResourceRecommendations}},
		}, nil)

		err := service.validateAPIKeyPermissions(ctx, user, permissions)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user does not have permission")
	})

	t.Run("fail when GetAuthContext fails", func(t *testing.T) {
		mockStore := new(MockStore)
		service := &Service{store: mockStore}
		t.Cleanup(func() { mockStore.AssertExpectations(t) })

		user := &User{ID: "user-123", GroupIDs: []string{"g"}}
		permissions := []Permission{{Action: ActionView, Resource: ResourceRecommendations}}

		mockStore.On("GetUserByID", ctx, "user-123").Return(nil, assert.AnError)

		err := service.validateAPIKeyPermissions(ctx, user, permissions)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get user permissions")
	})
}
