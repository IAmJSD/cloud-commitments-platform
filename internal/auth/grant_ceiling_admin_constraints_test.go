package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGrantCeilingAllows_HeldAdminConstraints(t *testing.T) {
	scoped := func(accounts ...string) *PermissionConstraints {
		return &PermissionConstraints{AccountIDs: accounts}
	}
	admin := func(c *PermissionConstraints) Permission {
		return Permission{Action: ActionAdmin, Resource: ResourceAll, Constraints: c}
	}
	view := func(c *PermissionConstraints) Permission {
		return Permission{Action: ActionView, Resource: ResourcePlans, Constraints: c}
	}

	cases := []struct {
		name string
		held Permission
		req  Permission
		want bool
	}{
		{"constrained admin, unconstrained admin", admin(scoped("a")), admin(nil), false},
		{"constrained admin, unconstrained other", admin(scoped("a")), view(nil), false},
		{"constrained admin, wider scope", admin(scoped("a")), view(scoped("a", "b")), false},
		{"constrained admin, different scope", admin(scoped("a")), view(scoped("b")), false},
		{"constrained admin, cap removed", admin(&PermissionConstraints{MaxPurchaseAmount: 10}), view(nil), false},
		{"constrained admin, within scope", admin(scoped("a", "b")), view(scoped("a")), true},
		{"constrained admin, same scope admin", admin(scoped("a")), admin(scoped("a")), true},
		{"services admin, wider services", admin(&PermissionConstraints{Services: []string{"ec2"}}), view(&PermissionConstraints{Services: []string{"ec2", "rds"}}), false},
		{"services admin, no services", admin(&PermissionConstraints{Services: []string{"ec2"}}), view(nil), false},
		{"services admin, subset services", admin(&PermissionConstraints{Services: []string{"ec2", "rds"}}), view(&PermissionConstraints{Services: []string{"ec2"}}), true},
		{"regions admin, wider regions", admin(&PermissionConstraints{Regions: []string{"us-east-1"}}), view(&PermissionConstraints{Regions: []string{"us-east-1", "eu-west-1"}}), false},
		{"regions admin, no regions", admin(&PermissionConstraints{Regions: []string{"us-east-1"}}), view(nil), false},
		{"regions admin, subset regions", admin(&PermissionConstraints{Regions: []string{"us-east-1", "eu-west-1"}}), view(&PermissionConstraints{Regions: []string{"us-east-1"}}), true},
		{"unconstrained admin, unconstrained admin", admin(nil), admin(nil), true},
		{"unconstrained admin, other", admin(nil), view(scoped("a")), true},
		{"empty constraints admin, other", admin(&PermissionConstraints{}), view(nil), true},
		{"unconstrained admin, carved-out", admin(nil), Permission{Action: ActionExecute, Resource: ResourcePurchases}, false},
		{"constrained admin, carved-out within scope", admin(scoped("a")), Permission{Action: ActionExecute, Resource: ResourcePurchases, Constraints: scoped("a")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, grantCeilingAllows([]Permission{tc.held}, tc.req))
		})
	}
}
