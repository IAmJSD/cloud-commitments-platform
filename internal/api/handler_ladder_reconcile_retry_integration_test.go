//go:build integration

package api

// Real PostgreSQL: the ladder reconciler against the real Retry write path
// (persistRetrySuccessorTx -> LinkRetryExecutionAtomic), issue #118 PR-C2.

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FIXTURE: the provider layer listing.
type visibleNever struct{}

func (visibleNever) CommitmentVisible(context.Context, config.LadderTrancheDB, string) (bool, error) {
	return false, nil
}

// pausedChainStore holds the reconciler's transaction open right after it has
// locked the chain (tranche row + tail execution row), until release is closed.
type pausedChainStore struct {
	*config.PostgresStore
	locked  chan struct{}
	release chan struct{}
}

func (s *pausedChainStore) LockLadderTrancheChainTx(ctx context.Context, tx pgx.Tx, id string) ([]config.LadderChainNode, error) {
	chain, err := s.PostgresStore.LockLadderTrancheChainTx(ctx, tx, id)
	close(s.locked)
	<-s.release
	return chain, err
}

type ladderRetryRig struct {
	store      *config.PostgresStore
	trancheID  string
	failedExec *config.PurchaseExecution
}

func newLadderRetryRig(t *testing.T) *ladderRetryRig {
	pg, store := setupSubmitDB(t)
	ctx := context.Background()
	pool := pg.DB.Pool()
	acct := uuid.New().String()
	_, err := pool.Exec(ctx, `INSERT INTO cloud_accounts (id, name, enabled, provider, external_id, aws_is_org_root, created_at, updated_at)
		VALUES ($1, 'c2', true, 'aws', '123456789012', false, now(), now())`, acct)
	require.NoError(t, err)
	cfg, err := store.UpsertLadderConfig(ctx, &config.LadderConfigDB{CloudAccountID: acct, Provider: "aws", Enabled: true,
		Mode: "email_approval", Cadence: "daily", TargetCoverage: 80, BufferFraction: 0.1, BaselinePercentile: 5, LookbackDays: 30,
		BufferUtilizationThreshold: 50, MaxActionsPerRun: 5, RampSchedule: []byte(`{"steps":[{"after_days":0,"fraction":1.0}]}`)})
	require.NoError(t, err)
	tr := config.LadderTrancheDB{ID: uuid.New().String(), ConfigID: &cfg.ID, LayerType: ladder.LayerComputeSP,
		Term: ladder.Term1Year, PaymentOption: ladder.PaymentNoUpfront, Status: ladder.TrancheStatusScheduled,
		AmountUSDHr: 5, ScheduledDate: time.Now().Add(-time.Hour)}
	require.NoError(t, store.SaveLadderTranches(ctx, []config.LadderTrancheDB{tr}))
	var failedID string
	_, err = store.ClaimDueLadderTranches(ctx, cfg.ID, time.Now(), 1,
		func(ctx context.Context, tx pgx.Tx, _ config.LadderTrancheDB) (string, error) {
			insErr := tx.QueryRow(ctx, `INSERT INTO purchase_executions (scheduled_date, status, no_submission, error)
				VALUES (NOW(), 'failed', true, '') RETURNING execution_id`).Scan(&failedID)
			return failedID, insErr
		})
	require.NoError(t, err)
	failed, err := store.GetExecutionByID(ctx, failedID)
	require.NoError(t, err)
	return &ladderRetryRig{store: store, trancheID: tr.ID, failedExec: failed}
}

func (r *ladderRetryRig) trancheStatus(t *testing.T) string {
	var s string
	require.NoError(t, r.store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status FROM ladder_tranches WHERE id = $1`, r.trancheID).Scan(&s)
	}))
	return s
}

func (r *ladderRetryRig) successor() *config.PurchaseExecution {
	return &config.PurchaseExecution{Status: "pending", ScheduledDate: time.Now(), RetryAttemptN: 1}
}

// A retry that links first leaves the tranche fired: the reconciler sees the
// linked tail and does not conclude the chain is proven failed.
func TestLadderReconcile_RetryLinkedFirstKeepsTrancheFired(t *testing.T) {
	r := newLadderRetryRig(t)
	h := &Handler{config: r.store}
	require.NoError(t, h.persistRetrySuccessorTx(context.Background(), r.failedExec, r.successor(), nil))

	res, err := purchase.ReconcileLadderTranches(context.Background(), r.store, visibleNever{}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Unresolved)
	assert.Equal(t, "fired", r.trancheStatus(t))
}

// While the reconciler holds the tail row, the real Retry write cannot link a
// successor to it: persistRetrySuccessorTx blocks until the reconciler's
// transaction ends. Without the tail lock the retry would link mid-decision.
func TestLadderReconcile_TailLockBlocksRealRetryLink(t *testing.T) {
	r := newLadderRetryRig(t)
	ctx := context.Background()
	paused := &pausedChainStore{PostgresStore: r.store, locked: make(chan struct{}), release: make(chan struct{})}

	reconciled := make(chan error, 1)
	go func() {
		_, err := purchase.ReconcileLadderTranches(ctx, paused, visibleNever{}, time.Now())
		reconciled <- err
	}()
	<-paused.locked

	h := &Handler{config: r.store}
	retried := make(chan error, 1)
	go func() { retried <- h.persistRetrySuccessorTx(ctx, r.failedExec, r.successor(), nil) }()

	select {
	case err := <-retried:
		t.Fatalf("retry linked while the reconciler held the tail row (err=%v)", err)
	case <-time.After(1500 * time.Millisecond):
	}

	close(paused.release)
	require.NoError(t, <-reconciled)
	require.NoError(t, <-retried)
	assert.Equal(t, "failed", r.trancheStatus(t), "decided on the chain it locked")
}
