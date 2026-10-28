//go:build integration

package config

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// historySummaryFixture seeds two accounts with purchase_history rows and
// executions covering every shape the History summary groups on.
type historySummaryFixture struct {
	store    *PostgresStore
	accountA string
	accountB string
	userID   string
	base     time.Time
}

func newHistorySummaryFixture(ctx context.Context, t *testing.T) *historySummaryFixture {
	t.Helper()
	store := setupRampStepStore(ctx, t)
	f := &historySummaryFixture{store: store, base: time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)}
	a := &CloudAccount{Name: "Summary A", Provider: "aws", ExternalID: "ext-sum-a", Enabled: true}
	b := &CloudAccount{Name: "Summary B", Provider: "azure", ExternalID: "ext-sum-b", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, a))
	require.NoError(t, store.CreateCloudAccount(ctx, b))
	f.accountA, f.accountB = a.ID, b.ID
	require.NoError(t, store.db.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, group_ids)
		 SELECT 'summary-fixture@example.com', 'x', ARRAY[id] FROM groups ORDER BY name LIMIT 1 RETURNING id`).Scan(&f.userID))
	return f
}

func (f *historySummaryFixture) savePurchase(ctx context.Context, t *testing.T, day int, provider, accountID string, cloudAccountID *string, upfront *float64, savings float64, revoked bool) {
	t.Helper()
	rec := &PurchaseHistoryRecord{
		AccountID: accountID, PurchaseID: uuid.NewString(), Timestamp: f.base.Add(time.Duration(day) * 24 * time.Hour),
		Provider: provider, Service: "ec2", Region: "us-east-1", ResourceType: "t3.micro", Count: 1, Term: 1,
		Payment: "no-upfront", UpfrontCost: upfront, EstimatedSavings: savings, CloudAccountID: cloudAccountID,
	}
	require.NoError(t, f.store.SavePurchaseHistory(ctx, rec))
	if cloudAccountID == nil {
		// SavePurchaseHistory now links a row to the registered account with
		// the same external id (#1235); clear it so nil seeds a legacy row.
		tag, err := f.store.db.Exec(ctx, `UPDATE purchase_history SET cloud_account_id = NULL WHERE purchase_id = $1`, rec.PurchaseID)
		require.NoError(t, err)
		require.Equal(t, int64(1), tag.RowsAffected())
	}
	if revoked {
		_, err := f.store.db.Exec(ctx, `UPDATE purchase_history SET revoked_at = now() WHERE purchase_id = $1`, rec.PurchaseID)
		require.NoError(t, err)
	}
}

func (f *historySummaryFixture) saveExecution(ctx context.Context, t *testing.T, at time.Time, status, execErr string, cloudAccountID, createdBy *string, recs []RecommendationRecord) string {
	t.Helper()
	exec := &PurchaseExecution{
		ExecutionID: uuid.NewString(), IdempotencyKey: uuid.NewString(), Status: status, Error: execErr,
		ScheduledDate: at, CloudAccountID: cloudAccountID, CreatedByUserID: createdBy, Recommendations: recs,
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, exec))
	return exec.ExecutionID
}

func (f *historySummaryFixture) seed(ctx context.Context, t *testing.T) {
	t.Helper()
	a, b := f.accountA, f.accountB
	for day := 0; day < 20; day++ {
		up := float64(day) + 0.25
		f.savePurchase(ctx, t, day, "aws", "ext-sum-a", &a, &up, float64(day)/2, day%7 == 0)
		f.savePurchase(ctx, t, day, "azure", "ext-sum-b", &b, &up, 3, day%5 == 0)
		f.savePurchase(ctx, t, day, "aws", "ext-sum-a", nil, &up, 1, false) // legacy row: no cloud_account_id
	}
	f.savePurchase(ctx, t, 3, "aws", "ext-sum-a", &a, nil, 2, false) // unknown upfront
	f.savePurchase(ctx, t, 4, "", "ext-sum-a", nil, new(float64), 1, false)

	recA := []RecommendationRecord{{Provider: "aws", CloudAccountID: &a}}
	mixed := []RecommendationRecord{{Provider: "aws", CloudAccountID: &a}, {Provider: "azure", CloudAccountID: &b}}
	statuses := []string{"pending", "notified", "scheduled", "approved", "running", "paused", "failed", "expired",
		StatusCanceled, "partially_completed"}
	for i, st := range statuses {
		at := f.base.Add(time.Duration(i) * 36 * time.Hour)
		f.saveExecution(ctx, t, at, st, "", &a, nil, recA)
		f.saveExecution(ctx, t, at, st, "", nil, &f.userID, mixed)
		f.saveExecution(ctx, t, at, st, "", nil, nil, nil) // recommendations stored as JSON null
	}
	f.saveExecution(ctx, t, f.base, "completed", "", &a, nil, recA)                     // clean: excluded
	f.saveExecution(ctx, t, f.base, "completed", "history write failed", &a, nil, recA) // audit gap: kept
}

func purchaseGroupKey(account string, cloudAccountID *string, provider string, revoked bool) string {
	ca := ""
	if cloudAccountID != nil {
		ca = *cloudAccountID
	}
	return fmt.Sprintf("%s|%s|%s|%t", account, ca, provider, revoked)
}

func recsKey(recs []RecommendationRecord) string {
	parts := make([]string, len(recs))
	for i := range recs {
		ca := ""
		if recs[i].CloudAccountID != nil {
			ca = *recs[i].CloudAccountID
		}
		parts[i] = recs[i].Provider + "/" + ca
	}
	return strings.Join(parts, ",")
}

func execGroupKey(status string, cloudAccountID, createdBy *string, recs []RecommendationRecord) string {
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return fmt.Sprintf("%s|%s|%s|%s", status, deref(cloudAccountID), deref(createdBy), recsKey(recs))
}

type purchaseTally struct {
	count   int
	upfront *float64
	savings float64
}

// tallyPurchaseRows groups fetched rows the way HistorySummaryAggregate should.
func tallyPurchaseRows(rows []PurchaseHistoryRecord) map[string]*purchaseTally {
	out := map[string]*purchaseTally{}
	for i := range rows {
		r := &rows[i]
		k := purchaseGroupKey(r.AccountID, r.CloudAccountID, r.Provider, r.RevokedAt != nil)
		tl, ok := out[k]
		if !ok {
			tl = &purchaseTally{upfront: new(float64)}
			out[k] = tl
		}
		tl.count++
		if tl.upfront != nil && r.UpfrontCost != nil {
			*tl.upfront += *r.UpfrontCost
		} else {
			tl.upfront = nil
		}
		tl.savings += r.EstimatedSavings
	}
	return out
}

func assertPurchaseGroupsMatch(t *testing.T, want map[string]*purchaseTally, got []PurchaseHistorySummaryGroup) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range got {
		g := &got[i]
		w, ok := want[purchaseGroupKey(g.AccountID, g.CloudAccountID, g.Provider, g.Revoked)]
		require.True(t, ok, "unexpected group %+v", g)
		assert.Equal(t, w.count, g.Count)
		assert.InDelta(t, w.savings, g.EstimatedSavings, 1e-6)
		if w.upfront == nil {
			assert.Nil(t, g.UpfrontCost)
		} else {
			require.NotNil(t, g.UpfrontCost)
			assert.InDelta(t, *w.upfront, *g.UpfrontCost, 1e-6)
		}
	}
}

// TestIntegration_HistorySummaryAggregate_MatchesRowFetch is the drift guard
// for issue #675: for each filter, the aggregate's groups must equal the rows
// GetPurchaseHistoryFiltered / GetExecutionsByStatuses return without a cap,
// grouped in Go. A predicate added to one query and not the other fails here.
func TestIntegration_HistorySummaryAggregate_MatchesRowFetch(t *testing.T) {
	ctx := context.Background()
	f := newHistorySummaryFixture(ctx, t)
	f.seed(ctx, t)

	start := f.base.Add(5 * 24 * time.Hour)
	end := f.base.Add(12*24*time.Hour - time.Second)
	filters := map[string]PurchaseHistoryFilter{
		"none":             {},
		"provider":         {Provider: "aws"},
		"account uuid":     {AccountIDs: []string{f.accountA}},
		"account external": {ExternalIDsByProvider: map[string][]string{"aws": {"ext-sum-a"}}},
		"account both":     {AccountIDs: []string{f.accountB}, ExternalIDsByProvider: map[string][]string{"aws": {"ext-sum-a"}, "": {"ext-sum-b"}}},
		"date":             {Start: &start, End: &end},
		"all":              {Provider: "aws", AccountIDs: []string{f.accountA}, Start: &start, End: &end},
	}
	statuses := []string{"pending", "notified", "scheduled", "approved", "running", "paused",
		"completed", "partially_completed", "failed", "expired", StatusCanceled}
	for name, filter := range filters {
		t.Run(name, func(t *testing.T) {
			agg, err := f.store.HistorySummaryAggregate(ctx, HistorySummaryFilter{
				History: filter, ExecutionStatuses: statuses, ExecutionStart: filter.Start, ExecutionEnd: filter.End,
			})
			require.NoError(t, err)

			uncapped := filter
			uncapped.Limit = MaxListLimit
			rows, err := f.store.GetPurchaseHistoryFiltered(ctx, uncapped)
			require.NoError(t, err)
			require.Less(t, len(rows), MaxListLimit, "fixture must fit under the cap")
			require.NotEmpty(t, rows)
			assertPurchaseGroupsMatch(t, tallyPurchaseRows(rows), agg.Purchases)

			execs, err := f.store.GetExecutionsByStatuses(ctx, statuses, MaxListLimit)
			require.NoError(t, err)
			want := map[string]int{}
			for i := range execs {
				e := &execs[i]
				if filter.Start != nil && (e.ScheduledDate.Before(*filter.Start) || e.ScheduledDate.After(*filter.End)) {
					continue
				}
				want[execGroupKey(e.Status, e.CloudAccountID, e.CreatedByUserID, e.Recommendations)]++
			}
			got := map[string]int{}
			for i := range agg.Executions {
				g := &agg.Executions[i]
				got[execGroupKey(g.Status, g.CloudAccountID, g.CreatedByUserID, g.Recommendations)] += g.Count
			}
			assert.Equal(t, want, got)
		})
	}
}

// TestIntegration_HistorySummaryAggregate_DateBoundsInclusive pins the SQL
// date bound to the History handler's in-memory check: a row exactly at Start
// or End counts, one microsecond outside does not.
func TestIntegration_HistorySummaryAggregate_DateBoundsInclusive(t *testing.T) {
	ctx := context.Background()
	f := newHistorySummaryFixture(ctx, t)
	start := f.base
	end := f.base.Add(24*time.Hour - time.Second)
	for _, at := range []time.Time{start.Add(-time.Microsecond), start, end, end.Add(time.Microsecond)} {
		f.saveExecution(ctx, t, at, "failed", "boom", &f.accountA, nil, nil)
	}
	agg, err := f.store.HistorySummaryAggregate(ctx, HistorySummaryFilter{
		ExecutionStatuses: []string{"failed"}, ExecutionStart: &start, ExecutionEnd: &end,
	})
	require.NoError(t, err)
	require.Len(t, agg.Executions, 1)
	assert.Equal(t, 2, agg.Executions[0].Count)
	assert.Empty(t, agg.Executions[0].Recommendations, "JSON null recommendations project to an empty list")
}

// TestIntegration_HistorySummaryAggregate_SnapshotUnderConcurrentCompletion
// races the aggregate against a writer that moves one purchase back and forth
// between an in-flight execution and a purchase_history row, each move in one
// transaction. Every aggregate must count that purchase exactly once: reading
// the two tables outside one snapshot can see it in both or in neither.
func TestIntegration_HistorySummaryAggregate_SnapshotUnderConcurrentCompletion(t *testing.T) {
	ctx := context.Background()
	f := newHistorySummaryFixture(ctx, t)
	execID := f.saveExecution(ctx, t, f.base, "running", "", &f.accountA, nil, nil)
	up := 10.0
	purchaseID := uuid.NewString()
	complete := func() error {
		tx, err := f.store.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `UPDATE purchase_executions SET status = 'completed' WHERE execution_id = $1`, execID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment, upfront_cost, estimated_savings)
			VALUES ('ext-sum-a', $1, now(), 'aws', 'ec2', 'us-east-1', 't3.micro', 1, 'no-upfront', $2, 1)`, purchaseID, up); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	reopen := func() error {
		tx, err := f.store.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `DELETE FROM purchase_history WHERE purchase_id = $1`, purchaseID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE purchase_executions SET status = 'running' WHERE execution_id = $1`, execID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	var writerErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if err := complete(); err != nil {
				writerErr = err
				return
			}
			if err := reopen(); err != nil {
				writerErr = err
				return
			}
		}
	}()
	filter := HistorySummaryFilter{ExecutionStatuses: []string{"running", "completed"}}
	for i := 0; i < 1500; i++ {
		agg, err := f.store.HistorySummaryAggregate(ctx, filter)
		require.NoError(t, err)
		seen := 0
		for _, g := range agg.Purchases {
			seen += g.Count
		}
		for _, g := range agg.Executions {
			seen += g.Count
		}
		if seen != 1 {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("iteration %d: purchase counted %d times, want exactly once", i, seen)
		}
	}
	stop.Store(true)
	wg.Wait()
	require.NoError(t, writerErr)
}
