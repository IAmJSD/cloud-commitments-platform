//go:build integration

package api

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retrySubmitRound seeds one failed web execution owned by creator, then runs
// a retry of it and a fresh identical submit by the same creator
// concurrently. The side named by retryLeads starts first and the other one
// starts lag later. It returns both results once both have finished: the
// submit's duplicate (nil when it inserted), the retry error and the submit
// error.
func retrySubmitRound(t *testing.T, pg *testhelpers.PostgresContainer, h *Handler, creator string, retryLeads bool, lag time.Duration) (submitDup *config.PurchaseExecution, retryErr, submitErr error) {
	t.Helper()
	ctx := context.Background()
	_, err := pg.DB.Pool().Exec(ctx, `TRUNCATE purchase_executions CASCADE`)
	require.NoError(t, err)
	recs, _ := submitFixture(t)
	failed := &config.PurchaseExecution{
		ExecutionID: uuid.NewString(), IdempotencyKey: uuid.NewString(), Status: config.StatusFailed,
		ScheduledDate: time.Now().Add(-time.Hour), Recommendations: recs, Source: common.PurchaseSourceWeb,
		CapacityPercent: 100, CreatedByUserID: &creator,
	}
	require.NoError(t, h.config.SavePurchaseExecution(ctx, failed))

	fresh := newSubmitExecution(t, recs)
	fresh.CreatedByUserID = &creator
	key := mustPurchaseIdempotencyKey(creator, recs, 100)

	retryLag, submitLag := lag, time.Duration(0)
	if retryLeads {
		retryLag, submitLag = 0, lag
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(retryLag)
		_, _, retryErr = h.persistRetryExecution(ctx, failed, &Session{UserID: creator}, 0, 0)
	}()
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(submitLag)
		submitDup, submitErr = h.persistExecutionAndSuppressions(ctx, fresh, nil, creator, key)
	}()
	close(start)
	wg.Wait()
	return submitDup, retryErr, submitErr
}

func requireNoDeadlock(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		require.NotEqual(t, "40P01", pgErr.Code, "deadlock between the retry and the submit: %v", err)
	}
}

func countPendingRows(t *testing.T, pg *testhelpers.PostgresContainer) int {
	t.Helper()
	var n int
	require.NoError(t, pg.DB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM purchase_executions WHERE status = 'pending'`).Scan(&n))
	return n
}

// TestRetrySuccessorRacesFreshSubmit is issue #760: a retry of a failed
// execution writes a web-source pending row for the retrying user and the
// same recommendations, so it must serialize with that user's identical
// submits on the same per-creator lock and dedupe against them. Whichever
// takes the lock first wins; the other collapses (submit) or is refused with
// a 409 (retry). Every round must end with exactly one pending row and no
// deadlock (40P01). Rounds alternate which side starts first.
//
// The forced case holds each transaction at its INSERT until both have
// arrived (or 300ms pass), so without the lock both pass their checks before
// either inserts; its 50ms head start makes each side win half the rounds.
// The free case races with a sub-millisecond random lag.
func TestRetrySuccessorRacesFreshSubmit(t *testing.T) {
	pg, store := setupSubmitDB(t)
	ctx := context.Background()
	creator := uuid.NewString()
	_, err := pg.DB.Pool().Exec(ctx, `
		INSERT INTO users (id, email, password_hash, salt, active, group_ids, created_at, updated_at)
		SELECT $1, 'retrier@example.com', 'x', 'y', true, ARRAY[g.id], now(), now()
		  FROM groups g ORDER BY g.name LIMIT 1`, creator)
	require.NoError(t, err)

	for _, tc := range []struct {
		name       string
		h          *Handler
		rounds     int
		lag        func() time.Duration
		bothOrders bool
	}{
		{"forced interleaving", &Handler{config: &insertBarrierStore{PostgresStore: store, arrived: make(chan struct{}, 2), parties: 2, wait: 300 * time.Millisecond}},
			8, func() time.Duration { return 50 * time.Millisecond }, true},
		{"free race", &Handler{config: store}, 100, func() time.Duration { return time.Duration(rand.IntN(500)) * time.Microsecond }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retryWon := 0
			for round := 0; round < tc.rounds; round++ {
				if b, ok := tc.h.config.(*insertBarrierStore); ok {
					for len(b.arrived) > 0 {
						<-b.arrived
					}
				}
				submitDup, retryErr, submitErr := retrySubmitRound(t, pg, tc.h, creator, round%2 == 0, tc.lag())
				requireNoDeadlock(t, retryErr)
				requireNoDeadlock(t, submitErr)
				require.NoError(t, submitErr, "round %d", round)
				require.Equal(t, 1, countPendingRows(t, pg), "round %d: a retry racing an identical submit must leave exactly one pending row", round)
				if retryErr == nil {
					require.NotNil(t, submitDup, "round %d: the retry won, so the submit must collapse onto its successor", round)
					retryWon++
					continue
				}
				ce, ok := IsClientError(retryErr)
				require.True(t, ok, "round %d: unexpected retry error %v", round, retryErr)
				assert.Equal(t, 409, ce.code, "round %d", round)
				assert.Contains(t, ce.message, "identical purchase", "round %d", round)
				assert.Nil(t, submitDup, "round %d: the submit won, so it must not report a duplicate", round)
			}
			t.Logf("%d rounds: retry won %d, submit won %d", tc.rounds, retryWon, tc.rounds-retryWon)
			if tc.bothOrders {
				assert.Positive(t, retryWon, "no round exercised the retry-first order")
				assert.Less(t, retryWon, tc.rounds, "no round exercised the submit-first order")
			}
		})
	}
}
