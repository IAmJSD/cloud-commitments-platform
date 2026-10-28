//go:build integration

package api

// Issue #835 on real PostgreSQL: approveWithDelay's CAS to "scheduled" wins,
// then a revoke cancels the row before the scheduled_execution_at stamp is
// saved. Real store and handler; the revoke is the exact store work
// revokeScheduledExecution does (CancelScheduledExecutionAtomic plus the
// suppression release, in one transaction). Nothing here reaches a cloud API.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// revokeBeforeStampStore runs revoke once, ahead of the first execution save
// of either kind, so the revoke lands between the CAS and the stamp whether
// the stamp is guarded or not.
type revokeBeforeStampStore struct {
	*config.PostgresStore
	once   sync.Once
	revoke func()
}

func (s *revokeBeforeStampStore) SavePurchaseExecution(ctx context.Context, e *config.PurchaseExecution) error {
	s.once.Do(s.revoke)
	return s.PostgresStore.SavePurchaseExecution(ctx, e)
}

func (s *revokeBeforeStampStore) SaveExecutionIfStatus(ctx context.Context, e *config.PurchaseExecution, expected []string) error {
	s.once.Do(s.revoke)
	return s.PostgresStore.SaveExecutionIfStatus(ctx, e, expected)
}

// Before the fix the stamp's full-row upsert wrote "scheduled" back over the
// canceled row, with a scheduled_execution_at the scheduler then picks up: a
// purchase the user revoked for free would be bought anyway.
func TestApproveWithDelay_Postgres_RevokeBeforeStampStaysCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	f := newCreateConcurrencyFixture(ctx, t)

	row := &config.PurchaseExecution{
		ExecutionID: uuid.NewString(), PlanID: f.planID, Status: "notified", StepNumber: 3, ScheduledDate: time.Now(),
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, row))
	revokedBy := "revoker@example.com"
	var canceled bool
	f.handler.config = &revokeBeforeStampStore{PostgresStore: f.store, revoke: func() {
		require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			if canceled, _, err = f.store.CancelScheduledExecutionAtomic(ctx, tx, row.ExecutionID, &revokedBy); err != nil {
				return err
			}
			return f.store.DeleteSuppressionsByExecutionTx(ctx, tx, row.ExecutionID)
		}))
	}}

	// A delay already in the past: had the stamp landed, the scheduler's due
	// query would return the row on its next tick.
	_, err := f.handler.approveWithDelay(ctx, row, -time.Minute, "approver@example.com", nil, "")

	require.True(t, canceled, "the revoke must have won its CAS on the scheduled row")
	got, getErr := f.store.GetExecutionByID(ctx, row.ExecutionID)
	require.NoError(t, getErr)
	assert.Equal(t, config.StatusCanceled, got.Status, "the revoke must survive the approve's stamp")
	assert.Nil(t, got.ScheduledExecutionAt)
	due, dueErr := f.store.GetScheduledExecutionsDue(ctx)
	require.NoError(t, dueErr)
	for i := range due {
		assert.NotEqual(t, row.ExecutionID, due[i].ExecutionID, "a revoked row must never be due for the scheduler")
	}
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 409, ce.code)
}
