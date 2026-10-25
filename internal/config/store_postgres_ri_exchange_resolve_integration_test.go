//go:build integration

package config

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Real-PostgreSQL coverage for #692: a processing RI exchange left behind by a
// crashed run holds daily-cap headroom until an operator resolves it, and the
// resolve never releases a hold that a running writer may still settle. Uses the
// dedicated issue41_ database guard from newRIExchangeIntegrationEnv.

const resolveStaleAge = RIExchangeResolveStaleAfter + time.Hour

func (e *riExchangeIntegrationEnv) resolveReq(id, outcome string, row *RIExchangeRecord) RIExchangeResolve {
	return RIExchangeResolve{
		ID: id, Outcome: outcome, Note: "operator:" + outcome + ": verified source RIs in the console",
		Check:      RIExchangeCloudCheck{AccountID: row.AccountID, Region: row.Region, CheckedAt: time.Now().UTC().Add(-time.Minute)},
		StaleAfter: RIExchangeResolveStaleAfter,
	}
}

func (e *riExchangeIntegrationEnv) status(t *testing.T, id string) (status, note string, completedAt *time.Time) {
	t.Helper()
	var n *string
	require.NoError(t, e.poolA.QueryRow(e.ctx,
		`SELECT status, error, completed_at FROM ri_exchange_history WHERE id = $1`, id).Scan(&status, &n, &completedAt))
	if n != nil {
		note = *n
	}
	return status, note, completedAt
}

func autoCandidate(account int64) *RIExchangeRecord {
	return &RIExchangeRecord{AccountID: testAccountID(account), Region: "us-east-1", SourceRIIDs: []string{fmt.Sprintf("ri-resolve-%d", account)},
		SourceInstanceType: "m5.large", SourceCount: 1, TargetOfferingID: "offering-test", TargetInstanceType: "m6i.large", TargetCount: 1,
		PaymentDue: "1.000000", Status: "processing", Mode: "auto"}
}

// The issue's scenario: a crashed run leaves a $1000 hold that blocks every
// later exchange, on every later day. Resolving it as failed after the operator
// verified nothing happened at AWS frees the headroom.
func TestPostgresStoreDB_ResolveStaleRIExchange_FailedReleasesStrandedHold(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	now := time.Now().UTC()
	cap := env.capAbove(t, now)
	hold := env.processing(t, "auto", testAccountID(692000000001), 1000, now.Add(-resolveStaleAge))
	defer env.release(t, hold.ID)

	next := autoCandidate(692000000002)
	_, err := env.storeA.ReserveRIExchange(env.ctx, next, cap, "1000.000000")
	require.ErrorIs(t, err, ErrRIExchangeDailyCapExceeded, "the stranded hold must still block before it is resolved")
	require.Equal(t, 1, env.spend(t, now.Add(48*time.Hour)).Cmp(usdMicros(999)), "the hold still counts two days later")

	res, err := env.storeA.ResolveStaleRIExchange(env.ctx, env.resolveReq(hold.ID, StatusFailed, hold))
	require.NoError(t, err)
	require.False(t, res.Replayed)
	require.Equal(t, StatusFailed, res.Status)
	status, note, completedAt := env.status(t, hold.ID)
	require.Equal(t, StatusFailed, status)
	require.Equal(t, res.Note, note)
	require.Nil(t, completedAt)

	ceiling, err := env.storeA.ReserveRIExchange(env.ctx, next, cap, "1000.000000")
	require.NoError(t, err, "a released hold no longer counts")
	require.Equal(t, "1000.000000", ceiling)
	env.release(t, next.ID)
}

// Resolving as completed keeps the reserved amount on the books for today (it
// is an upper bound of what AWS accepted) and lets it drop off tomorrow like
// any settled exchange.
func TestPostgresStoreDB_ResolveStaleRIExchange_CompletedCountsTodayOnly(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	now := time.Now().UTC()
	tomorrow := now.Add(24 * time.Hour)
	before, beforeTomorrow := env.spend(t, now), env.spend(t, tomorrow)
	hold := env.processing(t, "manual", testAccountID(692000000011), 700, now.Add(-resolveStaleAge))
	defer env.release(t, hold.ID)

	req := env.resolveReq(hold.ID, StatusCompleted, hold)
	req.ExchangeID = "riex-found-in-console"
	res, err := env.storeA.ResolveStaleRIExchange(env.ctx, req)
	require.NoError(t, err)
	require.Equal(t, "700.000000", res.PaymentDue)
	status, _, completedAt := env.status(t, hold.ID)
	require.Equal(t, StatusCompleted, status)
	require.NotNil(t, completedAt)
	var exchangeID string
	require.NoError(t, env.poolA.QueryRow(env.ctx, `SELECT exchange_id FROM ri_exchange_history WHERE id = $1`, hold.ID).Scan(&exchangeID))
	require.Equal(t, "riex-found-in-console", exchangeID)

	require.Zero(t, env.spend(t, now).Cmp(new(big.Int).Add(before, usdMicros(700))), "a completed resolve still counts today")
	require.Zero(t, env.spend(t, tomorrow).Cmp(beforeTomorrow), "and only today")
}

