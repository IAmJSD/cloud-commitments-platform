//go:build integration

package api

// Real PostgreSQL: operator resolve x resolve x reconciler x real Retry link on
// one fired tranche (platform#118 PR5b). The resolve takes the tranche row and
// then the chain tail, the order Retry's link and the reconciler use, so
// exactly one writer wins and a failed tranche never has a live successor.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLadderResolve_RacesWithReconcileAndRetry(t *testing.T) {
	r := newLadderRetryRig(t)
	ctx := context.Background()
	h := &Handler{config: r.store}
	var accountID string
	require.NoError(t, r.store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT cloud_account_id::text FROM ladder_configs WHERE id = $1`, r.cfgID).Scan(&accountID)
	}))

	const rounds = 25
	var failedRounds, firedRounds, operatorWins, reconcilerWins int
	for round := 0; round < rounds; round++ {
		if round > 0 {
			r.addFiredTranche(t)
		}
		resolve := func() {
			_, err := r.store.ResolveLadderTrancheFailed(ctx, config.LadderResolveFailed{TrancheID: r.trancheID, AccountID: accountID,
				StatusReason: `operator:failed:proven: reason="race"`, Now: time.Now().UTC(), StaleAfter: purchase.LadderFiredStaleAfter})
			if err != nil {
				require.ErrorIs(t, err, config.ErrLadderResolveConflict)
			}
		}
		reconcile := func() {
			_, err := purchase.ReconcileLadderTranches(ctx, r.store, visibleNever{}, time.Now())
			require.NoError(t, err)
		}
		retry := func() {
			err := h.persistRetrySuccessorTx(ctx, r.failedExec, r.successor(), nil)
			if err != nil {
				ce, ok := IsClientError(err)
				require.True(t, ok, "retry failed unexpectedly: %v", err)
				assert.Equal(t, 409, ce.code)
			}
		}
		writers := []func(){resolve, resolve, reconcile, reconcile, retry, retry, retry, retry}
		runRaceRound(round, writers[:4], writers[4:])

		status := r.trancheStatus(t)
		linked := r.headLinked(t)
		switch status {
		case "failed":
			failedRounds++
			assert.False(t, linked, "round %d: a failed tranche must not have a live successor", round)
			reason := r.trancheReason(t)
			if strings.HasPrefix(reason, "operator:") {
				operatorWins++
			} else {
				assert.Equal(t, "no_submission", reason)
				reconcilerWins++
			}
		case "fired":
			firedRounds++
			assert.True(t, linked, "round %d: a tranche left fired must be the one Retry extended", round)
		default:
			t.Fatalf("round %d: unexpected tranche status %q", round, status)
		}
	}
	assert.Positive(t, failedRounds, "the resolve/reconcile side never won: the race was not exercised")
	assert.Positive(t, firedRounds, "Retry never won: the race was not exercised")
	assert.Positive(t, operatorWins, "the operator resolve never won a round")
	t.Logf("failed=%d (operator=%d reconciler=%d) fired-by-retry=%d", failedRounds, operatorWins, reconcilerWins, firedRounds)
}

// runRaceRound runs finishers (resolve, reconcile) against retries. Rounds
// cycle through three schedules so both orders happen: all at once, finishers
// head start, retries head start.
func runRaceRound(round int, finishers, retries []func()) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	launch := func(fns []func()) {
		for _, fn := range fns {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				fn()
			}()
		}
	}
	switch round % 3 {
	case 0:
		launch(finishers)
		launch(retries)
		close(start)
	case 1:
		launch(finishers)
		close(start)
		time.Sleep(150 * time.Millisecond)
		start = make(chan struct{})
		launch(retries)
		close(start)
	default:
		launch(retries)
		close(start)
		time.Sleep(150 * time.Millisecond)
		start = make(chan struct{})
		launch(finishers)
		close(start)
	}
	wg.Wait()
}

func (r *ladderRetryRig) headLinked(t *testing.T) bool {
	var linked bool
	require.NoError(t, r.store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT retry_execution_id IS NOT NULL FROM purchase_executions WHERE execution_id = $1`,
			r.failedExec.ExecutionID).Scan(&linked)
	}))
	return linked
}

func (r *ladderRetryRig) trancheReason(t *testing.T) string {
	var s string
	require.NoError(t, r.store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT COALESCE(status_reason, '') FROM ladder_tranches WHERE id = $1`, r.trancheID).Scan(&s)
	}))
	return s
}
