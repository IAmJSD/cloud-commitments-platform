package auth

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func globalAllowed(t *testing.T, perms []Permission, action, resource string) bool {
	t.Helper()
	got, err := PermissionsAllowForConstraintSets(perms, action, resource, []PermissionConstraints{GlobalConstraints()})
	require.NoError(t, err)
	return got
}

func TestGlobalConstraints_HeldGrantTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		held *PermissionConstraints
		want bool
	}{
		{"nil constraints", nil, true},
		{"empty constraints", &PermissionConstraints{}, true},
		{"account", &PermissionConstraints{AccountIDs: []string{"a"}}, false},
		{"blank account id", &PermissionConstraints{AccountIDs: []string{""}}, false},
		{"provider", &PermissionConstraints{Providers: []string{"aws"}}, false},
		{"service", &PermissionConstraints{Services: []string{"ec2"}}, false},
		{"regions only", &PermissionConstraints{Regions: []string{"us-east-1"}}, false},
		{"positive cap", &PermissionConstraints{MaxPurchaseAmount: 5}, false},
		{"NaN cap", &PermissionConstraints{MaxPurchaseAmount: math.NaN()}, false},
		{"negative cap", &PermissionConstraints{MaxPurchaseAmount: -1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plain := []Permission{{Action: ActionUpdate, Resource: ResourceConfig, Constraints: tc.held}}
			assert.Equal(t, tc.want, globalAllowed(t, plain, ActionUpdate, ResourceConfig))
			admin := []Permission{constrainedAdmin(tc.held)}
			assert.Equal(t, tc.want, globalAllowed(t, admin, ActionUpdate, ResourceConfig))
		})
	}
}

func TestGlobalConstraints_CarvedOutAndUnion(t *testing.T) {
	t.Parallel()
	admin := []Permission{{Action: ActionAdmin, Resource: ResourceAll}}
	assert.False(t, globalAllowed(t, admin, ActionExecute, ResourcePurchases), "carve-out still needs an explicit grant")
	assert.False(t, globalAllowed(t, nil, ActionUpdate, ResourceConfig))

	union := []Permission{
		{Action: ActionExecute, Resource: ResourcePurchases, Constraints: &PermissionConstraints{AccountIDs: []string{"a"}}},
		{Action: ActionExecute, Resource: ResourcePurchases},
	}
	assert.True(t, globalAllowed(t, union, ActionExecute, ResourcePurchases), "an unconstrained grant in the union allows")
	assert.False(t, globalAllowed(t, union[:1], ActionExecute, ResourcePurchases))
}

func TestGlobalConstraints_NilAndEmptyProbesUnchanged(t *testing.T) {
	t.Parallel()
	capped := []Permission{{Action: ActionExecute, Resource: ResourcePurchases,
		Constraints: &PermissionConstraints{AccountIDs: []string{"a"}, MaxPurchaseAmount: 5}}}
	assert.True(t, permissionsAllow(capped, ActionExecute, ResourcePurchases, nil))
	assert.True(t, permissionsAllow(capped, ActionExecute, ResourcePurchases, &PermissionConstraints{}))
	assert.True(t, permissionsAllow([]Permission{constrainedAdmin(&PermissionConstraints{AccountIDs: []string{"a"}})},
		ActionUpdate, ResourceConfig, nil))
}

func TestPermissionConstraintsRestricts_CoversEveryField(t *testing.T) {
	t.Parallel()
	assert.False(t, (*PermissionConstraints)(nil).restricts())
	assert.False(t, (&PermissionConstraints{}).restricts())
	assert.False(t, (&PermissionConstraints{StrictScope: true, Global: true}).restricts())

	typ := reflect.TypeFor[PermissionConstraints]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Name == "StrictScope" || field.Name == "Global" {
			continue
		}
		c := PermissionConstraints{}
		fv := reflect.ValueOf(&c).Elem().Field(i)
		switch field.Type.Kind() {
		case reflect.Slice:
			fv.Set(reflect.ValueOf([]string{"x"}))
		case reflect.Float64:
			fv.SetFloat(1)
		default:
			t.Fatalf("field %s has kind %s that this guard cannot populate; extend restricts() and the guard", field.Name, field.Type.Kind())
		}
		assert.True(t, c.restricts(), "restricts() ignores field %s", field.Name)
	}
}

func TestGlobalIsRequestOnly(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(GlobalConstraints())
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(encoded))
	var decoded PermissionConstraints
	require.NoError(t, json.Unmarshal([]byte(`{"Global":true,"global":true}`), &decoded))
	assert.False(t, decoded.Global)
	field, found := reflect.TypeFor[PermissionConstraints]().FieldByName("Global")
	require.True(t, found)
	assert.Equal(t, "-", field.Tag.Get("dynamodbav"))
}

// Key x owner decision matrix: Global needs an unconstrained grant on BOTH the
// key's effective permissions and the owner's group permissions.
func TestHasAPIKeyPermissionForConstraintsAPI_GlobalKeyOwnerMatrix(t *testing.T) {
	scoped := &PermissionConstraints{AccountIDs: []string{"acct-a"}}
	cases := []struct {
		name     string
		keyHeld  *PermissionConstraints
		ownerHld *PermissionConstraints
		want     bool
	}{
		{"unconstrained key, unconstrained owner", nil, nil, true},
		{"constrained key, unconstrained owner", scoped, nil, false},
		{"unconstrained key, constrained owner", nil, scoped, false},
		{"constrained key, constrained owner", scoped, scoped, false},
		{"cap-only key, unconstrained owner", &PermissionConstraints{MaxPurchaseAmount: 9}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := effKey(Permission{Action: ActionUpdate, Resource: ResourceConfig, Constraints: tc.keyHeld})
			ownerPerms := []Permission{{Action: ActionUpdate, Resource: ResourceConfig, Constraints: tc.ownerHld}}
			s := &Service{store: effStore(t, key, effOwner(), ownerPerms)}
			got, err := s.HasAPIKeyPermissionForConstraintsAPI(context.Background(), effKeyID, effOwnerID,
				ActionUpdate, ResourceConfig, []PermissionConstraints{GlobalConstraints()})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHasAPIKeyPermissionForConstraintsAPI_GlobalConstrainedAdminKey(t *testing.T) {
	ownerPerms := []Permission{{Action: ActionAdmin, Resource: ResourceAll}}
	for _, tc := range []struct {
		name string
		held *PermissionConstraints
		want bool
	}{
		{"constrained admin key", &PermissionConstraints{AccountIDs: []string{"acct-a"}}, false},
		{"unconstrained admin key", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{store: effStore(t, effKey(constrainedAdmin(tc.held)), effOwner(), ownerPerms)}
			got, err := s.HasAPIKeyPermissionForConstraintsAPI(context.Background(), effKeyID, effOwnerID,
				ActionAdmin, ResourceAll, []PermissionConstraints{GlobalConstraints()})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestUserHasAdminCapability_IgnoresConstrainedAdmin(t *testing.T) {
	for _, tc := range []struct {
		name string
		held *PermissionConstraints
		want bool
	}{
		{"unconstrained", nil, true},
		{"empty constraints", &PermissionConstraints{}, true},
		{"account constrained", &PermissionConstraints{AccountIDs: []string{"acct-a"}}, false},
		{"cap constrained", &PermissionConstraints{MaxPurchaseAmount: 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{store: effStore(t, nil, effOwner(), []Permission{constrainedAdmin(tc.held)})}
			got, err := s.UserHasAdminCapability(context.Background(), effOwnerID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
