//go:build integration

package api

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
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
		{config.StatusFailed, false}, {config.StatusCanceled, false}, {config.StatusExpired, false},
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

// seedSubmitRows bulk-inserts n pending executions scheduled at `at` with the
// given source, creator (nil stores NULL) and recommendations.
func seedSubmitRows(t *testing.T, pg *testhelpers.PostgresContainer, n int, at time.Time, source string, creator *string, recs []config.RecommendationRecord) {
	t.Helper()
	recsJSON, err := json.Marshal(recs)
	require.NoError(t, err)
	_, err = pg.DB.Pool().Exec(context.Background(), `
		INSERT INTO purchase_executions (status, scheduled_date, recommendations, source, created_by_user_id, capacity_percent, error)
		SELECT 'pending', $1, $2::jsonb, $3, $4::uuid, 100, '' FROM generate_series(1, $5)`,
		at, recsJSON, source, creator, n)
	require.NoError(t, err)
}

// TestPersistExecution_DedupeSeesPastThousandPendingRows pins #266: the
// duplicate check must not depend on how many unrelated rows are pending.
// The old check read one page of pending rows (ORDER BY scheduled_date ASC
// LIMIT 1000), so 1,600 earlier-scheduled pending rows pushed the first
// submit off the page and its identical resubmit was inserted as a second
// execution.
func TestPersistExecution_DedupeSeesPastThousandPendingRows(t *testing.T) {
	pg, store := setupSubmitDB(t)
	ctx := context.Background()
	recs, key := submitFixture(t)
	h := &Handler{config: store}
	now := time.Now()

	// Plan/ramp-style rows: not web-submitted, scheduled an hour ago.
	seedSubmitRows(t, pg, 1100, now.Add(-time.Hour), "", nil, recs)
	// The same creator's earlier web submits of a different rec set, inside
	// the window: read by the dedupe query but with a different key.
	otherRecs := append([]config.RecommendationRecord(nil), recs...)
	otherRecs[0].Count = 7
	seedSubmitRows(t, pg, 500, now.Add(-time.Minute), common.PurchaseSourceWeb, nil, otherRecs)

	first := newSubmitExecution(t, recs)
	dup, err := h.persistExecutionAndSuppressions(ctx, first, nil, "", key)
	require.NoError(t, err)
	require.Nil(t, dup, "no earlier row carries this submit's key")

	dup, err = h.persistExecutionAndSuppressions(ctx, newSubmitExecution(t, recs), nil, "", key)
	require.NoError(t, err)
	require.NotNil(t, dup, "the resubmit must collapse onto the first submit despite 1,600 earlier pending rows")
	assert.Equal(t, first.ExecutionID, dup.ExecutionID)
	assert.Equal(t, 1601, countSubmitRows(t, pg), "1,600 seeded rows plus the first submit")
}

