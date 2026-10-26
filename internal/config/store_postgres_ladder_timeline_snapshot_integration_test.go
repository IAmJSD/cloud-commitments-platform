//go:build integration

package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// errLadderTimelineChanged is the failure the web client's completeTimeline
// (frontend/src/api/ladder.ts) raises when the pages it stitches together
// disagree: "The ladder timeline changed while loading. Reload to try again."
var errLadderTimelineChanged = errors.New("the ladder timeline changed while loading")

// clientTimelinePage is the wire shape completeTimeline reads.
type clientTimelinePage struct {
	TotalUSDHr   *string               `json:"total_usd_hr"`
	TotalODUSDHr *string               `json:"total_od_usd_hr"`
	NextCursor   *LadderTimelineCursor `json:"next_cursor"`
	Events       []clientTimelineRow   `json:"events"`
	Runs         []clientTimelineRow   `json:"runs"`
	TotalCount   int64                 `json:"total_count"`
}

type clientTimelineRow struct {
	ID          string `json:"id"`
	AmountUSDHr string `json:"amount_usd_hr"`
	ODUSDHr     string `json:"od_usd_hr"`
}

type clientTimelineLoad struct {
	ids    []string
	total  string
	rounds int
}

func fetchClientTimelinePage(ctx context.Context, store *PostgresStore, accountID string, runs bool, cursor *LadderTimelineCursor) (*clientTimelinePage, error) {
	var page any
	var err error
	if runs {
		page, err = store.ListLadderTimelineRuns(ctx, accountID, "aws", cursor)
	} else {
		page, err = store.ListLadderTimeline(ctx, accountID, "aws", cursor)
	}
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return nil, err
	}
	var wire clientTimelinePage
	return &wire, json.Unmarshal(raw, &wire)
}

func sameTimelineTotals(a, b *clientTimelinePage) bool {
	same := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.TotalCount == b.TotalCount && same(a.TotalUSDHr, b.TotalUSDHr) && same(a.TotalODUSDHr, b.TotalODUSDHr)
}

func addLadderMicro(sum *big.Int, amount string) error {
	r, ok := new(big.Rat).SetString(amount)
	if !ok {
		return fmt.Errorf("unparseable amount %q", amount)
	}
	micro := new(big.Rat).Mul(r, new(big.Rat).SetInt(ladderMicro))
	if !micro.IsInt() {
		return fmt.Errorf("amount %q is finer than a micro-dollar", amount)
	}
	sum.Add(sum, micro.Num())
	return nil
}

// loadLadderTimelineLikeClient follows next_cursor exactly as completeTimeline
// does and applies the same cross-page checks: one total_count, total_usd_hr
// and total_od_usd_hr across pages, no repeated ID, and rows that add up to
// the totals. between runs before each follow-up page request.
func loadLadderTimelineLikeClient(ctx context.Context, store *PostgresStore, accountID string, runs bool, between func()) (clientTimelineLoad, error) {
	var load clientTimelineLoad
	var first *clientTimelinePage
	var cursor *LadderTimelineCursor
	seen := map[string]bool{}
	amount, od := new(big.Int), new(big.Int)
	for {
		page, err := fetchClientTimelinePage(ctx, store, accountID, runs, cursor)
		if err != nil {
			return load, err
		}
		load.rounds++
		if first == nil {
			first = page
		} else if !sameTimelineTotals(first, page) {
			return load, fmt.Errorf("%w: totals moved on page %d", errLadderTimelineChanged, load.rounds)
		}
		for _, row := range append(page.Events, page.Runs...) {
			if seen[row.ID] {
				return load, fmt.Errorf("%w: %s repeated", errLadderTimelineChanged, row.ID)
			}
			seen[row.ID] = true
			load.ids = append(load.ids, row.ID)
			if !runs && (addLadderMicro(amount, row.AmountUSDHr) != nil || addLadderMicro(od, row.ODUSDHr) != nil) {
				return load, fmt.Errorf("unparseable amounts on %s", row.ID)
			}
		}
		if page.NextCursor == nil {
			break
		}
		if between != nil {
			between()
		}
		cursor = page.NextCursor
	}
	return load, checkClientTimelineLoad(&load, first, runs, amount, od)
}

