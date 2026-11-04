package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func constrainedAdminService(t *testing.T, c *PermissionConstraints) *Service {
	t.Helper()
	mockStore := new(MockStore)
	svc := createTestService(mockStore, new(MockEmailSender))
	ctx := context.Background()
	mockStore.On("GetUserByID", ctx, "u1").Return(&User{ID: "u1", GroupIDs: []string{"g1"}}, nil)
	mockStore.On("GetGroup", ctx, "g1").Return(&Group{
		ID:          "g1",
		Permissions: []Permission{{Action: ActionAdmin, Resource: ResourceAll, Constraints: c}},
	}, nil)
	return svc
}

func TestAdminCapability_RequiresUnconstrainedGrant(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		c    *PermissionConstraints
		want bool
	}{
		{"nil constraints", nil, true},
		{"empty constraints", &PermissionConstraints{}, true},
		{"account scoped", &PermissionConstraints{AccountIDs: []string{"a"}}, false},
		{"provider scoped", &PermissionConstraints{Providers: []string{"aws"}}, false},
		{"service scoped", &PermissionConstraints{Services: []string{"ec2"}}, false},
		{"region scoped", &PermissionConstraints{Regions: []string{"us-east-1"}}, false},
		{"amount capped", &PermissionConstraints{MaxPurchaseAmount: 10}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := constrainedAdminService(t, tc.c)
			has, err := svc.HasPermissionAPI(ctx, "u1", ActionAdmin, ResourceAll)
			require.NoError(t, err)
			assert.Equal(t, tc.want, has, "HasPermissionAPI(admin,*)")

			isAdmin, err := svc.UserHasAdminCapability(ctx, "u1")
			require.NoError(t, err)
			assert.Equal(t, tc.want, isAdmin, "UserHasAdminCapability")
		})
	}
}

func TestAdminCapability_ConstrainedGrantKeepsScopedMeaning(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *PermissionConstraints
	}{
		{"account scoped", &PermissionConstraints{AccountIDs: []string{"a"}}},
		{"amount capped", &PermissionConstraints{MaxPurchaseAmount: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := constrainedAdminService(t, tc.c)
			for _, p := range []Permission{
				{Action: ActionView, Resource: ResourcePlans},
				{Action: ActionUpdate, Resource: ResourceConfig},
				{Action: ActionUpdate, Resource: ResourcePlans},
				{Action: ActionView, Resource: ResourcePurchases},
			} {
				has, err := svc.HasPermissionAPI(context.Background(), "u1", p.Action, p.Resource)
				require.NoError(t, err)
				assert.True(t, has, "%s:%s", p.Action, p.Resource)
			}
		})
	}
}
