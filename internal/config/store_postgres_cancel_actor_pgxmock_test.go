package config

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyCancelActorColumn is the pre-migration-000089 column, split so the
// US-locale misspell linter does not flag it; it is the value under test.
const legacyCancelActorColumn = "cancel" + "led_by"

const coalescedActorProjection = "COALESCE(canceled_by, " + legacyCancelActorColumn + ") AS " + legacyCancelActorColumn

var executionProjectionCols = []string{
	"plan_id", "execution_id", "status", "step_number", "scheduled_date",
	"notification_sent", "approval_token", "recommendations",
	"total_upfront_cost", "estimated_savings", "completed_at", "error", "expires_at",
	"cloud_account_id", "source", "approved_by", legacyCancelActorColumn, "capacity_percent",
	"created_by_user_id", "retry_execution_id", "retry_attempt_n",
	"approval_token_expires_at",
	"executed_by_user_id", "executed_at", "pre_approval_skip_reason",
	"idempotency_key", "scheduled_execution_at", "no_submission", "created_by_system",
}

// TestPGXMock_ExecutionProjectionsReadCoalescedActor pins the read half of the
// 000089 contract while old builds can still write the legacy column: every
// statement that projects a purchase_executions row must read
// COALESCE(canceled_by, cancelled_by), so an actor stored in either column
// scans back. A bare canceled_by projection loses the attribution of legacy
// rows (written to cancelled_by only) until the follow-up drain runs.
func TestPGXMock_ExecutionProjectionsReadCoalescedActor(t *testing.T) {
	var mu sync.Mutex
	var projections, bare []string
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherFunc(func(_, actual string) error {
		if !strings.Contains(actual, "capacity_percent") {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		projections = append(projections, actual)
		if !strings.Contains(actual, coalescedActorProjection) {
			bare = append(bare, actual)
		}
		return nil
	})))
	require.NoError(t, err)
	mock.MatchExpectationsInOrder(false)
	store := storeWith(mock)
	ctx := context.Background()
	empty := func() *pgxmock.Rows { return pgxmock.NewRows(executionProjectionCols) }
	// Two rows of expectations per call: a zero-row transition is followed by
	// a probe SELECT.
	expect := func() {
		mock.ExpectQuery(".").WillReturnRows(empty())
		mock.ExpectQuery(".").WillReturnRows(empty()).Maybe()
	}

	expect()
	_, _ = store.GetExecutionsByStatusesFiltered(ctx, ExecutionsByStatusesQuery{Statuses: []string{"pending"}})
	expect()
	_, _ = store.GetPlannedExecutions(ctx, []string{"scheduled"}, 10)
	expect()
	_, _ = store.GetStaleApprovedExecutions(ctx, time.Hour)
	expect()
	_, _ = store.ListStuckExecutions(ctx, []string{"approved"}, time.Hour)
	expect()
	_, _ = store.GetPendingExecutions(ctx)
	expect()
	_, _ = store.GetExecutionByID(ctx, "exec-1")
	expect()
	_, _ = store.GetExecutionByPlanAndDate(ctx, "plan-1", time.Now())
	expect()
	_, _ = store.GetScheduledExecutionsDue(ctx)
	actor := "11111111-1111-1111-1111-111111111111"
	expect()
	_, _ = store.TransitionExecutionStatus(ctx, "exec-1", []string{"pending"}, StatusCanceled, &actor)
	mock.ExpectBegin()
	mock.ExpectExec(".").WillReturnResult(pgxmock.NewResult("SELECT", 1))
	expect()
	tx, err := mock.Begin(ctx)
	require.NoError(t, err)
	_, _ = store.ListRecentSubmitsTx(ctx, tx, "creator", time.Now())

	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, len(projections), 10, "every purchase_executions projection must be exercised")
	assert.Empty(t, bare, "projections that do not read COALESCE(canceled_by, legacy actor)")
}

// TestPGXMock_ExecutionCancelActorWritesOnlyCanceledBy pins the write half:
// the save, the cancel transition and SetCancelledBy write canceled_by alone
// and never the legacy column (RETURNING projections still read it). The
// matcher rejects any INSERT or UPDATE statement whose write part names the legacy
// column, so each call fails against the dual-write queries.
//
// The save keeps an actor already on the row (COALESCE(existing, $17)): a save
// from a copy read before a concurrent cancel must not erase that actor, and a
// legacy-only actor read through COALESCE is carried into canceled_by.
func TestPGXMock_ExecutionCancelActorWritesOnlyCanceledBy(t *testing.T) {
	var dualWrites []string
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherFunc(func(expected, actual string) error {
		write, _, _ := strings.Cut(actual, "RETURNING")
		verb := strings.ToUpper(strings.Fields(write + " x")[0])
		if (verb == "INSERT" || verb == "UPDATE") && strings.Contains(write, legacyCancelActorColumn) {
			dualWrites = append(dualWrites, actual)
			return errors.New("write still names the legacy cancel actor column: " + actual)
		}
		return pgxmock.QueryMatcherRegexp.Match(expected, actual)
	})))
	require.NoError(t, err)
	store := storeWith(mock)
	ctx := context.Background()

	actor := "11111111-1111-1111-1111-111111111111"
	mock.ExpectQuery(`(?s)SET status = \$2.*canceled_by = COALESCE\(\(SELECT email FROM users WHERE id = \$5\), canceled_by\)\s+WHERE`).
		WithArgs(anyArgsCfg(7)...).
		WillReturnRows(pgxmock.NewRows(executionProjectionCols))
	// The empty result row makes the store probe the row afterwards; that
	// probe and its error are not under test.
	mock.ExpectQuery(`FROM purchase_executions`).WillReturnRows(pgxmock.NewRows(executionProjectionCols)).Maybe()
	_, _ = store.TransitionExecutionStatus(ctx, "exec-1", []string{"pending"}, StatusCanceled, &actor)
	require.Empty(t, dualWrites, "the cancel transition must write canceled_by only")

	mock.ExpectExec(`SET canceled_by = \$2, updated_at = NOW\(\)`).
		WithArgs("exec-1", "revoker@example.com").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, store.SetCancelledBy(ctx, "exec-1", "revoker@example.com"))

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)approved_by, canceled_by, capacity_percent.*canceled_by = COALESCE\(purchase_executions\.canceled_by, \$17\)`).
		WithArgs(anyArgsCfg(29)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	require.NoError(t, store.SavePurchaseExecution(ctx, &PurchaseExecution{
		ExecutionID: "exec-1", PlanID: "plan-1", Status: StatusCanceled, ScheduledDate: time.Now(),
	}))

	assert.NoError(t, mock.ExpectationsWereMet())
}
