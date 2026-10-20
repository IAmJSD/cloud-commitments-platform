package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #520: DELETE /api/purchases/planned/{id} cancels an execution and
// then disables its plan. A single-account child clears requireExecutionAccess
// on its own account while the plan spans accounts the caller cannot see, so
// the plan disable needs the every-account check of the plan mutation routes.
//
// Issue #623: when that check fails, the whole request is refused with a 403
// before anything is written, instead of canceling the execution and silently
// leaving the plan enabled.

const plannedDeleteExecID = "55555555-5555-4555-8555-555555555555"

type plannedDeleteFixture struct {
	h     *Handler
	store *MockConfigStore
}

// newPlannedDeleteFixture wires a scoped session on a child execution for the
// in-scope account, whose plan spans planAccounts. The plan is enabled, so a
// disable shows up as an UpdatePurchasePlan call.
func newPlannedDeleteFixture(t *testing.T, planAccounts []config.CloudAccount, scope ...string) plannedDeleteFixture {
	t.Helper()
	h, store := scopedHandler(t, scope...)
	seedPlanAccountsStore(store, planAccounts)
	store.GetPurchasePlanFn = func(_ context.Context, id string) (*config.PurchasePlan, error) {
		return &config.PurchasePlan{ID: id, Name: "scoped plan", Enabled: true}, nil
	}
	childAccount := scopedInAccount
	exec := &config.PurchaseExecution{
		ExecutionID:    plannedDeleteExecID,
		PlanID:         scopePlanID,
		Status:         "pending",
		CloudAccountID: &childAccount,
	}
	canceled := *exec
	canceled.Status = config.StatusCanceled
	store.On("GetExecutionByID", mock.Anything, plannedDeleteExecID).Return(exec, nil)
	store.On("TransitionExecutionStatus", mock.Anything, plannedDeleteExecID, mock.Anything, config.StatusCanceled, mock.Anything).Return(&canceled, nil).Maybe()
	store.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil).Maybe()
	return plannedDeleteFixture{h: h, store: store}
}

func (f plannedDeleteFixture) delete(t *testing.T) (*DeletePlannedPurchaseResponse, error) {
	t.Helper()
	resp, err := NewRouter(f.h).Route(context.Background(), "DELETE", "/api/purchases/planned/"+plannedDeleteExecID, scopedRequest(""))
	if err != nil {
		return nil, err
	}
	typed, ok := resp.(*DeletePlannedPurchaseResponse)
	require.True(t, ok, "unexpected response type %T", resp)
	return typed, nil
}

// callIndex returns the position of the first recorded call to method, or -1.
func callIndex(store *MockConfigStore, method string) int {
	for i, c := range store.Calls {
		if c.Method == method {
			return i
		}
	}
	return -1
}

func (f plannedDeleteFixture) planDisables() int {
	n := 0
	for _, c := range f.store.Calls {
		if c.Method == "UpdatePurchasePlan" {
			n++
		}
	}
	return n
}

// requireRefusedBeforeCancel asserts the request was refused with a 403 that
// says why, and that neither the execution nor the plan was touched.
func (f plannedDeleteFixture) requireRefusedBeforeCancel(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %T: %v", err, err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, err.Error(), "every account on the plan")
	store := f.store
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	assert.Zero(t, f.planDisables(), "a refused request must not disable the plan")
}

// Issue #623: a caller who cannot disable the plan is refused before the
// cancel, so the plan is never left enabled behind a 200.
func TestRouterDispatch_DeletePlannedPurchase_PartiallyScopedRefusedBeforeCancel(t *testing.T) {
	f := newPlannedDeleteFixture(t, []config.CloudAccount{inScopeAccount(), outOfScopeAccount()}, scopedInAccount)
	store := f.store

	resp, err := f.delete(t)

	assert.Nil(t, resp)
	f.requireRefusedBeforeCancel(t, err)
	store.AssertNotCalled(t, "GetPurchasePlan", mock.Anything, mock.Anything)
}

