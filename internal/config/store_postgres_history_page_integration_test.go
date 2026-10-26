//go:build integration
// +build integration

package config

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// historyPageFixture is a store with purchase_history rows and executions
// whose timestamps collide within and across the two tables.
type historyPageFixture struct {
	store *PostgresStore
	keys  map[string]HistoryPageKey // purchase_id or execution_id -> stream key
}

func newHistoryPageFixture(ctx context.Context, t *testing.T) *historyPageFixture {
	t.Helper()
	return &historyPageFixture{store: setupRampStepStore(ctx, t), keys: map[string]HistoryPageKey{}}
}

func (f *historyPageFixture) addPurchase(ctx context.Context, t *testing.T, provider, account string, cloudAccountID *string, at time.Time) string {
	t.Helper()
	rec := &PurchaseHistoryRecord{
		AccountID: account, PurchaseID: "ph-" + uuid.NewString(), Timestamp: at, Provider: provider,
		Service: "ec2", Region: "us-east-1", ResourceType: "t3.micro", Count: 1, Term: 1, Payment: "no-upfront",
		CloudAccountID: cloudAccountID,
	}
	require.NoError(t, f.store.SavePurchaseHistory(ctx, rec))
	var id string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT id::text FROM purchase_history WHERE purchase_id = $1`, rec.PurchaseID).Scan(&id))
	f.keys[rec.PurchaseID] = HistoryPageKey{Timestamp: at, Source: HistorySourcePurchase, ID: id}
	return rec.PurchaseID
}

func (f *historyPageFixture) addExecution(ctx context.Context, t *testing.T, status, execErr string, at time.Time) string {
	t.Helper()
	exec := &PurchaseExecution{
		ExecutionID: uuid.NewString(), IdempotencyKey: uuid.NewString(), Status: status, Error: execErr, ScheduledDate: at,
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, exec))
	f.keys[exec.ExecutionID] = HistoryPageKey{Timestamp: at, Source: HistorySourceExecution, ID: exec.ExecutionID}
	return exec.ExecutionID
}

// sortedIDs orders ids by their stream key.
func (f *historyPageFixture) sortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return f.keys[out[i]].SortsBefore(f.keys[out[j]]) })
	return out
}

// walkPurchases pages ListPurchaseHistoryPage to the end. between, when set,
// runs after each page.
func walkPurchases(ctx context.Context, t *testing.T, s *PostgresStore, filter PurchaseHistoryFilter, between func(int)) []string {
	t.Helper()
	var got []string
	var after *HistoryPageKey
	for page := 0; ; page++ {
		require.Less(t, page, 500)
		rows, err := s.ListPurchaseHistoryPage(ctx, filter, after)
		require.NoError(t, err)
		for i := range rows {
			got = append(got, rows[i].PurchaseID)
		}
		if between != nil {
			between(page)
		}
		if len(rows) < filter.Limit {
			return got
		}
		k := rows[len(rows)-1].Key()
		after = &k
	}
}

func walkExecutions(ctx context.Context, t *testing.T, s *PostgresStore, q HistoryExecutionsPageQuery) []string {
	t.Helper()
	var got []string
	for page := 0; ; page++ {
		require.Less(t, page, 500)
		rows, err := s.ListHistoryExecutionsPage(ctx, q)
		require.NoError(t, err)
		for i := range rows {
			got = append(got, rows[i].ExecutionID)
		}
		if len(rows) < q.Limit {
			return got
		}
		last := rows[len(rows)-1]
		q.After = &HistoryPageKey{Timestamp: last.ScheduledDate, Source: HistorySourceExecution, ID: last.ExecutionID}
	}
}

// TestIntegration_ListPurchaseHistoryPage_WalksEveryRowOnce pages through
// purchase_history rows in groups that share a timestamp, with pages ending
// inside a group. Every row comes back once, in (timestamp DESC, id DESC)
// order, and rows inserted above the cursor between pages do not shift the
// walk.
func TestIntegration_ListPurchaseHistoryPage_WalksEveryRowOnce(t *testing.T) {
	ctx := context.Background()
	f := newHistoryPageFixture(ctx, t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	var ids []string
	for i := 0; i < 37; i++ {
		ids = append(ids, f.addPurchase(ctx, t, "aws", "111111111111", nil, base.Add(-time.Duration(i/4)*time.Minute)))
	}

	var inserted []string
	got := walkPurchases(ctx, t, f.store, PurchaseHistoryFilter{Limit: 5}, func(page int) {
		if page == 1 {
			inserted = append(inserted, f.addPurchase(ctx, t, "aws", "111111111111", nil, time.Now().UTC().Truncate(time.Microsecond)))
		}
	})

	assert.Equal(t, f.sortedIDs(ids), got)
	assert.NotContains(t, got, inserted[0])
}

// TestIntegration_ListHistoryExecutionsPage_WalksEveryRowOnce is the same walk
// over executions, with rows the query must exclude mixed in: a clean
// completed execution and a status outside the requested set.
func TestIntegration_ListHistoryExecutionsPage_WalksEveryRowOnce(t *testing.T) {
	ctx := context.Background()
	f := newHistoryPageFixture(ctx, t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	statuses := []string{"failed", "expired", StatusCanceled, "partially_completed"}
	var ids []string
	for i := 0; i < 29; i++ {
		ids = append(ids, f.addExecution(ctx, t, statuses[i%len(statuses)], "boom", base.Add(-time.Duration(i/3)*time.Minute)))
	}
	ids = append(ids, f.addExecution(ctx, t, "completed", "history write failed", base))
	f.addExecution(ctx, t, "completed", "", base)
	f.addExecution(ctx, t, "pending", "", base)

	got := walkExecutions(ctx, t, f.store, HistoryExecutionsPageQuery{
		Statuses: append(statuses, "completed"), Limit: 4,
	})

	assert.Equal(t, f.sortedIDs(ids), got)
}

// TestIntegration_HistoryKeyset_CrossSourceTies pins the keyset predicate
// where the two tables meet. At an equal timestamp purchase_history rows sort
// before executions, so a cursor on a purchase row still returns executions at
// that timestamp, and a cursor on an execution no longer returns purchase rows
// at that timestamp.
func TestIntegration_HistoryKeyset_CrossSourceTies(t *testing.T) {
	ctx := context.Background()
	f := newHistoryPageFixture(ctx, t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	p1 := f.addPurchase(ctx, t, "aws", "111111111111", nil, at)
	p2 := f.addPurchase(ctx, t, "aws", "111111111111", nil, at)
	e1 := f.addExecution(ctx, t, "failed", "boom", at)
	e2 := f.addExecution(ctx, t, "failed", "boom", at)
	older := f.addPurchase(ctx, t, "aws", "111111111111", nil, at.Add(-time.Microsecond))
	olderExec := f.addExecution(ctx, t, "failed", "boom", at.Add(-time.Microsecond))
	order := f.sortedIDs([]string{p1, p2, e1, e2, older, olderExec})
	require.Equal(t, HistorySourcePurchase, f.keys[order[0]].Source)
	require.Equal(t, HistorySourcePurchase, f.keys[order[1]].Source)

	purchasesAfter := func(k HistoryPageKey) []string {
		rows, err := f.store.ListPurchaseHistoryPage(ctx, PurchaseHistoryFilter{Limit: 10}, &k)
		require.NoError(t, err)
		ids := make([]string, len(rows))
		for i := range rows {
			ids[i] = rows[i].PurchaseID
		}
		return ids
	}
	execsAfter := func(k HistoryPageKey) []string {
		rows, err := f.store.ListHistoryExecutionsPage(ctx, HistoryExecutionsPageQuery{Statuses: []string{"failed"}, Limit: 10, After: &k})
		require.NoError(t, err)
		ids := make([]string, len(rows))
		for i := range rows {
			ids[i] = rows[i].ExecutionID
		}
		return ids
	}

	// Cursor on the first purchase row at `at`.
	assert.Equal(t, []string{order[1], older}, purchasesAfter(f.keys[order[0]]))
	assert.Equal(t, []string{order[2], order[3], olderExec}, execsAfter(f.keys[order[0]]))
	// Cursor on the first execution at `at`: no purchase row at `at` is left.
	assert.Equal(t, []string{older}, purchasesAfter(f.keys[order[2]]))
	assert.Equal(t, []string{order[3], olderExec}, execsAfter(f.keys[order[2]]))
}

// TestIntegration_ListPurchaseHistoryPage_MatchesFilteredFetch guards the
// page query's predicate against GetPurchaseHistoryFiltered's: with no cursor
// and a limit above the row count both must select the same rows for every
// filter shape (provider, both account columns, the date bounds).
func TestIntegration_ListPurchaseHistoryPage_MatchesFilteredFetch(t *testing.T) {
	ctx := context.Background()
	f := newHistoryPageFixture(ctx, t)
	acct := &CloudAccount{Name: "Paging drift", Provider: "aws", ExternalID: "222222222222", Enabled: true}
	require.NoError(t, f.store.CreateCloudAccount(ctx, acct))
	start := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	end := start.Add(24 * time.Hour)
	for _, at := range []time.Time{start.Add(-time.Microsecond), start, start.Add(time.Hour), end, end.Add(time.Microsecond)} {
		f.addPurchase(ctx, t, "aws", acct.ExternalID, &acct.ID, at)
		f.addPurchase(ctx, t, "aws", acct.ExternalID, nil, at)
		f.addPurchase(ctx, t, "AWS", acct.ExternalID, nil, at)
		f.addPurchase(ctx, t, "", acct.ExternalID, nil, at)
		f.addPurchase(ctx, t, "azure", acct.ExternalID, nil, at)
		f.addPurchase(ctx, t, "aws", "333333333333", nil, at)
	}
	filters := map[string]PurchaseHistoryFilter{
		"none":              {},
		"provider":          {Provider: "aws"},
		"cloud account":     {AccountIDs: []string{acct.ID}},
		"external by aws":   {ExternalIDsByProvider: map[string][]string{"aws": {acct.ExternalID}}},
		"external unknown":  {ExternalIDsByProvider: map[string][]string{"": {acct.ExternalID}}},
		"both account cols": {AccountIDs: []string{acct.ID}, ExternalIDsByProvider: map[string][]string{"aws": {acct.ExternalID}}},
		"date window":       {Start: &start, End: &end},
		"everything":        {Provider: "aws", AccountIDs: []string{acct.ID}, ExternalIDsByProvider: map[string][]string{"aws": {acct.ExternalID}}, Start: &start, End: &end},
	}
	for name, filter := range filters {
		t.Run(name, func(t *testing.T) {
			filter.Limit = MaxListLimit
			want, err := f.store.GetPurchaseHistoryFiltered(ctx, filter)
			require.NoError(t, err)
			page, err := f.store.ListPurchaseHistoryPage(ctx, filter, nil)
			require.NoError(t, err)
			wantIDs := make([]string, len(want))
			for i := range want {
				wantIDs[i] = want[i].PurchaseID
			}
			gotIDs := make([]string, len(page))
			for i := range page {
				gotIDs[i] = page[i].PurchaseID
				assert.Equal(t, f.keys[page[i].PurchaseID].ID, page[i].RowID, "row id scanned")
			}
			require.NotEmpty(t, wantIDs, "a vacuous comparison proves nothing")
			assert.ElementsMatch(t, wantIDs, gotIDs)
			assert.Equal(t, f.sortedIDs(gotIDs), gotIDs, "stream order")
		})
	}
}

// TestIntegration_ListHistoryExecutionsPage_DateBoundsInclusive checks the
// scheduled_date bounds include both edges, like the History handler's
// in-memory date check, and nothing a microsecond outside them.
func TestIntegration_ListHistoryExecutionsPage_DateBoundsInclusive(t *testing.T) {
	ctx := context.Background()
	f := newHistoryPageFixture(ctx, t)
	start := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	end := start.Add(24 * time.Hour)
	f.addExecution(ctx, t, "failed", "boom", start.Add(-time.Microsecond))
	onStart := f.addExecution(ctx, t, "failed", "boom", start)
	onEnd := f.addExecution(ctx, t, "failed", "boom", end)
	f.addExecution(ctx, t, "failed", "boom", end.Add(time.Microsecond))

	rows, err := f.store.ListHistoryExecutionsPage(ctx, HistoryExecutionsPageQuery{
		Statuses: []string{"failed"}, Start: &start, End: &end, Limit: 10,
	})
	require.NoError(t, err)
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ExecutionID
	}
	assert.Equal(t, []string{onEnd, onStart}, ids)
}
