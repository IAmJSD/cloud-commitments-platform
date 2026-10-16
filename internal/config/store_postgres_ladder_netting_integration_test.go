//go:build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestLadderNetting_MixedStoredBasis(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	cfg := seedLadderConfig(ctx, t, store)
	od, native, rate := LadderAmountOnDemand, LadderAmountCommitment, "1/2"
	rows := []LadderTrancheDB{
		lifecycleTranche(cfg, 15, ladder.TrancheStatusScheduled, lockTestAsOf.Add(time.Hour)),
		lifecycleTranche(cfg, 20, ladder.TrancheStatusFired, lockTestAsOf.Add(-time.Hour)),
	}
	rows[0].AmountBasis = &od
	rows[1].AmountBasis, rows[1].CommitmentRate = &native, &rate
	require.NoError(t, store.SaveLadderTranches(ctx, rows))
	_, err := store.PlanAndSaveLadderRun(ctx, cfg, lockTestAsOf, func(rows []LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		require.Equal(t, 55.0, mustOnDemand(t, rows))
		return lockTestRun(cfg), nil, nil
	})
	require.NoError(t, err)
	var amount, storedRate string
	require.NoError(t, store.db.QueryRow(ctx, "SELECT amount_usd_hr::text,commitment_rate FROM ladder_tranches WHERE id=$1", rows[1].ID).Scan(&amount, &storedRate))
	require.Equal(t, "20.000000", amount)
	require.Equal(t, rate, storedRate)
}

func TestLadderNetting_UnknownRefusesBeforeBuilder(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	cfg := seedLadderConfig(ctx, t, store)
	row := lifecycleTranche(cfg, 5, ladder.TrancheStatusFired, lockTestAsOf)
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{row}))
	built := false
	_, err := store.PlanAndSaveLadderRun(ctx, cfg, lockTestAsOf, func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		built = true
		return lockTestRun(cfg), nil, nil
	})
	require.Error(t, err)
	require.False(t, built)
	require.Zero(t, countRuns(ctx, t, store, cfg))
	_, err = store.db.Exec(ctx, "UPDATE ladder_tranches SET amount_basis='commitment',commitment_rate='bad' WHERE id=$1", row.ID)
	require.NoError(t, err)
	_, err = store.PlanAndSaveLadderRun(ctx, cfg, lockTestAsOf, func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		built = true
		return lockTestRun(cfg), nil, nil
	})
	require.Error(t, err)
	require.False(t, built)
	require.Zero(t, countRuns(ctx, t, store, cfg))
}

func TestLadderNetting_SelectionBoundaries(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	cfg := seedLadderConfig(ctx, t, store)
	od, native := LadderAmountOnDemand, LadderAmountCommitment
	cutoff, anchor := ladderGraceCutoff(lockTestAsOf), ladderSettleAnchor(lockTestAsOf)
	before, after := anchor.Add(-time.Microsecond), anchor.Add(time.Microsecond)
	add := func(status ladder.TrancheStatus, amount float64, at time.Time, settled *time.Time, rate string) {
		tr := lifecycleTranche(cfg, amount, status, at)
		tr.AmountBasis, tr.SettledAt = &od, settled
		if status == ladder.TrancheStatusCompleted {
			tr.LayerType = ladder.LayerEC2InstanceSP
		}
		if rate != "" {
			tr.AmountBasis, tr.CommitmentRate = &native, &rate
		}
		require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))
	}
	add(ladder.TrancheStatusFired, 1, lockTestAsOf.Add(-24*time.Hour), nil, "1/2")
	add(ladder.TrancheStatusScheduled, 2, cutoff.Add(time.Microsecond), nil, "")
	add(ladder.TrancheStatusScheduled, 4, cutoff, nil, "")
	add(ladder.TrancheStatusScheduled, 8, cutoff.Add(-time.Microsecond), nil, "")
	add(ladder.TrancheStatusCompleted, 16, lockTestAsOf, &after, "1/4")
	add(ladder.TrancheStatusCompleted, 32, lockTestAsOf, &anchor, "")
	add(ladder.TrancheStatusCompleted, 64, lockTestAsOf, &before, "")
	add(ladder.TrancheStatusFailed, 128, lockTestAsOf, nil, "")
	add(ladder.TrancheStatusCancelled, 256, lockTestAsOf, nil, "")
	other := seedLadderConfigWithExtID(ctx, t, store, "fixture-other")
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{lifecycleTranche(other, 512, ladder.TrancheStatusFired, lockTestAsOf)}))
	_, err := store.PlanAndSaveLadderRun(ctx, cfg, lockTestAsOf, func(rows []LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		require.Equal(t, 68.0, mustOnDemand(t, rows))
		return lockTestRun(cfg), nil, nil
	})
	require.NoError(t, err)
	var overdue int
	require.NoError(t, store.db.QueryRow(ctx, "SELECT count(*) FROM ladder_tranches WHERE config_id=$1 AND status='cancelled' AND amount_usd_hr IN (4,8)", cfg).Scan(&overdue))
	require.Equal(t, 2, overdue)
}

type nettingHookDB struct {
	dbConn
	hook func()
}
type nettingHookTx struct {
	pgx.Tx
	hook  func()
	reads int
}

func (db nettingHookDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.dbConn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &nettingHookTx{Tx: tx, hook: db.hook}, nil
}
func (tx *nettingHookTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.reads++
	if tx.reads == 2 {
		tx.hook()
	}
	return tx.Tx.Query(ctx, sql, args...)
}

func TestLadderNetting_CompletionBetweenReadsCountsTwice(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	cfg := seedLadderConfig(ctx, t, store)
	native, rate := LadderAmountCommitment, "1/3"
	tr := lifecycleTranche(cfg, 1, ladder.TrancheStatusFired, lockTestAsOf.Add(-time.Hour))
	tr.AmountBasis, tr.CommitmentRate = &native, &rate
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))
	original := store.db
	transitioned := false
	store.db = nettingHookDB{dbConn: original, hook: func() {
		_, err := original.Exec(ctx, "UPDATE ladder_tranches SET status='completed',settled_at=$2 WHERE id=$1", tr.ID, lockTestAsOf)
		require.NoError(t, err)
		transitioned = true
	}}
	_, err := store.PlanAndSaveLadderRun(ctx, cfg, lockTestAsOf, func(rows []LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		require.True(t, transitioned)
		require.Equal(t, 6.0, mustOnDemand(t, rows))
		return lockTestRun(cfg), nil, nil
	})
	require.NoError(t, err)
}

func TestLadderNetting_ReadFailureDoesNotBecomeZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfg := seedLadderConfig(ctx, t, store)
	original := store.db
	store.db = nettingHookDB{dbConn: original, hook: cancel}
	built := false
	_, err := store.PlanAndSaveLadderRun(ctx, cfg, lockTestAsOf, func([]LadderInFlightRow) (*LadderRunDB, []LadderTrancheDB, error) {
		built = true
		return lockTestRun(cfg), nil, nil
	})
	require.Error(t, err)
	require.False(t, built)
	require.Zero(t, countRuns(context.Background(), t, store, cfg))
}
