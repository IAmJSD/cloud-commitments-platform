package auth

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #312 (audit A02-012): a missing user, group or API key reached the
// API layer as the store's raw pgx.ErrNoRows or as an untyped error, so the
// handlers could not tell it from a database failure and answered 500. The
// service now returns exported sentinels the handlers map to 404.

func TestService_GetUser_MissingUserIsErrUserNotFound(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	service := createTestService(mockStore, new(MockEmailSender))
	mockStore.On("GetUserByID", ctx, "missing").Return(nil, pgx.ErrNoRows).Once()

	user, err := service.GetUser(ctx, "missing")

	require.Error(t, err)
	assert.Nil(t, user)
	assert.ErrorIs(t, err, ErrUserNotFound)
	// Existing callers that test for the store's sentinel keep working.
	assert.ErrorIs(t, err, pgx.ErrNoRows)
}

func TestService_GetGroupAPI_MissingGroupIsErrGroupNotFound(t *testing.T) {
	ctx := context.Background()

	t.Run("store returns ErrNoRows", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := createTestService(mockStore, new(MockEmailSender))
		mockStore.On("GetGroup", ctx, "missing").Return(nil, pgx.ErrNoRows).Once()

		_, err := service.GetGroupAPI(ctx, "missing")

		assert.ErrorIs(t, err, ErrGroupNotFound)
		assert.ErrorIs(t, err, pgx.ErrNoRows)
	})

	t.Run("store returns nil group", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		service := createTestService(mockStore, new(MockEmailSender))
		mockStore.On("GetGroup", ctx, "missing").Return(nil, nil).Once()

		_, err := service.GetGroupAPI(ctx, "missing")

		assert.ErrorIs(t, err, ErrGroupNotFound)
	})
}

func TestService_APIKeyAccess_NotFoundAndNotOwnedSentinels(t *testing.T) {
	ctx := context.Background()
	actions := map[string]func(*Service) error{
		"revoke": func(s *Service) error { return s.RevokeAPIKeyAPI(ctx, "user-123", "key-1") },
		"delete": func(s *Service) error { return s.DeleteAPIKeyAPI(ctx, "user-123", "key-1") },
	}

	for name, act := range actions {
		t.Run(name+" missing key", func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(nil, pgx.ErrNoRows).Once()

			err := act(&Service{store: mockStore})

			assert.ErrorIs(t, err, ErrAPIKeyNotFound)
		})

		t.Run(name+" nil key", func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			mockStore.On("GetAPIKeyByID", ctx, "key-1").Return(nil, nil).Once()

			err := act(&Service{store: mockStore})

			assert.ErrorIs(t, err, ErrAPIKeyNotFound)
		})

		t.Run(name+" another user's key", func(t *testing.T) {
			mockStore := new(MockStore)
			t.Cleanup(func() { mockStore.AssertExpectations(t) })
			mockStore.On("GetAPIKeyByID", ctx, "key-1").
				Return(&UserAPIKey{ID: "key-1", UserID: "user-456", IsActive: true}, nil)
			mockStore.On("GetUserByID", ctx, "user-123").Return(&User{ID: "user-123"}, nil)

			err := act(&Service{store: mockStore})

			assert.ErrorIs(t, err, ErrAPIKeyNotOwned)
			assert.NotErrorIs(t, err, ErrAPIKeyNotFound)
			// The service-level message is unchanged.
			assert.Contains(t, err.Error(), "unauthorized: cannot "+name+" another user's API key")
			mockStore.AssertNotCalled(t, "UpdateAPIKey", mock.Anything, mock.Anything)
			mockStore.AssertNotCalled(t, "DeleteAPIKey", mock.Anything, mock.Anything)
		})
	}
}
