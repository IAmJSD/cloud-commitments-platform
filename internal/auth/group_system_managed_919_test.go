package auth

// Issue #919: the system_managed guard must live on Service.UpdateGroup
// itself, not only on its caller UpdateGroupAPI, and the API representation
// of a group must carry the flag so the dashboard can hide Edit and Delete.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func seededPurchaserGroup() *Group {
	return &Group{
		ID:            DefaultPurchaserGroupID,
		Name:          GroupPurchaser,
		SystemManaged: true,
		Permissions: []Permission{
			{Action: ActionExecute, Resource: ResourcePurchases},
			{Action: ActionApproveAny, Resource: ResourcePurchases},
		},
	}
}

func TestServiceUpdateGroup_SystemManagedGuard(t *testing.T) {
	ctx := context.Background()

	t.Run("refuses a system-managed group even when the caller's struct omits the flag", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		svc := newCeilingService(t, mockStore)

		mockStore.On("GetGroup", ctx, DefaultPurchaserGroupID).Return(seededPurchaserGroup(), nil).Once()
		mockStore.On("UpdateGroup", ctx, mock.AnythingOfType("*auth.Group")).Return(nil).Maybe()

		// A caller-built struct: SystemManaged is false here, so the guard
		// has to consult the stored row rather than trust its argument.
		wiped := &Group{
			ID:          DefaultPurchaserGroupID,
			Name:        GroupPurchaser,
			Permissions: []Permission{{Action: ActionView, Resource: ResourceAll}},
		}
		err := svc.UpdateGroup(ctx, wiped)

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSystemManagedGroup)
		assert.Contains(t, err.Error(), GroupPurchaser)
		mockStore.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
	})

	t.Run("a missing group is reported as not found without writing", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		svc := newCeilingService(t, mockStore)

		mockStore.On("GetGroup", ctx, ceilingTargetID).Return(nil, nil).Once()

		err := svc.UpdateGroup(ctx, &Group{ID: ceilingTargetID, Name: "Gone"})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "group not found")
		assert.NotErrorIs(t, err, ErrSystemManagedGroup)
		mockStore.AssertNotCalled(t, "UpdateGroup", mock.Anything, mock.Anything)
	})

	t.Run("an ordinary group is still updatable", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		svc := newCeilingService(t, mockStore)

		group := &Group{ID: ceilingTargetID, Name: "Team"}
		mockStore.On("GetGroup", ctx, ceilingTargetID).Return(&Group{ID: ceilingTargetID, Name: "Old"}, nil).Once()
		mockStore.On("UpdateGroup", ctx, group).Return(nil).Once()

		require.NoError(t, svc.UpdateGroup(ctx, group))
	})
}

// TestAPIGroup_CarriesSystemManaged pins the wire field the dashboard reads
// (frontend/src/api/types.ts APIGroup.system_managed) on both read paths.
func TestAPIGroup_CarriesSystemManaged(t *testing.T) {
	ctx := context.Background()
	custom := &Group{ID: ceilingTargetID, Name: "Team"}

	wire := func(t *testing.T, v any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		return m
	}

	t.Run("GetGroupAPI", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		svc := newCeilingService(t, mockStore)
		mockStore.On("GetGroup", ctx, DefaultPurchaserGroupID).Return(seededPurchaserGroup(), nil).Once()
		mockStore.On("GetGroup", ctx, ceilingTargetID).Return(custom, nil).Once()

		seeded, err := svc.GetGroupAPI(ctx, DefaultPurchaserGroupID)
		require.NoError(t, err)
		assert.Equal(t, true, wire(t, seeded)["system_managed"])

		plain, err := svc.GetGroupAPI(ctx, ceilingTargetID)
		require.NoError(t, err)
		assert.NotContains(t, wire(t, plain), "system_managed", "false is omitted, matching the optional frontend field")
	})

	t.Run("ListGroupsAPI", func(t *testing.T) {
		mockStore := new(MockStore)
		t.Cleanup(func() { mockStore.AssertExpectations(t) })
		svc := newCeilingService(t, mockStore)
		mockStore.On("ListGroups", ctx).Return([]Group{*seededPurchaserGroup(), *custom}, nil).Once()

		list, err := svc.ListGroupsAPI(ctx)
		require.NoError(t, err)
		raw, err := json.Marshal(list)
		require.NoError(t, err)
		var groups []map[string]any
		require.NoError(t, json.Unmarshal(raw, &groups))
		require.Len(t, groups, 2)
		assert.Equal(t, true, groups[0]["system_managed"])
		assert.NotContains(t, groups[1], "system_managed")
	})
}

// TestServiceUpdateGroupAPI_SystemManagedRefusalPrecedesCeiling pins the
// order of the two 403 causes (#1521). The service-level guard on UpdateGroup
// would also refuse, but only after the ceiling checks; this request would
// trip the ceiling first, so only the UpdateGroupAPI guard yields
// ErrSystemManagedGroup.
func TestServiceUpdateGroupAPI_SystemManagedRefusalPrecedesCeiling(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	svc := newCeilingService(t, mockStore)

	stubActorPermissionsMaybe(ctx, mockStore, []Permission{{Action: ActionView, Resource: ResourceAll}})
	stubTargetGroup(ctx, mockStore, seededPurchaserGroup())

	_, err := svc.UpdateGroupAPI(ctx, ceilingActorID, ceilingTargetID,
		updateReqWith(APIPermission{Action: ActionAdmin, Resource: ResourceAll}))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSystemManagedGroup)
	assert.NotErrorIs(t, err, ErrPermissionCeiling)
}