func checkClientTimelineLoad(load *clientTimelineLoad, first *clientTimelinePage, runs bool, amount, od *big.Int) error {
	if int64(len(load.ids)) != first.TotalCount {
		return fmt.Errorf("%w: %d rows for total_count %d", errLadderTimelineChanged, len(load.ids), first.TotalCount)
	}
	if runs {
		return nil
	}
	load.total = *first.TotalUSDHr
	if formatLadderMicro(amount) != *first.TotalUSDHr || formatLadderMicro(od) != *first.TotalODUSDHr {
		return fmt.Errorf("%w: rows add up to %s/%s, totals say %s/%s", errLadderTimelineChanged,
			formatLadderMicro(amount), formatLadderMicro(od), *first.TotalUSDHr, *first.TotalODUSDHr)
	}
	return nil
}

type ladderTimelineFixture struct {
	store     *PostgresStore
	cfgID     string
	accountID string
	runID     string
	amendID   string
	date      time.Time
}

// seedLadderTimelineFixture saves one planned run whose n scheduled tranches
// are all editable, plus n-1 more planned runs, so both endpoints span
// several of the earlier 100-row pages.
func seedLadderTimelineFixture(ctx context.Context, t *testing.T, n int) *ladderTimelineFixture {
	t.Helper()
	store := setupLadderStore(ctx, t)
	f := &ladderTimelineFixture{store: store, cfgID: seedLadderConfig(ctx, t, store), date: time.Now().UTC().Add(48 * time.Hour)}
	require.NoError(t, store.db.QueryRow(ctx, "SELECT cloud_account_id FROM ladder_configs WHERE id=$1", f.cfgID).Scan(&f.accountID))
	run, err := store.SaveLadderRun(ctx, &LadderRunDB{ConfigID: &f.cfgID, Status: ladder.RunStatusPlanned, StartedAt: time.Now().UTC(), TotalHourlyCommit: float64(n), Plan: []byte(`{}`)})
	require.NoError(t, err)
	f.runID = run.ID
	tranches := make([]LadderTrancheDB, n)
	for i := range tranches {
		tranches[i] = f.tranche(float64(i%7+1) / 8)
	}
	require.NoError(t, store.SaveLadderTranches(ctx, tranches))
	f.amendID = tranches[0].ID
	_, err = store.db.Exec(ctx, `INSERT INTO ladder_runs(config_id,status,created_at)
 SELECT $1,'planned',NOW() FROM generate_series(1,$2::int)`, f.cfgID, n-1)
	require.NoError(t, err)
	return f
}

func (f *ladderTimelineFixture) tranche(amount float64) LadderTrancheDB {
	return LadderTrancheDB{
		ID: uuid.NewString(), ConfigID: &f.cfgID, RunID: &f.runID,
		LayerType: ladder.LayerComputeSP, Term: ladder.Term1Year, PaymentOption: ladder.PaymentNoUpfront,
		Status: ladder.TrancheStatusScheduled, AmountUSDHr: amount, ScheduledDate: f.date,
	}
}

// amend lowers the fixture's editable tranche through the real amend path,
// which always allows a reduction.
func (f *ladderTimelineFixture) amend(ctx context.Context, revision int64, amount string) error {
	_, err := f.store.AmendLadderTranche(ctx, f.amendID, f.accountID, "aws", "timeline-test", LadderAmendment{
		ExpectedRevision: &revision, ScheduledDate: f.date, AmountUSDHr: amount,
	})
	return err
}

