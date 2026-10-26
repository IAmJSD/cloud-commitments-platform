//go:build integration

package config

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The RI exchange approve and reject links answer an expired exchange with
// 410 and an exchange another writer already moved with 409 (issue #587).
// Both codes come from the sentinel errors TransitionRIExchangeStatus wraps
// when its compare-and-set matches no row, so these tests pin them on a real
// PostgreSQL: the expiry predicate and the row lock that serializes
// concurrent transitions only exist in the database.

func saveTransitionTestExchange(ctx context.Context, t *testing.T, store *PostgresStore, expiresAt time.Time) string {
	t.Helper()
	record := &RIExchangeRecord{
		AccountID:          "123456789012",
		ExchangeID:         "transition-test-exchange",
		Region:             "us-east-1",
		SourceRIIDs:        []string{"ri-1"},
		SourceInstanceType: "r5.large",
		SourceCount:        1,
		TargetOfferingID:   "off-1",
		TargetInstanceType: "r5.xlarge",
		TargetCount:        1,
		Status:             "pending",
		Mode:               "manual",
		ExpiresAt:          &expiresAt,
	}
	require.NoError(t, store.SaveRIExchangeRecord(ctx, record))
	return record.ID
}

func TestPostgresStoreDB_TransitionRIExchangeStatus_ExpiredIsErrRIExchangeExpired(t *testing.T) {
	conn := setupTestContainerDB(t)
	store := NewPostgresStore(conn)
	ctx := context.Background()

	id := saveTransitionTestExchange(ctx, t, store, time.Now().Add(-time.Hour))

	for _, to := range []string{"processing", StatusCanceled} {
		got, err := store.TransitionRIExchangeStatus(ctx, id, "pending", to, nil)
		require.Error(t, err, "an expired exchange must not transition to %s", to)
		assert.Nil(t, got)
		assert.ErrorIs(t, err, ErrRIExchangeExpired)
		assert.NotErrorIs(t, err, ErrRIExchangeNotInExpectedStatus)
	}

	record, err := store.GetRIExchangeRecord(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "pending", record.Status, "a refused transition must leave the row alone")
}

// TestPostgresStoreDB_TransitionRIExchangeStatus_ConcurrentLosersGetNotInExpectedStatus
// races approvals (pending -> processing) against rejections (pending ->
// canceled) on one unexpired row. Exactly one wins; every loser must get
// ErrRIExchangeNotInExpectedStatus (409), never a bare error (500) or the
// expiry sentinel (410).
func TestPostgresStoreDB_TransitionRIExchangeStatus_ConcurrentLosersGetNotInExpectedStatus(t *testing.T) {
	conn := setupTestContainerDB(t)
	store := NewPostgresStore(conn)
	ctx := context.Background()

	const callers = 8
	for round := 0; round < 5; round++ {
		id := saveTransitionTestExchange(ctx, t, store, time.Now().Add(time.Hour))

		start := make(chan struct{})
		type outcome struct {
			to  string
			rec *RIExchangeRecord
			err error
		}
		results := make(chan outcome, callers)
		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			to := "processing"
			if i%2 == 1 {
				to = StatusCanceled
			}
			wg.Add(1)
			go func(to string) {
				defer wg.Done()
				<-start
				rec, err := store.TransitionRIExchangeStatus(ctx, id, "pending", to, nil)
				results <- outcome{to: to, rec: rec, err: err}
			}(to)
		}
		close(start)
		wg.Wait()
		close(results)

		var winners []string
		for r := range results {
			if r.err == nil {
				require.NotNil(t, r.rec)
				winners = append(winners, r.to)
				continue
			}
			assert.ErrorIs(t, r.err, ErrRIExchangeNotInExpectedStatus, "round %d: a lost race must map to 409", round)
			assert.NotErrorIs(t, r.err, ErrRIExchangeExpired, "round %d", round)
		}
		require.Len(t, winners, 1, "round %d: exactly one transition must win", round)

		record, err := store.GetRIExchangeRecord(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, winners[0], record.Status, "round %d", round)
	}
}