func TestRouterDispatch_DeletePlannedPurchase_FullyScopedDisablesPlan(t *testing.T) {
	f := newPlannedDeleteFixture(t, []config.CloudAccount{inScopeAccount(), outOfScopeAccount()}, scopedInAccount, scopedOutAccount)
	store := f.store

	resp, err := f.delete(t)

	require.NoError(t, err)
	require.NotNil(t, resp.PlanDisabled)
	assert.True(t, *resp.PlanDisabled)
	store.AssertNumberOfCalls(t, "TransitionExecutionStatus", 1)
	store.AssertCalled(t, "UpdatePurchasePlan", mock.Anything, mock.MatchedBy(func(p *config.PurchasePlan) bool {
		return p.ID == scopePlanID && !p.Enabled
	}))
	assert.Equal(t, 1, f.planDisables())
}

func TestRouterDispatch_DeletePlannedPurchase_UnrestrictedDisablesPlanWithoutScopeLookup(t *testing.T) {
	f := newPlannedDeleteFixture(t, []config.CloudAccount{inScopeAccount(), outOfScopeAccount()})
	store := f.store

	resp, err := f.delete(t)

	require.NoError(t, err)
	require.NotNil(t, resp.PlanDisabled)
	assert.True(t, *resp.PlanDisabled)
	assert.Equal(t, 1, f.planDisables())
	store.AssertNotCalled(t, "GetPlanAccounts", mock.Anything, mock.Anything)
}

// A plan with no accounts is hidden from scoped callers, so the disable is
// refused like the partially scoped case.
func TestRouterDispatch_DeletePlannedPurchase_ZeroAccountPlanRefusedBeforeCancel(t *testing.T) {
	f := newPlannedDeleteFixture(t, nil, scopedInAccount)

	_, err := f.delete(t)

	f.requireRefusedBeforeCancel(t, err)
}

// A store failure while resolving the plan's accounts is not a scope refusal:
// it surfaces as a server error, and nothing is canceled or disabled.
func TestRouterDispatch_DeletePlannedPurchase_PlanAccountLookupFailureSurfacesBeforeCancel(t *testing.T) {
	f := newPlannedDeleteFixture(t, nil, scopedInAccount)
	store := f.store
	store.GetPlanAccountsFn = func(context.Context, string) ([]config.CloudAccount, error) {
		return nil, errors.New("db down")
	}

	_, err := f.delete(t)

	require.Error(t, err)
	assert.False(t, IsNotFoundError(err))
	_, isClient := IsClientError(err)
	assert.False(t, isClient, "a store failure must not read as a client refusal: %v", err)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	assert.Zero(t, f.planDisables())
}

// The plan's accounts can change between the pre-cancel check and the
// disable (an admin attaches an out-of-scope account mid-request). The
// disable re-checks and, when it is refused, reports a 409 naming the
// half-done state instead of a 200 that hides it.
func TestRouterDispatch_DeletePlannedPurchase_PlanAccountsChangeDuringCancel(t *testing.T) {
	f := newPlannedDeleteFixture(t, nil, scopedInAccount)
	lookups := 0
	f.store.GetPlanAccountsFn = func(context.Context, string) ([]config.CloudAccount, error) {
		lookups++
		if lookups == 1 {
			return []config.CloudAccount{inScopeAccount()}, nil
		}
		return []config.CloudAccount{inScopeAccount(), outOfScopeAccount()}, nil
	}

	resp, err := f.delete(t)

	assert.Nil(t, resp)
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %T: %v", err, err)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, err.Error(), "was canceled")
	assert.Contains(t, err.Error(), "not disabled")
	assert.Equal(t, 2, lookups, "the disable must re-check the plan's accounts after the cancel")
	f.store.AssertNumberOfCalls(t, "TransitionExecutionStatus", 1)
	assert.Zero(t, f.planDisables(), "the plan must not be disabled once it spans an out-of-scope account")
}

