package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #956: an {admin, *} permission that carries Constraints must be
// narrowed by them on every check, not only under StrictScope. Before the
// fix, permissionsAllow applied an admin permission's Constraints only when
// the request set StrictScope, so a key created as {admin, *} with
// accounts=[A] or a max_amount passed every non-strict check for account B
// or for any amount.

func constrainedAdmin(c *PermissionConstraints) Permission {
	return Permission{Action: ActionAdmin, Resource: ResourceAll, Constraints: c}
}

func TestPermissionsAllow_ConstrainedAdminNonStrict(t *testing.T) {
	t.Parallel()
	held := constrainedAdmin(&PermissionConstraints{
		AccountIDs: []string{"acct-a"}, Providers: []string{"aws"}, Services: []string{"ec2"},
		Regions: []string{"us-east-1"}, MaxPurchaseAmount: 500,
	})
	cases := []struct {
		name string
		req  PermissionConstraints
		want bool
	}{
		{"other account denied", PermissionConstraints{AccountIDs: []string{"acct-b"}}, false},
		{"own account allowed", PermissionConstraints{AccountIDs: []string{"acct-a"}}, true},
		{"amount over cap denied", PermissionConstraints{MaxPurchaseAmount: 501}, false},
		{"amount at cap allowed", PermissionConstraints{MaxPurchaseAmount: 500}, true},
		{"other provider denied", PermissionConstraints{Providers: []string{"azure"}}, false},
		{"other service denied", PermissionConstraints{Services: []string{"rds"}}, false},
		{"other region denied", PermissionConstraints{Regions: []string{"eu-west-1"}}, false},
		{"full in-scope request allowed", PermissionConstraints{
			AccountIDs: []string{"acct-a"}, Providers: []string{"aws"}, Services: []string{"ec2"},
			Regions: []string{"us-east-1"}, MaxPurchaseAmount: 100,
		}, true},
		// The empty probe sessionHoldsVerb sends must still report the verb
		// as held: an empty request dimension is "not specified".
		{"empty holds-verb probe allowed", PermissionConstraints{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := PermissionsAllowForConstraintSets([]Permission{held}, ActionExecuteAny, ResourcePurchases, []PermissionConstraints{tc.req})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPermissionsAllow_AdminWithoutConstraintsUnchanged(t *testing.T) {
	t.Parallel()
	admin := []Permission{{Action: ActionAdmin, Resource: ResourceAll}}
	req := &PermissionConstraints{AccountIDs: []string{"acct-b"}, MaxPurchaseAmount: 1e6}
	assert.True(t, permissionsAllow(admin, ActionExecuteAny, ResourcePurchases, req))
	assert.True(t, permissionsAllow(admin, ActionUpdate, ResourceConfig, nil))
	// Carve-outs still apply to an admin permission, constrained or not.
	assert.False(t, permissionsAllow(admin, ActionExecute, ResourcePurchases, req))
	constrained := []Permission{constrainedAdmin(&PermissionConstraints{AccountIDs: []string{"acct-a"}})}
	assert.False(t, permissionsAllow(constrained, ActionExecute, ResourcePurchases, &PermissionConstraints{AccountIDs: []string{"acct-a"}}))
}

// A constrained admin grant is narrowed even when another grant in the set
// matches the action: the decision is per permission, and an in-scope
// explicit grant still allows.
func TestPermissionsAllow_ConstrainedAdminAlongsideExplicitGrant(t *testing.T) {
	t.Parallel()
	perms := []Permission{
		constrainedAdmin(&PermissionConstraints{AccountIDs: []string{"acct-a"}}),
		{Action: ActionUpdate, Resource: ResourceConfig, Constraints: &PermissionConstraints{AccountIDs: []string{"acct-c"}}},
	}
	assert.False(t, permissionsAllow(perms, ActionUpdate, ResourceConfig, &PermissionConstraints{AccountIDs: []string{"acct-b"}}))
	assert.True(t, permissionsAllow(perms, ActionUpdate, ResourceConfig, &PermissionConstraints{AccountIDs: []string{"acct-a"}}))
	assert.True(t, permissionsAllow(perms, ActionUpdate, ResourceConfig, &PermissionConstraints{AccountIDs: []string{"acct-c"}}))
}

// End to end through the service: an Administrators-group owner holds an
// unconstrained {admin, *}, so the owner gate passes everything; only the
// key's own constraints can refuse account B or an amount over the cap.
func TestHasAPIKeyPermissionForConstraintsAPI_ConstrainedAdminKey(t *testing.T) {
	key := effKey(constrainedAdmin(&PermissionConstraints{AccountIDs: []string{"acct-a"}, MaxPurchaseAmount: 500}))
	ownerPerms := []Permission{{Action: ActionAdmin, Resource: ResourceAll}}
	cases := []struct {
		name string
		req  PermissionConstraints
		want bool
	}{
		{"account B refused", PermissionConstraints{AccountIDs: []string{"acct-b"}, MaxPurchaseAmount: 100}, false},
		{"amount over key cap refused", PermissionConstraints{AccountIDs: []string{"acct-a"}, MaxPurchaseAmount: 1000}, false},
		{"in scope allowed", PermissionConstraints{AccountIDs: []string{"acct-a"}, MaxPurchaseAmount: 100}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{store: effStore(t, key, effOwner(), ownerPerms)}
			got, err := s.HasAPIKeyPermissionForConstraintsAPI(context.Background(), effKeyID, effOwnerID,
				ActionExecuteAny, ResourcePurchases, []PermissionConstraints{tc.req})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The same rule holds for a group-granted constrained admin permission on the
// session path (HasPermissionForConstraintsAPI).
func TestHasPermissionForConstraintsAPI_ConstrainedAdminGroup(t *testing.T) {
	groupPerms := []Permission{constrainedAdmin(&PermissionConstraints{AccountIDs: []string{"acct-a"}})}
	s := &Service{store: effStore(t, nil, effOwner(), groupPerms)}
	ctx := context.Background()

	got, err := s.HasPermissionForConstraintsAPI(ctx, effOwnerID, ActionUpdate, ResourceConfig,
		[]PermissionConstraints{{AccountIDs: []string{"acct-b"}}})
	require.NoError(t, err)
	assert.False(t, got, "a group's {admin, *} constrained to acct-a must not authorize acct-b")

	got, err = s.HasPermissionForConstraintsAPI(ctx, effOwnerID, ActionUpdate, ResourceConfig,
		[]PermissionConstraints{{AccountIDs: []string{"acct-a"}}})
	require.NoError(t, err)
	assert.True(t, got)
}
