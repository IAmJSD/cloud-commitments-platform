//go:build integration

package config

// Real-PostgreSQL test for issue #1102 item 5. purchase_executions.execution_id
// is a UUID column, and POST /api/purchases/{id}/revoke asks GetExecutionByID
// about every ID before falling back to purchase_history. An Azure purchase ID
// is an ARM path, which Postgres refuses to cast to uuid (SQLSTATE 22P02), so
// the lookup failed with an error the handler answered as a 500 instead of the
// ErrNotFound that sends the revoke on to the purchase_history path.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestGetExecutionByID_NonUUIDIsNotFoundPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store := setupLadderStore(ctx, t)

	for _, id := range []string{
		"/subscriptions/00000000-0000-0000-0000-000000000000/providers/Microsoft.Capacity/reservationOrders/order-1/reservations/res-1",
		"/providers/Microsoft.Capacity/reservationOrders/order-1",
		"pid-1",
		"",
	} {
		exec, err := store.GetExecutionByID(ctx, id)
		require.ErrorIs(t, err, ErrNotFound, "id %q", id)
		require.Nil(t, exec)
	}

	// The connection is still usable afterwards, and a well-formed UUID behaves
	// as before: missing is ErrNotFound, present is returned.
	_, err := store.GetExecutionByID(ctx, uuid.NewString())
	require.ErrorIs(t, err, ErrNotFound)

	execID := uuid.NewString()
	require.NoError(t, store.SavePurchaseExecution(ctx, &PurchaseExecution{
		ExecutionID: execID, Status: "scheduled", ScheduledDate: time.Now(),
	}))
	got, err := store.GetExecutionByID(ctx, execID)
	require.NoError(t, err)
	require.Equal(t, execID, got.ExecutionID)
}
