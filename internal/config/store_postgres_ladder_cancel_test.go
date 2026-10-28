package config

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
)

// Regression tests for issue #542: the ladder_runs canceled-by audit column was
// never assigned, so a canceled ladder run carried no record of who canceled it.

//nolint:misspell // SQL column names, not prose
var ladderRunMockColumns = []string{
	"id", "config_id", "started_at", "completed_at", "status", "mode", "cadence",
	"baseline_usd_hr", "target_usd_hr", "existing_usd_hr", "gap_usd_hr",
	"plan", "total_hourly_commit", "total_upfront_cost", "estimated_savings",
	"approval_token_hash", "approval_token_expires_at",
	"approved_by", "cancelled_by", "fire_at",
	"created_at", "updated_at",
}

func newLadderCancelMockStore(t *testing.T) (*PostgresStore, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return &PostgresStore{db: pool}, pool
}

func TestCancelLadderRun_StampsCancelledBy(t *testing.T) {
	store, pool := newLadderCancelMockStore(t)
	now := time.Now().UTC()
	actor := "alice@example.com"
	pool.ExpectQuery(regexp.QuoteMeta(ladderRunCancelQuery)).
		WithArgs("run-1", string(ladder.RunStatusCancelled), []string{"awaiting_approval", "approved"}, actor).
		WillReturnRows(pgxmock.NewRows(ladderRunMockColumns).AddRow(
			"run-1", nil, now, nil, ladder.RunStatusCancelled, nil, nil,
			nil, nil, nil, nil,
			[]byte(`{}`), 0.0, 0.0, 0.0,
			nil, nil,
			nil, &actor, nil,
			now, now,
		))

	run, err := store.CancelLadderRun(context.Background(), "run-1",
		[]ladder.RunStatus{ladder.RunStatusAwaitingApproval, ladder.RunStatusApproved}, "  "+actor+" ")
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, ladder.RunStatusCancelled, run.Status)
	require.NotNil(t, run.CancelledBy, "a canceled run must record who canceled it")
	assert.Equal(t, actor, *run.CancelledBy)
	require.NoError(t, pool.ExpectationsWereMet())
}

func TestCancelLadderRun_CASLostReturnsNil(t *testing.T) {
	store, pool := newLadderCancelMockStore(t)
	pool.ExpectQuery(regexp.QuoteMeta(ladderRunCancelQuery)).
		WithArgs("run-1", string(ladder.RunStatusCancelled), []string{"approved"}, "bob@example.com").
		WillReturnRows(pgxmock.NewRows(ladderRunMockColumns))

	run, err := store.CancelLadderRun(context.Background(), "run-1",
		[]ladder.RunStatus{ladder.RunStatusApproved}, "bob@example.com")
	require.NoError(t, err)
	assert.Nil(t, run, "a lost CAS must return (nil, nil)")
	require.NoError(t, pool.ExpectationsWereMet())
}

func TestCancelLadderRun_RequiresActor(t *testing.T) {
	for _, actor := range []string{"", "   "} {
		store, pool := newLadderCancelMockStore(t)
		run, err := store.CancelLadderRun(context.Background(), "run-1",
			[]ladder.RunStatus{ladder.RunStatusApproved}, actor)
		require.ErrorIs(t, err, ErrLadderRunCancelActorRequired)
		assert.Nil(t, run)
		require.NoError(t, pool.ExpectationsWereMet(), "no UPDATE may run without an actor")
	}
}

func TestTransitionLadderRunStatus_RefusesCancel(t *testing.T) {
	store, pool := newLadderCancelMockStore(t)
	run, err := store.TransitionLadderRunStatus(context.Background(), "run-1",
		[]ladder.RunStatus{ladder.RunStatusExecuting}, ladder.RunStatusCancelled)
	require.ErrorIs(t, err, ErrLadderRunCancelViaTransition)
	assert.Nil(t, run)
	require.NoError(t, pool.ExpectationsWereMet(), "a cancel must not bypass the canceled-by stamp")
}
