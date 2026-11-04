package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func purchaseHeld(c *PermissionConstraints) []Permission {
	return []Permission{
		{Action: ActionUpdate, Resource: ResourceUsers},
		{Action: ActionExecute, Resource: ResourcePurchases, Constraints: c},
	}
}

var membershipConstraintCases = []struct {
	name   string
	held   *PermissionConstraints
	target *PermissionConstraints
	ok     bool
}{
	{"capped holder, uncapped group", &PermissionConstraints{MaxPurchaseAmount: 100}, nil, false},
	{"capped holder, higher cap group", &PermissionConstraints{MaxPurchaseAmount: 100}, &PermissionConstraints{MaxPurchaseAmount: 500}, false},
	{"capped holder, uncapped group with scope", &PermissionConstraints{MaxPurchaseAmount: 100}, &PermissionConstraints{AccountIDs: []string{"a"}}, false},
	{"capped holder, lower cap group", &PermissionConstraints{MaxPurchaseAmount: 100}, &PermissionConstraints{MaxPurchaseAmount: 50}, true},
	{"capped holder, equal cap group", &PermissionConstraints{MaxPurchaseAmount: 100}, &PermissionConstraints{MaxPurchaseAmount: 100}, true},
	{"account holder, unconstrained group", &PermissionConstraints{AccountIDs: []string{"a"}}, nil, false},
	{"account holder, wider accounts group", &PermissionConstraints{AccountIDs: []string{"a"}}, &PermissionConstraints{AccountIDs: []string{"a", "b"}}, false},
	{"account holder, other account group", &PermissionConstraints{AccountIDs: []string{"a"}}, &PermissionConstraints{AccountIDs: []string{"b"}}, false},
	{"account holder, subset group", &PermissionConstraints{AccountIDs: []string{"a", "b"}}, &PermissionConstraints{AccountIDs: []string{"a"}}, true},
	{"services holder, wider services group", &PermissionConstraints{Services: []string{"ec2"}}, &PermissionConstraints{Services: []string{"ec2", "rds"}}, false},
	{"services holder, other services group", &PermissionConstraints{Services: []string{"ec2"}}, &PermissionConstraints{Services: []string{"other"}}, false},
	{"services holder, no services group", &PermissionConstraints{Services: []string{"ec2"}}, nil, false},
	{"services holder, equal services group", &PermissionConstraints{Services: []string{"ec2"}}, &PermissionConstraints{Services: []string{"ec2"}}, true},
	{"services holder, subset services group", &PermissionConstraints{Services: []string{"ec2", "rds"}}, &PermissionConstraints{Services: []string{"ec2"}}, true},
	{"regions holder, wider regions group", &PermissionConstraints{Regions: []string{"us-east-1"}}, &PermissionConstraints{Regions: []string{"us-east-1", "eu-west-1"}}, false},
	{"regions holder, other regions group", &PermissionConstraints{Regions: []string{"us-east-1"}}, &PermissionConstraints{Regions: []string{"other"}}, false},
	{"regions holder, no regions group", &PermissionConstraints{Regions: []string{"us-east-1"}}, nil, false},
	{"regions holder, equal regions group", &PermissionConstraints{Regions: []string{"us-east-1"}}, &PermissionConstraints{Regions: []string{"us-east-1"}}, true},
	{"regions holder, subset regions group", &PermissionConstraints{Regions: []string{"us-east-1", "eu-west-1"}}, &PermissionConstraints{Regions: []string{"us-east-1"}}, true},
	{"unconstrained holder, unconstrained group", nil, nil, true},
	{"unconstrained holder, capped group", nil, &PermissionConstraints{MaxPurchaseAmount: 10}, true},
}

func targetGroupWith(c *PermissionConstraints) *Group {
	return &Group{
		ID:   ceilingTargetID,
		Name: "Target",
		Permissions: []Permission{
			{Action: ActionExecute, Resource: ResourcePurchases, Constraints: c},
		},
	}
}

func TestMembershipCeiling_CarvedOutHonoursHeldConstraints(t *testing.T) {
	ctx := context.Background()
	for _, tc := range membershipConstraintCases {
		t.Run(tc.name, func(t *testing.T) {
			mockStore := new(MockStore)
			svc := newCeilingService(t, mockStore)
			mockStore.On("GetGroup", ctx, ceilingTargetID).Return(targetGroupWith(tc.target), nil)

			err := svc.checkMembershipGrantCeilingWithPerms(ctx, purchaseHeld(tc.held), []string{ceilingTargetID})
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPermissionNotGrantable)
		})
	}
}

func TestGuardSelfCarvedOutGrant_HonoursHeldConstraints(t *testing.T) {
	ctx := context.Background()
	for _, tc := range membershipConstraintCases {
		t.Run(tc.name, func(t *testing.T) {
			mockStore := new(MockStore)
			svc := newCeilingService(t, mockStore)
			mockStore.On("GetGroup", ctx, ceilingTargetID).Return(targetGroupWith(tc.target), nil)

			err := svc.guardSelfCarvedOutGrant(ctx, purchaseHeld(tc.held), []string{ceilingTargetID}, nil)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrSelfEscalation)
		})
	}
}

// End to end through UpdateUser: a capped purchaser with update:users cannot
// move another user into an uncapped purchasing group.
func TestMembershipCeiling_UpdateUser_CappedPurchaserCannotAssignUncappedGroup(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	stubActorPermissions(ctx, mockStore, purchaseHeld(&PermissionConstraints{MaxPurchaseAmount: 100}))
	mockStore.On("GetUserByID", ctx, ceilingTargetID).
		Return(&User{ID: ceilingTargetID, Active: true}, nil)
	mockStore.On("GetGroup", ctx, DefaultPurchaserGroupID).Return(&Group{
		ID:          DefaultPurchaserGroupID,
		Name:        GroupPurchaser,
		Permissions: []Permission{{Action: ActionExecute, Resource: ResourcePurchases}},
	}, nil)

	_, err := svc.UpdateUser(ctx, ceilingActorID, ceilingTargetID, UpdateUserRequest{
		GroupIDs: []string{DefaultPurchaserGroupID},
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPermissionNotGrantable)
	mockStore.AssertNotCalled(t, "UpdateUserAdminFields", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Group create/update ceiling: a carved-out permission already on the group
// may be carried through an edit only at constraints no broader than stored.
func TestGroupCeiling_CarvedOutCarryThroughHonoursConstraints(t *testing.T) {
	ctx := context.Background()
	stored := []Permission{{Action: ActionExecute, Resource: ResourcePurchases,
		Constraints: &PermissionConstraints{MaxPurchaseAmount: 100}}}
	cases := []struct {
		name string
		req  *PermissionConstraints
		ok   bool
	}{
		{"same cap", &PermissionConstraints{MaxPurchaseAmount: 100}, true},
		{"lower cap", &PermissionConstraints{MaxPurchaseAmount: 10}, true},
		{"higher cap", &PermissionConstraints{MaxPurchaseAmount: 1000}, false},
		{"cap removed", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockStore := new(MockStore)
			svc := newCeilingService(t, mockStore)
			stubActorPermissions(ctx, mockStore, adminOnly)
			req := []Permission{{Action: ActionExecute, Resource: ResourcePurchases, Constraints: tc.req}}

			err := svc.checkGrantCeiling(ctx, ceilingActorID, req, stored)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPermissionNotGrantable)
		})
	}
}