// A scoped caller canceling an execution with no plan has nothing to disable,
// so no plan scope check runs and the cancel succeeds.
func TestRouterDispatch_DeletePlannedPurchase_ScopedNoPlanCancelsWithoutPlanCheck(t *testing.T) {
	f := newPlannedDeleteFixture(t, nil, scopedInAccount)
	store := f.store
	account := scopedInAccount
	noPlan := &config.PurchaseExecution{ExecutionID: plannedDeleteExecID, Status: "pending", CloudAccountID: &account}
	canceled := *noPlan
	canceled.Status = config.StatusCanceled
	store.ExpectedCalls = nil
	store.On("GetExecutionByID", mock.Anything, plannedDeleteExecID).Return(noPlan, nil)
	store.On("TransitionExecutionStatus", mock.Anything, plannedDeleteExecID, mock.Anything, config.StatusCanceled, mock.Anything).Return(&canceled, nil)
	lookups := 0
	store.GetPlanAccountsFn = func(context.Context, string) ([]config.CloudAccount, error) {
		lookups++
		return nil, nil
	}

	resp, err := f.delete(t)

	require.NoError(t, err)
	assert.Equal(t, "canceled", resp.Status)
	assert.Nil(t, resp.PlanDisabled)
	assert.Zero(t, lookups)
	store.AssertNumberOfCalls(t, "TransitionExecutionStatus", 1)
}

// The execution gate still comes first: a caller who holds none of the
// execution's accounts is refused before anything is canceled or disabled.
func TestRouterDispatch_DeletePlannedPurchase_OutOfScopeExecutionRefusedBeforeAnyWrite(t *testing.T) {
	f := newPlannedDeleteFixture(t, []config.CloudAccount{inScopeAccount(), outOfScopeAccount()}, scopedOutAccount)
	store := f.store

	_, err := f.delete(t)

	require.Error(t, err)
	assert.True(t, IsNotFoundError(err), "got %v", err)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	assert.Zero(t, f.planDisables())
}

// Permission is checked before scope: a caller without any delete or cancel
// verb gets 403 and no execution or plan lookup happens.
func TestRouterDispatch_DeletePlannedPurchase_PermissionRefusedBeforeScope(t *testing.T) {
	f := newPlannedDeleteFixture(t, []config.CloudAccount{inScopeAccount(), outOfScopeAccount()}, scopedInAccount)
	store := f.store
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.grantPermissionsScoped([]auth.Permission{{Action: auth.ActionView, Resource: auth.ResourcePurchases}}, []string{scopedInAccount})
	f.h.auth = mockAuth

	_, err := f.delete(t)

	require.Error(t, err)
	assert.False(t, IsNotFoundError(err), "expected a permission refusal, got %v", err)
	store.AssertNotCalled(t, "GetExecutionByID", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "GetPlanAccounts", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	assert.Zero(t, f.planDisables())
}

// The plan is disabled only after the execution is canceled: a failed cancel
// must never leave a disabled plan behind.
func TestRouterDispatch_DeletePlannedPurchase_CancelsBeforeDisablingPlan(t *testing.T) {
	f := newPlannedDeleteFixture(t, []config.CloudAccount{inScopeAccount()}, scopedInAccount)
	store := f.store

	_, err := f.delete(t)

	require.NoError(t, err)
	cancelAt := callIndex(store, "TransitionExecutionStatus")
	disableAt := callIndex(store, "UpdatePurchasePlan")
	require.NotEqual(t, -1, cancelAt)
	require.NotEqual(t, -1, disableAt)
	assert.Less(t, cancelAt, disableAt, "the cancel must be recorded before the plan disable")
}

// An execution without a plan omits plan_disabled from the JSON body.
func TestRouterDispatch_DeletePlannedPurchase_NoPlanOmitsPlanDisabled(t *testing.T) {
	f := newPlannedDeleteFixture(t, nil)
	store := f.store
	noPlan := &config.PurchaseExecution{ExecutionID: plannedDeleteExecID, Status: config.StatusCanceled}
	store.ExpectedCalls = nil
	store.On("GetExecutionByID", mock.Anything, plannedDeleteExecID).Return(noPlan, nil)
	store.On("TransitionExecutionStatus", mock.Anything, plannedDeleteExecID, mock.Anything, config.StatusCanceled, mock.Anything).Return(noPlan, nil)

	resp, err := f.delete(t)

	require.NoError(t, err)
	assert.Nil(t, resp.PlanDisabled)
	assert.Zero(t, f.planDisables())
	body, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"canceled"}`, string(body))
}
