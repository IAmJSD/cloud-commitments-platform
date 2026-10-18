//go:build integration

package api

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #415 against real PostgreSQL: the handler's Constraints check passes on
// the row it read, a second writer reprices the row over the cap before the
// claim CAS, and the funnel must refuse the row the CAS returned. Real store,
// real handler and purchase manager, counting fake provider (runNowService);
// the Constraints are decided by the real auth matcher. Nothing here reaches a
// cloud API.

const (
	claimRaceCap      = 1000.0
	claimRaceRepriced = 5000.0 // fanoutRecs() costs 300
)

// repriceDuringCheck answers the Constraints check with the real matcher and,
// on the first check that sees the original price, reprices the row with a
// separate UPDATE: the repricing lands after the handler's check and before
// the CAS. reprice=false leaves the row alone (the unmodified control).
func repriceDuringCheck(t *testing.T, f *pauseClaimFixture, perms []auth.Permission, reprice bool) *MockAuthService {
	t.Helper()
	var once sync.Once
	mockAuth := new(MockAuthService)
	session := &Session{UserID: *f.execution.CreatedByUserID, Email: "creator@example.com"}
	mockAuth.On("ValidateSession", mock.Anything, "admin-token").Return(session, nil).Maybe()
	mockAuth.On("ValidateCSRFToken", mock.Anything, "admin-token", "").Return(nil).Maybe()
	authCtx := &auth.AuthContext{Permissions: perms}
	mockAuth.On("HasPermissionAPI", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(func(action, resource string) bool { return authCtx.HasPermission(action, resource) }, nil).Maybe()
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	mockAuth.On("HasPermissionForConstraintsAPI", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(constraintDecisionFunc(func(action, resource string, sets []auth.PermissionConstraints) (bool, error) {
			allowed, err := auth.PermissionsAllowForConstraintSets(perms, action, resource, sets)
			if reprice && allowed && len(sets) > 0 && sets[0].MaxPurchaseAmount < claimRaceCap {
				once.Do(func() { repriceRow(t, f, claimRaceRepriced) })
			}
			return allowed, err
		}), nil).Maybe()
	return mockAuth
}

// repriceRow is the concurrent writer: a raw UPDATE on its own pool
// connection, as a repricing or an edit would be.
func repriceRow(t *testing.T, f *pauseClaimFixture, upfront float64) {
	t.Helper()
	recs := fanoutRecs()
	for i := range recs {
		recs[i].CloudAccountID = f.execution.CloudAccountID
		recs[i].UpfrontCost = upfront
	}
	payload, err := json.Marshal(recs)
	require.NoError(t, err)
	tag, err := f.pool.Exec(f.ctx, `UPDATE purchase_executions SET recommendations = $2 WHERE execution_id = $1`, f.execution.ExecutionID, payload)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

func claimRacePerm(action string) auth.Permission {
	return auth.Permission{Action: action, Resource: auth.ResourcePurchases, Constraints: &auth.PermissionConstraints{MaxPurchaseAmount: claimRaceCap}}
}

type claimRaceEntry struct {
	name  string
	perms []auth.Permission
	call  func(f *pauseClaimFixture) error
}

func claimRaceEntries() []claimRaceEntry {
	return []claimRaceEntry{
		{"run-now", []auth.Permission{claimRacePerm(auth.ActionExecute)}, func(f *pauseClaimFixture) error {
			_, err := f.runNow()
			return err
		}},
		{"session approve", []auth.Permission{claimRacePerm(auth.ActionApproveAny)}, func(f *pauseClaimFixture) error {
			_, err := f.handler.approvePurchase(f.ctx, f.request, f.execution.ExecutionID, "")
			return err
		}},
	}
}

func TestClaimedRowCheck_Postgres_RepricedBetweenCheckAndClaimIsRefused(t *testing.T) {
	for _, e := range claimRaceEntries() {
		t.Run(e.name, func(t *testing.T) {
			f, service := newRunNowFixture(t, 0, fanoutRecs())
			f.handler.auth = repriceDuringCheck(t, f, e.perms, true)

			err := e.call(f)

			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a client-facing error, got %v", err)
			assert.Equal(t, 403, ce.code)
			assert.Contains(t, ce.message, "nothing was bought")
			assert.Zero(t, service.calls.Load(), "the repriced row must never reach the provider")
			row, getErr := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
			require.NoError(t, getErr)
			assert.Equal(t, claimRaceRepriced, row.Recommendations[0].UpfrontCost, "the race must have landed")
			assert.Equal(t, "failed", row.Status)
			assert.True(t, row.NoSubmission, "refused before any provider call")
			assert.Nil(t, row.ApprovedBy)
			assert.Nil(t, row.ExecutedAt)
			assert.Contains(t, row.Error, "constraints")
			var histories int
			require.NoError(t, f.pool.QueryRow(f.ctx, "SELECT count(*) FROM purchase_history WHERE plan_id=$1", f.planID).Scan(&histories))
			assert.Zero(t, histories)
		})
	}
}

func TestClaimedRowCheck_Postgres_UnmodifiedRowPurchases(t *testing.T) {
	for _, e := range claimRaceEntries() {
		t.Run(e.name, func(t *testing.T) {
			f, service := newRunNowFixture(t, 0, fanoutRecs())
			f.handler.auth = repriceDuringCheck(t, f, e.perms, false)

			require.NoError(t, e.call(f))

			assert.EqualValues(t, 1, service.calls.Load())
			row, err := f.store.GetExecutionByID(f.ctx, f.execution.ExecutionID)
			require.NoError(t, err)
			assert.Equal(t, "completed", row.Status)
		})
	}
}
