package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Canceling a scheduled purchase that belongs to a plan also disables that
// plan, so it requires the permission that updating a plan requires.

const planPermPlanID = "dddd9790-0000-4000-8000-000000000001"

func planPerms(verbs ...string) []auth.Permission {
	perms := make([]auth.Permission, 0, len(verbs))
	for _, v := range verbs {
		perms = append(perms, auth.Permission{Action: v, Resource: auth.ResourcePlans})
	}
	return perms
}

// servePlannedDelete sends DELETE and reports the status plus the number of
// writes (cancel transitions and plan updates) that reached the store.
func servePlannedDelete(t *testing.T, a *keyScopeAuth, planID string, headers map[string]string) (int, int) {
	t.Helper()
	store := new(MockConfigStore)
	exec := keyScopeExec("pending", keyScopeVictim)
	exec.PlanID = planID
	canceled := *exec
	canceled.Status = config.StatusCanceled
	store.On("GetExecutionByID", mock.Anything, keyScopeExecID).Return(exec, nil)
	store.On("TransitionExecutionStatus", mock.Anything, keyScopeExecID, mock.Anything, config.StatusCanceled, mock.Anything).
		Return(&canceled, nil).Maybe()
	store.On("DeleteSuppressionsByExecution", mock.Anything, keyScopeExecID).Return(nil).Maybe()
	store.GetPurchasePlanFn = func(_ context.Context, id string) (*config.PurchasePlan, error) {
		return &config.PurchasePlan{ID: id, Name: "p", Enabled: true}, nil
	}
	store.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()

	h := &Handler{auth: a, config: store}
	rt := keyScopeRoutes[0]
	resp, err := h.HandleRequest(context.Background(), keyScopeRequest(rt, headers))
	require.NoError(t, err)
	require.NotNil(t, resp)

	writes := 0
	for _, c := range store.Calls {
		if c.Method == "TransitionExecutionStatus" || c.Method == "UpdatePurchasePlan" {
			writes++
		}
	}
	return resp.StatusCode, writes
}

func TestDeletePlannedPurchase_PlanBearingRowNeedsPlanUpdate(t *testing.T) {
	for _, verb := range []string{auth.ActionCancelAny, auth.ActionUpdateAny} {
		t.Run("session "+verb+" without plan update is refused", func(t *testing.T) {
			a := newKeyScopeAuthWithGroup(nil, purchasesPerms(verb))
			code, writes := servePlannedDelete(t, a, planPermPlanID, ownerHeaders())
			assert.Equal(t, http.StatusForbidden, code)
			assert.Zero(t, writes)
		})
		t.Run("session "+verb+" with plan update succeeds", func(t *testing.T) {
			group := append(purchasesPerms(verb), planPerms(auth.ActionUpdate)...)
			a := newKeyScopeAuthWithGroup(nil, group)
			code, writes := servePlannedDelete(t, a, planPermPlanID, ownerHeaders())
			assert.Equal(t, http.StatusOK, code)
			assert.Equal(t, 2, writes)
		})
		t.Run("key "+verb+" without plan update is refused", func(t *testing.T) {
			group := append(purchasesPerms(verb), planPerms(auth.ActionUpdate)...)
			a := newKeyScopeAuthWithGroup(purchasesPerms(verb), group)
			code, writes := servePlannedDelete(t, a, planPermPlanID, keyHeaders())
			assert.Equal(t, http.StatusForbidden, code)
			assert.Zero(t, writes)
		})
		t.Run("key "+verb+" with plan update succeeds", func(t *testing.T) {
			perms := append(purchasesPerms(verb), planPerms(auth.ActionUpdate)...)
			a := newKeyScopeAuthWithGroup(perms, perms)
			code, writes := servePlannedDelete(t, a, planPermPlanID, keyHeaders())
			assert.Equal(t, http.StatusOK, code)
			assert.Equal(t, 2, writes)
		})
	}
}

func TestDeletePlannedPurchase_RowWithoutPlanNeedsNoPlanUpdate(t *testing.T) {
	a := newKeyScopeAuthWithGroup(nil, purchasesPerms(auth.ActionCancelAny))
	code, writes := servePlannedDelete(t, a, "", ownerHeaders())
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, 1, writes)
}
