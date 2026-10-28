//go:build integration

package api

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #1129 against real PostgreSQL: a non-creator runs the row through an
// update-any bounded to provider aws. A second writer moves the row to gcp
// after the handler's last pre-claim check and before the claim CAS; the
// funnel's post-claim re-check must refuse the row the CAS returned. Real
// store, handler and purchase manager, counting fake provider (runNowService);
// the Constraints are decided by the real auth matcher. Nothing here reaches a
// cloud API.

// insertNonCreator adds a real users row for the operator: the claim stamps
// the session user onto transitioned_by, a foreign key.
func insertNonCreator(t *testing.T, f *pauseClaimFixture) string {
	t.Helper()
	id := uuid.NewString()
	_, err := f.pool.Exec(f.ctx, `INSERT INTO users (id,email,password_hash,salt,active,group_ids,created_at,updated_at) SELECT $1,'operator@example.invalid','x','y',true,ARRAY[g.id],now(),now() FROM groups g ORDER BY g.name LIMIT 1`, id)
	require.NoError(t, err)
	require.NotEqual(t, id, *f.execution.CreatedByUserID)
	return id
}

// moveProviderDuringExecuteCheck answers permission checks for a non-creator
// session with the real matcher and, on the first execute Constraints check
// (the handler's last pre-claim check), rewrites the row's provider with a
// separate UPDATE. move=false leaves the row alone (the unmodified control).
func moveProviderDuringExecuteCheck(t *testing.T, f *pauseClaimFixture, perms []auth.Permission, move bool) *MockAuthService {
	t.Helper()
	operator := insertNonCreator(t, f)
	var once sync.Once
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{UserID: operator, Email: "operator@example.invalid"}, nil).Maybe()
	mockAuth.On("ValidateCSRFToken", mock.Anything, "admin-token", "").Return(nil).Maybe()
	authCtx := &auth.AuthContext{Permissions: perms}
	mockAuth.On("HasPermissionAPI", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(func(action, resource string) bool { return authCtx.HasPermission(action, resource) }, nil).Maybe()
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	mockAuth.On("HasPermissionForConstraintsAPI", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(constraintDecisionFunc(func(action, resource string, sets []auth.PermissionConstraints) (bool, error) {
			allowed, err := auth.PermissionsAllowForConstraintSets(perms, action, resource, sets)
			if move && action == auth.ActionExecute {
				once.Do(func() { moveRowProvider(t, f, "gcp") })
			}
			return allowed, err
		}), nil).Maybe()
	return mockAuth
}

func moveRowProvider(t *testing.T, f *pauseClaimFixture, provider string) {
	t.Helper()
	recs := fanoutRecs()
	for i := range recs {
		recs[i].CloudAccountID = f.execution.CloudAccountID
		recs[i].Provider = provider
	}
	payload, err := json.Marshal(recs)
	require.NoError(t, err)
	tag, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET recommendations = $2 WHERE execution_id = $1`, f.execution.ExecutionID, payload)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

func runBoundPerms() []auth.Permission {
	return []auth.Permission{
		{Action: auth.ActionExecute, Resource: auth.ResourcePurchases},
		{Action: auth.ActionUpdateAny, Resource: auth.ResourcePurchases, Constraints: &auth.PermissionConstraints{Providers: []string{"aws"}}},
	}
}

func TestRunNow_Postgres_RowMovedOutOfUpdateAnyScopeBeforeClaimIsRefused(t *testing.T) {
	f, service := newRunNowFixture(t, 0, fanoutRecs())
	f.handler.auth = moveProviderDuringExecuteCheck(t, f, runBoundPerms(), true)

	_, err := f.runNow()

	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client-facing error, got %v", err)
	assert.Equal(t, 403, ce.code)
	assert.Contains(t, ce.message, "nothing was bought")
	assert.Zero(t, service.calls.Load(), "the moved row must never reach the provider")
	row, getErr := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
	require.NoError(t, getErr)
	assert.Equal(t, "gcp", row.Recommendations[0].Provider, "the race must have landed")
	assert.Equal(t, "failed", row.Status)
	assert.True(t, row.NoSubmission, "refused before any provider call")
	var histories int
	require.NoError(t, f.pool.QueryRow(f.ctx, "SELECT count(*) FROM purchase_history WHERE plan_id=$1", f.planID).Scan(&histories))
	assert.Zero(t, histories)
}

func TestRunNow_Postgres_InScopeUpdateAnyNonCreatorPurchases(t *testing.T) {
	f, service := newRunNowFixture(t, 0, fanoutRecs())
	f.handler.auth = moveProviderDuringExecuteCheck(t, f, runBoundPerms(), false)

	_, err := f.runNow()
	require.NoError(t, err)

	assert.EqualValues(t, 1, service.calls.Load())
	row, getErr := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
	require.NoError(t, getErr)
	assert.Equal(t, "completed", row.Status)
}
