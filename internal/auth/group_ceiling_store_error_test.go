package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Store failures while resolving the acting user must keep the fail-closed
// refusal but not carry the store's connection detail in its text.

const ceilingStoreFailure = "failed to connect to `user=app_user database=app`: 10.1.2.3:5432 (10.1.2.3): dial error"

var ceilingStoreFailureNeedles = []string{"10.1.2.3", "app_user", "dial"}

func assertCeilingRefusalWithoutStoreDetail(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionCeiling)
	for _, needle := range ceilingStoreFailureNeedles {
		assert.NotContains(t, err.Error(), needle)
	}
}

func TestGrantCeilingPermissions_StoreErrorOmitsDetail(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	mockStore.On("GetUserByID", ctx, ceilingActorID).Return(nil, errors.New(ceilingStoreFailure))
	s := newCeilingService(t, mockStore)

	_, err := s.grantCeilingPermissions(ctx, ceilingActorID)

	assertCeilingRefusalWithoutStoreDetail(t, err)
}

func TestGrantCeilingAccounts_StoreErrorOmitsDetail(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	mockStore.On("GetUserByID", ctx, ceilingActorID).Return(nil, errors.New(ceilingStoreFailure))
	s := newCeilingService(t, mockStore)

	_, err := s.grantCeilingAccounts(ctx, ceilingActorID)

	assertCeilingRefusalWithoutStoreDetail(t, err)
}
