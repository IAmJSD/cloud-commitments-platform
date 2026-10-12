//go:build integration

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertBarrierStore holds each submit transaction at its INSERT until every
// participant has reached it (or the wait times out). That forces the
// interleaving MON-01 describes: both transactions have already run their
// duplicate check before either inserts. A guard that serializes the check
// (the per-creator lock) keeps the second transaction out of the barrier, so
// the first one times out and inserts alone.
type insertBarrierStore struct {
	*config.PostgresStore
	arrived chan struct{}
	parties int
	wait    time.Duration
}

func (s *insertBarrierStore) SavePurchaseExecutionTx(ctx context.Context, tx pgx.Tx, e *config.PurchaseExecution) error {
	s.arrived <- struct{}{}
	deadline := time.After(s.wait)
	for len(s.arrived) < s.parties {
		select {
		case <-deadline:
			return s.PostgresStore.SavePurchaseExecutionTx(ctx, tx, e)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return s.PostgresStore.SavePurchaseExecutionTx(ctx, tx, e)
}

func submitFixture(t *testing.T) ([]config.RecommendationRecord, string) {
	t.Helper()
	recs := []config.RecommendationRecord{{
		Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Count: 2, Term: 1, Payment: "no-upfront",
	}}
	return recs, mustPurchaseIdempotencyKey("", recs, 100)
}

func newSubmitExecution(t *testing.T, recs []config.RecommendationRecord) *config.PurchaseExecution {
	t.Helper()
	exec, _, err := newPendingExecution(&ExecutePurchaseRequest{Recommendations: recs, CapacityPercent: 100}, 0, 0)
	require.NoError(t, err)
	return exec
}

func countSubmitRows(t *testing.T, pg *testhelpers.PostgresContainer) int {
	t.Helper()
	var n int
	require.NoError(t, pg.DB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM purchase_executions`).Scan(&n))
	return n
}

func setupSubmitDB(t *testing.T) (*testhelpers.PostgresContainer, *config.PostgresStore) {
	t.Helper()
	ctx := context.Background()
	pg, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Cleanup(ctx)) })
	require.NoError(t, migrations.RunMigrations(ctx, pg.DB.Pool(), "../database/postgres/migrations", "", ""))
	return pg, config.NewPostgresStore(pg.DB)
}

// TestPersistExecution_ConcurrentIdenticalSubmitsCreateOneRow is MON-01 case 1:
// two identical submits (double click, client retry) must not both insert.
func TestPersistExecution_ConcurrentIdenticalSubmitsCreateOneRow(t *testing.T) {
	pg, store := setupSubmitDB(t)
	recs, key := submitFixture(t)
	barrier := &insertBarrierStore{PostgresStore: store, arrived: make(chan struct{}, 2), parties: 2, wait: time.Second}
	h := &Handler{config: barrier}

	var wg sync.WaitGroup
	dups := make([]*config.PurchaseExecution, 2)
	errs := make([]error, 2)
	for i := range dups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dups[i], errs[i] = h.persistExecutionAndSuppressions(context.Background(), newSubmitExecution(t, recs), nil, "", key)
		}()
	}
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, 1, countSubmitRows(t, pg), "identical concurrent submits must create exactly one execution")
	collapsed := 0
	for _, d := range dups {
		if d != nil {
			collapsed++
		}
	}
	assert.Equal(t, 1, collapsed, "exactly one submit must report the other as its duplicate")
}

// TestPersistExecution_RetryAfterRowLeftPending is MON-01 case 2: a direct
// execute moves its row out of pending within seconds, and a retry arriving
// then must still collapse onto it instead of buying again. A row that bought
// nothing (failed, canceled) must not absorb a resubmit.
func TestPersistExecution_RetryAfterRowLeftPending(t *testing.T) {
	pg, store := setupSubmitDB(t)
	for _, tc := range []struct {
		status  string
		wantDup bool
	}{
		{"pending", true}, {"notified", true}, {"approved", true}, {"running", true},
		{"completed", true}, {"partially_completed", true},
		{config.StatusFailed, false}, {config.StatusCanceled, false}, {config.LegacyStatusCanceled, false}, {config.StatusExpired, false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			recs, key := submitFixture(t)
			h := &Handler{config: store}
			ctx := context.Background()
			_, err := pg.DB.Pool().Exec(ctx, `TRUNCATE purchase_executions CASCADE`)
			require.NoError(t, err)

			first := newSubmitExecution(t, recs)
			dup, err := h.persistExecutionAndSuppressions(ctx, first, nil, "", key)
			require.NoError(t, err)
			require.Nil(t, dup)
			_, err = pg.DB.Pool().Exec(ctx, `UPDATE purchase_executions SET status = $1 WHERE execution_id = $2`, tc.status, first.ExecutionID)
			require.NoError(t, err)

			dup, err = h.persistExecutionAndSuppressions(ctx, newSubmitExecution(t, recs), nil, "", key)
			require.NoError(t, err)
			if tc.wantDup {
				require.NotNil(t, dup)
				assert.Equal(t, first.ExecutionID, dup.ExecutionID)
				assert.Equal(t, 1, countSubmitRows(t, pg))
			} else {
				assert.Nil(t, dup)
				assert.Equal(t, 2, countSubmitRows(t, pg))
			}
		})
	}
}

// TestPersistExecution_DedupeIsPerCreator pins the creator filter in the
// store query: two users submitting identical recommendations each get their
// own execution, while the same user's resubmit collapses.
func TestPersistExecution_DedupeIsPerCreator(t *testing.T) {
	pg, store := setupSubmitDB(t)
	ctx := context.Background()
	recs, _ := submitFixture(t)
	h := &Handler{config: store}

	newUser := func(email string) string {
		id := uuid.New().String()
		_, err := pg.DB.Pool().Exec(ctx, `
			INSERT INTO users (id, email, password_hash, salt, active, group_ids, created_at, updated_at)
			SELECT $1, $2, 'x', 'y', true, ARRAY[g.id], now(), now()
			  FROM groups g ORDER BY g.name LIMIT 1`, id, email)
		require.NoError(t, err)
		return id
	}
	submit := func(creator string) *config.PurchaseExecution {
		exec := newSubmitExecution(t, recs)
		exec.CreatedByUserID = &creator
		dup, err := h.persistExecutionAndSuppressions(ctx, exec, nil, creator, mustPurchaseIdempotencyKey(creator, recs, 100))
		require.NoError(t, err)
		return dup
	}
	alice, bob := newUser("alice@example.com"), newUser("bob@example.com")

	assert.Nil(t, submit(alice))
	assert.Nil(t, submit(bob), "another user's identical submit is not a duplicate")
	assert.NotNil(t, submit(alice), "the same user's resubmit collapses")
	assert.Equal(t, 2, countSubmitRows(t, pg))
}