// A row that changed inside the in-flight window may still be settled by its
// writer, so it is never resolved, whatever the operator says.
func TestPostgresStoreDB_ResolveStaleRIExchange_RefusesRowThatMayBeInFlight(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	now := time.Now().UTC()
	hold := env.processing(t, "auto", testAccountID(692000000021), 10, now.Add(-RIExchangeResolveStaleAfter+time.Minute))
	defer env.release(t, hold.ID)

	for _, outcome := range []string{StatusFailed, StatusCompleted} {
		_, err := env.storeA.ResolveStaleRIExchange(env.ctx, env.resolveReq(hold.ID, outcome, hold))
		require.ErrorIs(t, err, ErrRIExchangeResolveConflict, outcome)
		status, _, _ := env.status(t, hold.ID)
		require.Equal(t, "processing", status)
	}
}

// AWS returned an exchange id, so the exchange happened: it can be settled as
// completed but never released.
func TestPostgresStoreDB_ResolveStaleRIExchange_RefusesToReleaseRowWithExchangeID(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	now := time.Now().UTC()
	hold := env.processing(t, "auto", testAccountID(692000000031), 10, now)
	_, err := env.poolA.Exec(env.ctx, `UPDATE ri_exchange_history SET exchange_id = 'riex-accepted' WHERE id = $1`, hold.ID)
	require.NoError(t, err)
	env.setUpdatedAt(t, hold.ID, now.Add(-resolveStaleAge))
	defer env.release(t, hold.ID)

	_, err = env.storeA.ResolveStaleRIExchange(env.ctx, env.resolveReq(hold.ID, StatusFailed, hold))
	require.ErrorIs(t, err, ErrRIExchangeResolveConflict)
	status, _, _ := env.status(t, hold.ID)
	require.Equal(t, "processing", status)

	mismatch := env.resolveReq(hold.ID, StatusCompleted, hold)
	mismatch.ExchangeID = "riex-other"
	_, err = env.storeA.ResolveStaleRIExchange(env.ctx, mismatch)
	require.ErrorIs(t, err, ErrRIExchangeResolveInvalid)

	_, err = env.storeA.ResolveStaleRIExchange(env.ctx, env.resolveReq(hold.ID, StatusCompleted, hold))
	require.NoError(t, err)
}

func TestPostgresStoreDB_ResolveStaleRIExchange_ValidatesCloudCheck(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	now := time.Now().UTC()
	updated := now.Add(-resolveStaleAge)
	hold := env.processing(t, "auto", testAccountID(692000000041), 10, updated)
	defer env.release(t, hold.ID)

	cases := map[string]func(*RIExchangeResolve){
		"other account":           func(r *RIExchangeResolve) { r.Check.AccountID = testAccountID(692000000042) },
		"other region":            func(r *RIExchangeResolve) { r.Check.Region = "eu-west-1" },
		"checked before change":   func(r *RIExchangeResolve) { r.Check.CheckedAt = updated.Add(-time.Minute) },
		"checked at the change":   func(r *RIExchangeResolve) { r.Check.CheckedAt = updated },
		"checked in the future":   func(r *RIExchangeResolve) { r.Check.CheckedAt = now.Add(time.Hour) },
		"failed with exchange id": func(r *RIExchangeResolve) { r.ExchangeID = "riex-x" },
		"unknown outcome":         func(r *RIExchangeResolve) { r.Outcome = "canceled" },
		"empty note":              func(r *RIExchangeResolve) { r.Note = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := env.resolveReq(hold.ID, StatusFailed, hold)
			mutate(&req)
			_, err := env.storeA.ResolveStaleRIExchange(env.ctx, req)
			require.ErrorIs(t, err, ErrRIExchangeResolveInvalid)
			status, _, _ := env.status(t, hold.ID)
			require.Equal(t, "processing", status)
		})
	}

	_, err := env.storeA.ResolveStaleRIExchange(env.ctx, env.resolveReq("00000000-0000-4000-8000-000000000692", StatusFailed, hold))
	require.ErrorIs(t, err, ErrRIExchangeResolveNotFound)
}