// TestLadderTimelineLoadsWholeScopeInOneRead pins the read scaling half of
// #668: a client load is one statement whatever the history length (it was
// ceil(n/100) statements, each aggregating all n rows), and the rows and
// totals it gets are exactly the scope's.
func TestLadderTimelineLoadsWholeScopeInOneRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := seedLadderTimelineFixture(ctx, t, 250)
	events, err := loadLadderTimelineLikeClient(ctx, f.store, f.accountID, false, nil)
	require.NoError(t, err)
	require.Equal(t, 1, events.rounds, "the whole tranche timeline must come back in one read")
	var wantIDs []string
	var wantTotal string
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT array_agg(id::text ORDER BY created_at, id), SUM(amount_usd_hr)::text
 FROM ladder_tranches WHERE config_id=$1`, f.cfgID).Scan(&wantIDs, &wantTotal))
	require.Equal(t, wantIDs, events.ids, "same rows in the same (created_at, id) order")
	require.Equal(t, wantTotal, events.total)
	runs, err := loadLadderTimelineLikeClient(ctx, f.store, f.accountID, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, runs.rounds, "the whole run timeline must come back in one read")
	require.NoError(t, f.store.db.QueryRow(ctx, `SELECT array_agg(id::text ORDER BY created_at, id) FROM ladder_runs WHERE config_id=$1`, f.cfgID).Scan(&wantIDs))
	require.Equal(t, wantIDs, runs.ids)
}

// TestLadderTimelineLoadToleratesMidLoadChanges replays what used to fail the
// whole timeline: an amendment, a new tranche or a new run committed while
// the client was between two page requests.
func TestLadderTimelineLoadToleratesMidLoadChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := seedLadderTimelineFixture(ctx, t, 250)
	for _, tc := range []struct {
		change func() error
		name   string
		runs   bool
	}{
		{name: "amendment", change: func() error { return f.amend(ctx, 0, "0.100000") }},
		{name: "new tranche", change: func() error { return f.store.SaveLadderTranches(ctx, []LadderTrancheDB{f.tranche(2)}) }},
		{name: "new run", runs: true, change: func() error {
			_, err := f.store.SaveLadderRun(ctx, &LadderRunDB{ConfigID: &f.cfgID, Status: ladder.RunStatusPlanned, StartedAt: time.Now().UTC(), Plan: []byte(`{}`)})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := loadLadderTimelineLikeClient(ctx, f.store, f.accountID, tc.runs, nil)
			require.NoError(t, err)
			changed := false
			load, err := loadLadderTimelineLikeClient(ctx, f.store, f.accountID, tc.runs, func() {
				if !changed {
					changed = true
					require.NoError(t, tc.change())
				}
			})
			require.NoError(t, err, "a change between page requests must not fail the load")
			if !changed { // nothing between requests: the load must still see a full scope
				require.NoError(t, tc.change())
			}
			after, err := loadLadderTimelineLikeClient(ctx, f.store, f.accountID, tc.runs, nil)
			require.NoError(t, err)
			require.True(t, len(after.ids) != len(before.ids) || after.total != before.total, "the change must be visible to the next load")
			require.GreaterOrEqual(t, len(load.ids), len(before.ids))
		})
	}
}

// TestLadderTimelineLoadIsSnapshotConsistentUnderConcurrentWrites runs client
// loads while another connection keeps amending the timeline and adding
// tranches and runs. Every load must be internally consistent, and the loads
// must have observed the writes (the totals move between loads).
func TestLadderTimelineLoadIsSnapshotConsistentUnderConcurrentWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f := seedLadderTimelineFixture(ctx, t, 450)
	stop := make(chan struct{})
	writes := make(chan error, 1)
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		writes <- f.writeUntil(ctx, stop)
	}()
	totals := map[string]bool{}
	var loadErr error
	for i := 0; i < 40 && loadErr == nil; i++ {
		for _, runs := range []bool{false, true} {
			load, err := loadLadderTimelineLikeClient(ctx, f.store, f.accountID, runs, nil)
			if err != nil {
				loadErr = err
				break
			}
			totals[fmt.Sprintf("%t/%d/%s", runs, len(load.ids), load.total)] = true
		}
	}
	close(stop)
	writer.Wait()
	require.NoError(t, <-writes)
	require.NoError(t, loadErr, "every load must come from one snapshot")
	require.Greater(t, len(totals), 4, "the writer must have changed the timeline between loads")
}

// writeUntil keeps lowering the editable tranche by one micro-dollar and
// adding a tranche and a run until stop closes.
func (f *ladderTimelineFixture) writeUntil(ctx context.Context, stop <-chan struct{}) error {
	for revision := int64(0); ; revision++ {
		select {
		case <-stop:
			return nil
		default:
		}
		if err := f.amend(ctx, revision, fmt.Sprintf("0.%06d", 124999-revision)); err != nil {
			return fmt.Errorf("amend %d: %w", revision, err)
		}
		if err := f.store.SaveLadderTranches(ctx, []LadderTrancheDB{f.tranche(0.25)}); err != nil {
			return err
		}
		if _, err := f.store.SaveLadderRun(ctx, &LadderRunDB{ConfigID: &f.cfgID, Status: ladder.RunStatusPlanned, StartedAt: time.Now().UTC(), Plan: []byte(`{}`)}); err != nil {
			return err
		}
	}
}
