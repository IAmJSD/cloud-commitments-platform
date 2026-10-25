package server

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #956: a key created through the API as {admin, *} with accounts=[A]
// and a max_amount keeps those constraints on create (#957), and they must
// also be enforced on the non-strict constraint check the API layer runs for
// user-API-key sessions (requirePermissionConstraints ->
// HasAPIKeyPermissionForConstraintsAPI). The owner is an Administrators
// member with an unconstrained {admin, *}, so the owner gate passes
// everything and only the key's own constraints can refuse the request.
func TestAuthServiceAdapter_ConstrainedAdminKeyRefusedOutOfScope(t *testing.T) {
	adapter, mockStore := createMockAuthService(t)
	ctx := context.Background()
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	const ownerPass = "CorrectHorse@123" // throwaway fixture credential
	owner := &auth.User{
		ID: "user-1", Email: "test@example.com", Active: true,
		PasswordHash: auth.TestPasswordHash(t, ownerPass),
		GroupIDs:     []string{auth.DefaultAdminGroupID},
	}
	mockStore.On("GetUserByID", ctx, "user-1").Return(owner, nil)
	mockStore.On("GetGroup", ctx, auth.DefaultAdminGroupID).Return(&auth.Group{
		ID:          auth.DefaultAdminGroupID,
		Permissions: []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}},
	}, nil)
	var stored *auth.UserAPIKey
	mockStore.On("CreateAPIKey", ctx, mock.AnythingOfType("*auth.UserAPIKey")).
		Run(func(args mock.Arguments) { stored = args.Get(1).(*auth.UserAPIKey) }).Return(nil)

	expiresAt := time.Now().Add(24 * time.Hour)
	_, err := adapter.CreateAPIKeyAPI(ctx, "user-1", auth.APICreateAPIKeyRequest{
		Name:      "admin-scoped",
		Password:  ownerPass,
		ExpiresAt: &expiresAt,
		Permissions: []auth.APIPermission{{
			Action: auth.ActionAdmin, Resource: auth.ResourceAll,
			Constraints: &auth.APIPermissionConstraint{Accounts: []string{"acct-a"}, MaxAmount: 500},
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, stored)
	mockStore.On("GetAPIKeyByID", ctx, stored.ID).Return(stored, nil)

	cases := []struct {
		name string
		req  auth.PermissionConstraints
		want bool
	}{
		{"account B refused", auth.PermissionConstraints{AccountIDs: []string{"acct-b"}, MaxPurchaseAmount: 100}, false},
		{"amount over key cap refused", auth.PermissionConstraints{AccountIDs: []string{"acct-a"}, MaxPurchaseAmount: 501}, false},
		{"in scope allowed", auth.PermissionConstraints{AccountIDs: []string{"acct-a"}, MaxPurchaseAmount: 500}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := adapter.HasAPIKeyPermissionForConstraintsAPI(ctx, stored.ID, "user-1",
				auth.ActionExecuteAny, auth.ResourcePurchases, []auth.PermissionConstraints{tc.req})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
