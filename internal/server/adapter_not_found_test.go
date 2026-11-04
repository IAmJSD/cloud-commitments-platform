package server

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
)

// Issue #312 (audit A02-012): GET /api/users/{id} answers 404 only if the
// adapter hands the handler auth.ErrUserNotFound for a missing user, whether
// the store reports it as pgx.ErrNoRows or as a nil user.
func TestAuthServiceAdapter_GetUser_MissingUserIsErrUserNotFound(t *testing.T) {
	ctx := context.Background()

	t.Run("store returns ErrNoRows", func(t *testing.T) {
		adapter, mockStore := createMockAuthService(t)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		mockStore.On("GetUserByID", ctx, "missing").Return(nil, pgx.ErrNoRows)

		_, err := adapter.GetUser(ctx, "missing")

		assert.ErrorIs(t, err, auth.ErrUserNotFound)
	})

	t.Run("store returns nil user", func(t *testing.T) {
		adapter, mockStore := createMockAuthService(t)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		mockStore.On("GetUserByID", ctx, "missing").Return(nil, nil)

		_, err := adapter.GetUser(ctx, "missing")

		assert.ErrorIs(t, err, auth.ErrUserNotFound)
	})
}
