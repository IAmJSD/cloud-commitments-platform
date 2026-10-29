package api

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Plan mutations require EVERY plan account in the caller's scope (issue
// #520). A user scoped to [A] must not change or delete a plan on {A,B}.

func planMutationRoutes() []struct{ method, path, body string } {
	base := "/api/plans/" + scopePlanID
	return []struct{ method, path, body string }{
		{"PUT", base, `{"name":"x"}`},
		{"PATCH", base, `{"enabled":false}`},
		{"DELETE", base, ""},
		{"POST", base + "/purchases", `{}`},
		{"PUT", base + "/accounts", `{"account_ids":["` + scopedInAccount + `"]}`},
	}
}

func TestRouterDispatch_PlanMutations_PartiallyScopedPlanRefused(t *testing.T) {
	for _, rt := range planMutationRoutes() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			ctx := context.Background()
			h, store := scopedHandler(t, scopedInAccount)
			write := seedPlanAccountsStore(store, []config.CloudAccount{inScopeAccount(), outOfScopeAccount()})
			store.On("DeletePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()

			_, err := NewRouter(h).Route(ctx, rt.method, rt.path, scopedRequest(rt.body))

			require.Error(t, err)
			assert.True(t, IsNotFoundError(err), "expected not-found, got %v", err)
			assert.False(t, write.called)
			store.AssertNotCalled(t, "DeletePurchasePlan", mock.Anything, mock.Anything)
		})
	}
}

func TestRouterDispatch_PlanMutations_FullyScopedPlanPassesGate(t *testing.T) {
	for _, rt := range planMutationRoutes() {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			ctx := context.Background()
			h, store := scopedHandler(t, scopedInAccount)
			seedPlanAccountsStore(store, []config.CloudAccount{inScopeAccount()})
			store.On("DeletePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()

			_, err := NewRouter(h).Route(ctx, rt.method, rt.path, scopedRequest(rt.body))

			// Handlers may fail later (fixtures are minimal); the scope gate must not.
			if err != nil {
				assert.False(t, IsNotFoundError(err), "scope gate refused a fully in-scope plan: %v", err)
			}
		})
	}
}

func TestRouterDispatch_DeletePlan_AllAccountsInScopeDeletes(t *testing.T) {
	ctx := context.Background()
	h, store := scopedHandler(t, scopedInAccount, scopedOutAccount)
	seedPlanAccountsStore(store, []config.CloudAccount{inScopeAccount(), outOfScopeAccount()})
	store.On("DeletePurchasePlan", mock.Anything, scopePlanID).Return(nil).Once()

	_, err := NewRouter(h).Route(ctx, "DELETE", "/api/plans/"+scopePlanID, scopedRequest(""))

	require.NoError(t, err)
	store.AssertExpectations(t)
}

func TestRouterDispatch_DeletePlan_UnrestrictedSkipsPlanLookup(t *testing.T) {
	ctx := context.Background()
	h, store := scopedHandler(t)
	t.Cleanup(func() { store.AssertExpectations(t) })
	store.On("DeletePurchasePlan", mock.Anything, scopePlanID).Return(nil).Once()

	_, err := NewRouter(h).Route(ctx, "DELETE", "/api/plans/"+scopePlanID, scopedRequest(""))

	require.NoError(t, err)
	store.AssertNotCalled(t, "GetPlanAccounts", mock.Anything, mock.Anything)
}

func TestRouterDispatch_DeletePlan_ZeroAccountPlanHiddenFromScoped(t *testing.T) {
	ctx := context.Background()
	h, store := scopedHandler(t, scopedInAccount)
	seedPlanAccountsStore(store, nil)

	_, err := NewRouter(h).Route(ctx, "DELETE", "/api/plans/"+scopePlanID, scopedRequest(""))

	require.Error(t, err)
	assert.True(t, IsNotFoundError(err))
}

// Reads keep any-account visibility.
func TestRouterDispatch_GetPlan_PartiallyScopedPlanStillReadable(t *testing.T) {
	ctx := context.Background()
	h, store := scopedHandler(t, scopedInAccount)
	seedPlanAccountsStore(store, []config.CloudAccount{inScopeAccount(), outOfScopeAccount()})

	_, err := NewRouter(h).Route(ctx, "GET", "/api/plans/"+scopePlanID, scopedRequest(""))

	if err != nil {
		assert.False(t, IsNotFoundError(err), "read gate must stay any-account: %v", err)
	}
}
