//go:build integration

package config

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestLadderAmendmentAuditAndSiblingConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfig(ctx, t, store)
	var accountID string
	require.NoError(t, store.db.QueryRow(ctx, "SELECT cloud_account_id FROM ladder_configs WHERE id=$1", cfgID).Scan(&accountID))
	run, err := store.SaveLadderRun(ctx, &LadderRunDB{ConfigID: &cfgID, Status: ladder.RunStatusPlanned, StartedAt: time.Now(), TotalHourlyCommit: 10, TargetUSDHr: fptr(10), ExistingUSDHr: fptr(0), BaselineUSDHr: fptr(100), Plan: []byte(`{"original":true}`)})
	require.NoError(t, err)
	date := time.Now().UTC().Add(time.Hour)
	ids := []string{uuid.NewString(), uuid.NewString()}
	tranches := make([]LadderTrancheDB, 2)
	for i, id := range ids {
		tranches[i] = LadderTrancheDB{ID: id, ConfigID: &cfgID, RunID: &run.ID, LayerType: ladder.LayerComputeSP, Term: ladder.Term1Year, PaymentOption: ladder.PaymentNoUpfront, Status: ladder.TrancheStatusScheduled, AmountUSDHr: 4, ScheduledDate: date}
	}
	require.NoError(t, store.SaveLadderTranches(ctx, tranches))
	zero := int64(0)
	amendment := LadderAmendment{ExpectedRevision: &zero, ScheduledDate: date.Add(time.Hour), AmountUSDHr: "6.000000"}
	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, id := range ids {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, amendErr := store.AmendLadderTranche(ctx, id, accountID, "aws", "actor", amendment)
			errors <- amendErr
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	successes, rejected := 0, 0
	for amendErr := range errors {
		if amendErr == nil {
			successes++
		} else {
			require.ErrorIs(t, amendErr, ErrLadderAmendInvalid)
			rejected++
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, rejected)
	var sum string
	require.NoError(t, store.db.QueryRow(ctx, "SELECT SUM(amount_usd_hr)::text FROM ladder_tranches WHERE run_id=$1", run.ID).Scan(&sum))
	require.Equal(t, "10.000000", sum)
	var actor, oldAmount, newAmount, changedID string
	var previous, next int64
	var previousDate, nextDate time.Time
	require.NoError(t, store.db.QueryRow(ctx, `SELECT tranche_id,actor,previous_amount_usd_hr::text,new_amount_usd_hr::text,previous_revision,new_revision,previous_scheduled_date,new_scheduled_date
 FROM ladder_tranche_amendments`).Scan(&changedID, &actor, &oldAmount, &newAmount, &previous, &next, &previousDate, &nextDate))
	require.Equal(t, "actor", actor)
	require.Equal(t, "4.000000", oldAmount)
	require.Equal(t, "6.000000", newAmount)
	require.Equal(t, int64(0), previous)
	require.Equal(t, int64(1), next)
	require.Equal(t, date.Truncate(time.Microsecond), previousDate.UTC())
	require.Equal(t, date.Add(time.Hour).Truncate(time.Microsecond), nextDate.UTC())
	_, err = store.AmendLadderTranche(ctx, changedID, accountID, "aws", "actor", amendment)
	require.ErrorIs(t, err, ErrLadderAmendConflict)
	_, err = store.AmendLadderTranche(ctx, changedID, uuid.NewString(), "aws", "actor", amendment)
	require.ErrorIs(t, err, ErrLadderAmendNotFound)
	_, err = store.AmendLadderTranche(ctx, changedID, accountID, "azure", "actor", amendment)
	require.ErrorIs(t, err, ErrLadderAmendNotFound)
	one := int64(1)
	amendment.ExpectedRevision = &one
	amendment.AmountUSDHr = "7.000000"
	_, err = store.db.Exec(ctx, "UPDATE ladder_configs SET max_hourly_commit_per_run=8 WHERE id=$1", cfgID)
	require.NoError(t, err)
	_, err = store.AmendLadderTranche(ctx, changedID, accountID, "aws", "actor", amendment)
	require.ErrorIs(t, err, ErrLadderAmendInvalid)
	amendment.AmountUSDHr = "5.000000"
	reduced, err := store.AmendLadderTranche(ctx, changedID, accountID, "aws", "actor", amendment)
	require.NoError(t, err, "a reduction must be allowed even when the run total still exceeds a later-lowered cap")
	require.Equal(t, "9.000000", reduced.RunTotalUSDHr)
	_, err = store.db.Exec(ctx, "UPDATE ladder_runs SET status=$2 WHERE id=$1", run.ID, string(ladder.RunStatusAwaitingApproval))
	require.NoError(t, err)
	_, err = store.AmendLadderTranche(ctx, changedID, accountID, "aws", "actor", amendment)
	require.ErrorIs(t, err, ErrLadderAmendConflict)
	var count int
	require.NoError(t, store.db.QueryRow(ctx, "SELECT COUNT(*) FROM ladder_tranche_amendments").Scan(&count))
	require.Equal(t, 2, count)
	original, err := store.GetLadderRun(ctx, run.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"original":true}`, string(original.Plan))
}

func TestLadderAmendmentValidation(t *testing.T) {
	zero := int64(0)
	for _, amount := range []string{"0", "-1", "NaN", "Infinity", "1e2", "0.0000001", "100000000000000", "1/2"} {
		require.Error(t, (LadderAmendment{ExpectedRevision: &zero, ScheduledDate: time.Now(), AmountUSDHr: amount}).Validate(), amount)
	}
	require.NoError(t, (LadderAmendment{ExpectedRevision: &zero, ScheduledDate: time.Now(), AmountUSDHr: "0.123456"}).Validate())
	require.Error(t, (LadderAmendment{ScheduledDate: time.Now(), AmountUSDHr: "1"}).Validate())
}

func TestLadderAmendmentDeadlineWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfig(ctx, t, store)
	var accountID string
	require.NoError(t, store.db.QueryRow(ctx, "SELECT cloud_account_id FROM ladder_configs WHERE id=$1", cfgID).Scan(&accountID))
	run, err := store.SaveLadderRun(ctx, &LadderRunDB{ConfigID: &cfgID, Status: ladder.RunStatusPlanned, StartedAt: time.Now(), TotalHourlyCommit: 1})
	require.NoError(t, err)
	id := uuid.NewString()
	var date time.Time
	require.NoError(t, store.db.QueryRow(ctx, "SELECT clock_timestamp()+INTERVAL '5 seconds'").Scan(&date))
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{{ID: id, ConfigID: &cfgID, RunID: &run.ID, LayerType: ladder.LayerComputeSP, Term: ladder.Term1Year, PaymentOption: ladder.PaymentNoUpfront, Status: ladder.TrancheStatusScheduled, AmountUSDHr: 1, ScheduledDate: date}}))
	tx, err := store.db.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, "SELECT id FROM ladder_tranches WHERE id=$1 FOR UPDATE", id)
	require.NoError(t, err)
	zero := int64(0)
	finished := make(chan error, 1)
	go func() {
		_, amendErr := store.AmendLadderTranche(ctx, id, accountID, "aws", "actor", LadderAmendment{ExpectedRevision: &zero, ScheduledDate: date.Add(time.Hour), AmountUSDHr: "0.5"})
		finished <- amendErr
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := store.db.QueryRow(ctx, `SELECT clock_timestamp()<$1::timestamptz AND EXISTS(SELECT 1 FROM pg_stat_activity
	 WHERE wait_event_type='Lock' AND query LIKE '%scheduled_date>clock_timestamp()%FOR UPDATE%')`, date).Scan(&waiting)
		return queryErr == nil && waiting
	}, time.Second, 10*time.Millisecond, "amendment must reach the tranche lock before its deadline")
	_, err = tx.Exec(ctx, "SELECT pg_sleep(GREATEST(EXTRACT(EPOCH FROM $1::timestamptz-clock_timestamp()),0)+0.1)", date)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	require.ErrorIs(t, <-finished, ErrLadderAmendConflict)
	var revision, auditCount int
	require.NoError(t, store.db.QueryRow(ctx, "SELECT revision FROM ladder_tranches WHERE id=$1", id).Scan(&revision))
	require.Zero(t, revision)
	require.NoError(t, store.db.QueryRow(ctx, "SELECT COUNT(*) FROM ladder_tranche_amendments").Scan(&auditCount))
	require.Zero(t, auditCount)
}
