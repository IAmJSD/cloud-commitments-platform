//go:build integration

package auth

// Issue #919 against the real schema: the seeded groups come back from the
// API layer flagged system_managed, and Service.UpdateGroup refuses to write
// one even when handed a struct that does not carry the flag, leaving the
// stored row unchanged.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegration_SystemManagedGroup_APIFlagAndUpdateGuard(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	svc := NewService(ServiceConfig{Store: store})
	ctx := t.Context()

	listed, err := svc.ListGroupsAPI(ctx)
	require.NoError(t, err)
	groups, ok := listed.([]*APIGroup)
	require.True(t, ok)
	flags := make(map[string]bool, len(groups))
	for _, g := range groups {
		flags[g.ID] = g.SystemManaged
	}
	require.Contains(t, flags, DefaultAdminGroupID)
	require.Contains(t, flags, DefaultPurchaserGroupID)
	assert.True(t, flags[DefaultAdminGroupID], "Administrators must be flagged system_managed")
	assert.True(t, flags[DefaultPurchaserGroupID], "Purchaser must be flagged system_managed")

	before, err := store.GetGroup(ctx, DefaultPurchaserGroupID)
	require.NoError(t, err)
	require.NotNil(t, before)

	wiped := &Group{
		ID:          DefaultPurchaserGroupID,
		Name:        before.Name + " (renamed)",
		Permissions: []Permission{{Action: ActionView, Resource: ResourceAll}},
	}
	err = svc.UpdateGroup(ctx, wiped)
	require.ErrorIs(t, err, ErrSystemManagedGroup)

	after, err := store.GetGroup(ctx, DefaultPurchaserGroupID)
	require.NoError(t, err)
	assert.Equal(t, before.Name, after.Name)
	assert.Equal(t, before.Permissions, after.Permissions)
	assert.True(t, after.UpdatedAt.Equal(before.UpdatedAt), "a refused update must not touch the row")

	// Negative control: a custom group still updates through the same path.
	custom := &Group{Name: "Team 919", Permissions: []Permission{{Action: ActionView, Resource: ResourceAll}}}
	require.NoError(t, svc.CreateGroup(ctx, custom, ""))
	custom.Description = "edited"
	require.NoError(t, svc.UpdateGroup(ctx, custom))
	stored, err := store.GetGroup(ctx, custom.ID)
	require.NoError(t, err)
	assert.Equal(t, "edited", stored.Description)
	assert.False(t, stored.SystemManaged)
}