// An identical retry answers success without writing; a different request
// against an already-resolved row is a conflict.
func TestPostgresStoreDB_ResolveStaleRIExchange_ReplayAndConflict(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	hold := env.processing(t, "auto", testAccountID(692000000051), 10, time.Now().UTC().Add(-resolveStaleAge))
	defer env.release(t, hold.ID)

	req := env.resolveReq(hold.ID, StatusFailed, hold)
	_, err := env.storeA.ResolveStaleRIExchange(env.ctx, req)
	require.NoError(t, err)
	again, err := env.storeA.ResolveStaleRIExchange(env.ctx, req)
	require.NoError(t, err)
	require.True(t, again.Replayed)

	_, err = env.storeA.ResolveStaleRIExchange(env.ctx, env.resolveReq(hold.ID, StatusCompleted, hold))
	require.ErrorIs(t, err, ErrRIExchangeResolveConflict)
	other := req
	other.Note = "operator:failed: a different reason"
	_, err = env.storeA.ResolveStaleRIExchange(env.ctx, other)
	require.ErrorIs(t, err, ErrRIExchangeResolveConflict)
}

// A late settlement and an operator release racing on the same row must end
// with the exchange counted, in either lock order: a release that locks first
// is overwritten by the completion queued behind it, and a release queued
// behind the completion sees the row completed and refuses. A third
// transaction holds the row lock so both calls are provably parked, and the
// order they queue in is the order PostgreSQL grants the row lock.
func TestPostgresStoreDB_ResolveStaleRIExchange_RaceWithLateSettlementKeepsHold(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	for _, resolveFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("resolveFirst=%t", resolveFirst), func(t *testing.T) {
			hold := env.processing(t, "auto", testAccountID(692000000061), 400, time.Now().UTC().Add(-resolveStaleAge))
			defer env.release(t, hold.ID)
			lockTx, err := env.poolA.Begin(env.ctx)
			require.NoError(t, err)
			defer lockTx.Rollback(env.ctx)
			_, err = lockTx.Exec(env.ctx, `SELECT 1 FROM ri_exchange_history WHERE id = $1 FOR UPDATE`, hold.ID)
			require.NoError(t, err)

			var wg sync.WaitGroup
			var resolveErr, completeErr error
			resolve := func() {
				defer wg.Done()
				_, resolveErr = env.storeA.ResolveStaleRIExchange(env.ctx, env.resolveReq(hold.ID, StatusFailed, hold))
			}
			complete := func() {
				defer wg.Done()
				completeErr = env.storeB.CompleteRIExchangeWithPayment(env.ctx, hold.ID, "riex-late", "400.000000")
			}
			first, second := resolve, complete
			if !resolveFirst {
				first, second = complete, resolve
			}
			wg.Add(2)
			go first()
			env.waitForLockWaiters(t, 1)
			go second()
			env.waitForLockWaiters(t, 2)
			require.NoError(t, lockTx.Rollback(env.ctx))
			wg.Wait()

			require.NoError(t, completeErr)
			if resolveFirst {
				require.NoError(t, resolveErr)
			} else {
				require.ErrorIs(t, resolveErr, ErrRIExchangeResolveConflict, "a release queued behind the settlement must refuse")
			}
			status, _, completedAt := env.status(t, hold.ID)
			require.Equal(t, StatusCompleted, status, "the late settlement must win")
			require.NotNil(t, completedAt)
		})
	}
}

// Two operators resolving the same row at once: exactly one decision lands.
func TestPostgresStoreDB_ResolveStaleRIExchange_ConcurrentResolvesOneWins(t *testing.T) {
	env := newRIExchangeIntegrationEnv(t)
	hold := env.processing(t, "auto", testAccountID(692000000071), 10, time.Now().UTC().Add(-resolveStaleAge))
	defer env.release(t, hold.ID)
	lockTx, err := env.poolA.Begin(env.ctx)
	require.NoError(t, err)
	_, err = lockTx.Exec(env.ctx, `SELECT 1 FROM ri_exchange_history WHERE id = $1 FOR UPDATE`, hold.ID)
	require.NoError(t, err)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, run := range []struct {
		store   *PostgresStore
		outcome string
	}{{env.storeA, StatusFailed}, {env.storeB, StatusCompleted}} {
		wg.Add(1)
		go func(i int, store *PostgresStore, outcome string) {
			defer wg.Done()
			_, errs[i] = store.ResolveStaleRIExchange(env.ctx, env.resolveReq(hold.ID, outcome, hold))
		}(i, run.store, run.outcome)
	}
	env.waitForLockWaiters(t, 2)
	require.NoError(t, lockTx.Rollback(env.ctx))
	wg.Wait()

	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		require.True(t, errors.Is(err, ErrRIExchangeResolveConflict), "loser must see a conflict: %v", err)
	}
	require.Equal(t, 1, wins)
}

// waitForLockWaiters waits until n backends are blocked on a row lock.
func (e *riExchangeIntegrationEnv) waitForLockWaiters(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		err := e.poolA.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		return err == nil && waiting >= n
	}, 10*time.Second, 10*time.Millisecond, "both calls must be parked on the row lock")
}