// TestPersistExecution_DedupeKeySemantics round-trips the first submit
// through Postgres and changes exactly one component of the duplicate key in
// the resubmit: every component of purchaseIdempotencyKey, the capacity, the
// web source and the window. Only an exact match (provider and payment are
// compared case- and whitespace-insensitively) collapses.
func TestPersistExecution_DedupeKeySemantics(t *testing.T) {
	pg, store := setupSubmitDB(t)
	ctx := context.Background()
	h := &Handler{config: store}
	acct := "acct-b"
	base := func() config.RecommendationRecord {
		return config.RecommendationRecord{
			Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", Engine: "",
			Count: 2, Term: 1, Payment: "no-upfront", Details: zonalEC2Details("us-east-1a"),
		}
	}
	for _, tc := range []struct {
		name     string
		mutate   func(r *config.RecommendationRecord)
		capacity int
		afterSQL string // applied to the first row before the resubmit
		wantDup  bool
	}{
		{name: "identical", wantDup: true},
		{name: "provider case and spaces", mutate: func(r *config.RecommendationRecord) { r.Provider = " AWS " }, wantDup: true},
		{name: "payment case", mutate: func(r *config.RecommendationRecord) { r.Payment = "No-Upfront" }, wantDup: true},
		{name: "provider", mutate: func(r *config.RecommendationRecord) { r.Provider = "azure" }},
		{name: "service", mutate: func(r *config.RecommendationRecord) { r.Service = "rds"; r.Details = nil }},
		{name: "region", mutate: func(r *config.RecommendationRecord) { r.Region = "us-west-2" }},
		{name: "resource type", mutate: func(r *config.RecommendationRecord) { r.ResourceType = "m5.xlarge" }},
		{name: "engine", mutate: func(r *config.RecommendationRecord) { r.Engine = "postgres" }},
		{name: "account", mutate: func(r *config.RecommendationRecord) { r.CloudAccountID = &acct }},
		{name: "count", mutate: func(r *config.RecommendationRecord) { r.Count = 3 }},
		{name: "term", mutate: func(r *config.RecommendationRecord) { r.Term = 3 }},
		{name: "payment", mutate: func(r *config.RecommendationRecord) { r.Payment = "all-upfront" }},
		{name: "availability zone", mutate: func(r *config.RecommendationRecord) { r.Details = zonalEC2Details("us-east-1b") }},
		{name: "platform", mutate: func(r *config.RecommendationRecord) {
			r.Details = json.RawMessage(`{"instance_type":"m5.large","platform":"Windows","tenancy":"default","scope":"Availability Zone","availability_zone":"us-east-1a"}`)
		}},
		{name: "tenancy", mutate: func(r *config.RecommendationRecord) {
			r.Details = json.RawMessage(`{"instance_type":"m5.large","platform":"Linux/UNIX","tenancy":"dedicated","scope":"Availability Zone","availability_zone":"us-east-1a"}`)
		}},
		{name: "capacity", capacity: 50},
		{name: "first row not web-submitted", afterSQL: `UPDATE purchase_executions SET source = 'cudly-cli'`},
		{name: "first row outside the window", afterSQL: `UPDATE purchase_executions SET scheduled_date = now() - interval '3 minutes'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pg.DB.Pool().Exec(ctx, `TRUNCATE purchase_executions CASCADE`)
			require.NoError(t, err)
			firstRecs := []config.RecommendationRecord{base()}
			first := newSubmitExecution(t, firstRecs)
			dup, err := h.persistExecutionAndSuppressions(ctx, first, nil, "", mustPurchaseIdempotencyKey("", firstRecs, 100))
			require.NoError(t, err)
			require.Nil(t, dup)
			if tc.afterSQL != "" {
				_, err = pg.DB.Pool().Exec(ctx, tc.afterSQL)
				require.NoError(t, err)
			}

			r := base()
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			capacity := 100
			if tc.capacity != 0 {
				capacity = tc.capacity
			}
			resub := []config.RecommendationRecord{r}
			exec, _, err := newPendingExecution(&ExecutePurchaseRequest{Recommendations: resub, CapacityPercent: capacity}, 0, 0)
			require.NoError(t, err)
			dup, err = h.persistExecutionAndSuppressions(ctx, exec, nil, "", mustPurchaseIdempotencyKey("", resub, capacity))
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

// TestListRecentSubmitsTx_Predicate pins each clause of the dedupe query in
// SQL (source, status, window start inclusive, creator, NULL creator), so the
// read stays bounded by matching rows rather than relying on a page size.
func TestListRecentSubmitsTx_Predicate(t *testing.T) {
	pg, store := setupSubmitDB(t)
	ctx := context.Background()
	recs, _ := submitFixture(t)
	newUser := func(email string) *string {
		id := uuid.New().String()
		_, err := pg.DB.Pool().Exec(ctx, `
			INSERT INTO users (id, email, password_hash, salt, active, group_ids, created_at, updated_at)
			SELECT $1, $2, 'x', 'y', true, ARRAY[g.id], now(), now()
			  FROM groups g ORDER BY g.name LIMIT 1`, id, email)
		require.NoError(t, err)
		return &id
	}
	alice, bob := newUser("alice@example.com"), newUser("bob@example.com")
	since := time.Now().Add(-purchaseIdempotencyWindow).Truncate(time.Microsecond)

	insert := func(source, status string, at time.Time, creator *string) string {
		recsJSON, err := json.Marshal(recs)
		require.NoError(t, err)
		var id string
		require.NoError(t, pg.DB.Pool().QueryRow(ctx, `
			INSERT INTO purchase_executions (status, scheduled_date, recommendations, source, created_by_user_id, capacity_percent, error)
			VALUES ($1, $2, $3::jsonb, $4, $5::uuid, 100, '') RETURNING execution_id::text`,
			status, at, recsJSON, source, creator).Scan(&id))
		return id
	}
	web, inWindow := common.PurchaseSourceWeb, since.Add(time.Minute)
	match := insert(web, "pending", inWindow, alice)
	atBoundary := insert(web, "completed", since, alice)
	insert(common.PurchaseSourceCLI, "pending", inWindow, alice)
	insert(web, config.StatusFailed, inWindow, alice)
	insert(web, "pending", since.Add(-time.Microsecond), alice)
	insert(web, "pending", inWindow, bob)
	noCreator := insert(web, "notified", inWindow, nil)

	list := func(creatorID string) []string {
		var ids []string
		require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
			rows, err := store.ListRecentSubmitsTx(ctx, tx, creatorID, since)
			for i := range rows {
				ids = append(ids, rows[i].ExecutionID)
			}
			return err
		}))
		return ids
	}
	assert.ElementsMatch(t, []string{match, atBoundary}, list(*alice))
	assert.ElementsMatch(t, []string{noCreator}, list(""))
}
