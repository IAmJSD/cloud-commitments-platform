package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// constraintDimension sets one list dimension on a PermissionConstraints, so
// every case below runs once per dimension that matchStringListConstraints
// enforces.
type constraintDimension struct {
	name string
	set  func(c *PermissionConstraints, values []string)
}

func stringListDimensions() []constraintDimension {
	return []constraintDimension{
		{"AccountIDs", func(c *PermissionConstraints, v []string) { c.AccountIDs = v }},
		{"Providers", func(c *PermissionConstraints, v []string) { c.Providers = v }},
		{"Services", func(c *PermissionConstraints, v []string) { c.Services = v }},
	}
}

// TestMatchConstraints_StringListsRequireEveryRequestedValue pins the
// ALL-match rule on the AccountIDs, Providers and Services dimensions
// (issue #138).
//
// They used to be ANY-match (containsAny): a permission scoped to value A
// matched a request naming A and B, so the first caller to submit a
// multi-value request on one of these dimensions would have authorized an
// operation spanning B under a permission that never mentioned it. That is
// the bypass the Regions dimension already closed with
// matchAllRegionsConstraint. Every case is asserted through matchConstraints
// on a non-strict request, so wiring a dimension back to an any-match helper
// fails here rather than in a helper-only test.
func TestMatchConstraints_StringListsRequireEveryRequestedValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		perm []string
		req  []string
		want bool
	}{
		{"partially permitted multi-value request is denied", []string{"a"}, []string{"a", "b"}, false},
		{"permitted value listed second does not rescue the request", []string{"b"}, []string{"a", "b"}, false},
		{"every requested value permitted", []string{"a", "b", "c"}, []string{"a", "b"}, true},
		{"duplicated permitted value", []string{"a"}, []string{"a", "a"}, true},
		{"single permitted value", []string{"a", "b"}, []string{"b"}, true},
		{"single unpermitted value is denied", []string{"a"}, []string{"b"}, false},
		{"comparison stays exact: casing does not match", []string{"A"}, []string{"a"}, false},
		{"comparison stays exact: whitespace does not match", []string{" a"}, []string{"a"}, false},
		{"blank request value is not permitted by a constrained permission", []string{"a"}, []string{"a", ""}, false},
		{"unconstrained permission matches a multi-value request", nil, []string{"a", "b"}, true},
		{"request not naming the dimension still matches (unchanged)", []string{"a"}, nil, true},
	}
	for _, dim := range stringListDimensions() {
		for _, tt := range tests {
			t.Run(dim.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				perm, req := &PermissionConstraints{}, &PermissionConstraints{}
				dim.set(perm, tt.perm)
				dim.set(req, tt.req)
				assert.Equal(t, tt.want, matchConstraints(perm, req))
			})
		}
	}
}

// TestPermissionsAllow_MultiAccountRequestDeniedOutsideScope drives the
// all-match rule through the exported decision function the API handlers and
// MockAuthService share, so the fix is proven at the entry point and not only
// at the matcher. The scenario is the one issue #138 names: a permission
// scoped to account A must not authorize an operation spanning A and B.
func TestPermissionsAllow_MultiAccountRequestDeniedOutsideScope(t *testing.T) {
	t.Parallel()
	perms := []Permission{{
		Action:   ActionExecute,
		Resource: ResourcePurchases,
		Constraints: &PermissionConstraints{
			AccountIDs: []string{"acct-a"},
			Providers:  []string{"aws"},
			Services:   []string{"ec2"},
		},
	}}

	allowed := PermissionConstraints{
		AccountIDs: []string{"acct-a"}, Providers: []string{"aws"}, Services: []string{"ec2"},
	}
	ok, err := PermissionsAllowForConstraintSets(perms, ActionExecute, ResourcePurchases, []PermissionConstraints{allowed})
	require.NoError(t, err)
	assert.True(t, ok, "the permitted single-value request must still be allowed")

	for name, widen := range map[string]func(c *PermissionConstraints){
		"AccountIDs": func(c *PermissionConstraints) { c.AccountIDs = []string{"acct-a", "acct-b"} },
		"Providers":  func(c *PermissionConstraints) { c.Providers = []string{"aws", "azure"} },
		"Services":   func(c *PermissionConstraints) { c.Services = []string{"ec2", "rds"} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := allowed
			widen(&req)
			ok, err := PermissionsAllowForConstraintSets(perms, ActionExecute, ResourcePurchases, []PermissionConstraints{req})
			require.NoError(t, err)
			assert.False(t, ok, "a request naming a value outside the permission's %s must be denied", name)
		})
	}
}
