//go:build integration

package api

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestRevokePurchase_RecordsActorUUIDOnRealPostgres is the regression test for
// #720: revoking a completed execution used to pass the actor's email as
// transitioned_by, a UUID foreign key, so Postgres rejected every revoke.
func TestRevokePurchase_RecordsActorUUIDOnRealPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	f := newCreateConcurrencyFixture(ctx, t)

	var creator string
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT id::text FROM users WHERE email='creator@example.com'").Scan(&creator))
	execID := uuid.NewString()
	completedAt := time.Now()
	require.NoError(t, f.store.SavePurchaseExecution(ctx, &config.PurchaseExecution{
		ExecutionID: execID, PlanID: f.planID, Status: "completed", StepNumber: 3,
		ScheduledDate: time.Now(), CompletedAt: &completedAt, CreatedByUserID: &creator,
	}))

	// The session-authed revoke path enforces CSRF; this request carries no CSRF header.
	fixtureAuth := f.handler.auth.(*MockAuthService)
	t.Cleanup(func() { fixtureAuth.AssertExpectations(t) })
	fixtureAuth.On("ValidateCSRFToken", mock.Anything, "admin-token", "").Return(nil)
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}}
	_, err := f.handler.revokeViaEmailToken(ctx, req, execID, "")
	require.NoError(t, err, "revoke must not fail on the transitioned_by UUID column")

	var status string
	var transitionedBy *string
	require.NoError(t, f.pool.QueryRow(ctx,
		"SELECT status, transitioned_by::text FROM purchase_executions WHERE execution_id=$1", execID,
	).Scan(&status, &transitionedBy))
	require.Equal(t, "revocation_requested", status)
	require.NotNil(t, transitionedBy)
	require.Equal(t, creator, *transitionedBy)
}
