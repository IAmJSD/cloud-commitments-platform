package config

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

// ladderTimelineWholeScope is more rows than the 100-row pages the timeline
// used to return, so a read that still paged would truncate it.
const ladderTimelineWholeScope = 250

func TestLadderTimelineReturnsWholeScopeFromOneStatement(t *testing.T) {
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer pool.Close()
	store := &PostgresStore{db: pool}
	stamp := time.Now().UTC()
	events := make([]LadderTimelineEvent, ladderTimelineWholeScope)
	for i := range events {
		events[i] = LadderTimelineEvent{ID: uuid.NewString(), CreatedAt: stamp, AmountUSDHr: "0.123456", Revision: int64(i)}
	}
	raw, err := json.Marshal(events)
	require.NoError(t, err)
	scope := []byte(`[["0.123456",null,null]]`)
	pool.ExpectQuery(regexp.QuoteMeta(ladderTimelineQuery)).
		WithArgs("account", "aws", nil, nil).
		WillReturnRows(pgxmock.NewRows([]string{"count", "sum", "page", "scope"}).AddRow(int64(ladderTimelineWholeScope), "30.864000", raw, scope))
	page, err := store.ListLadderTimeline(context.Background(), "account", "aws", nil)
	require.NoError(t, err)
	require.Len(t, page.Events, ladderTimelineWholeScope, "every scoped row comes back from the one statement")
	require.Equal(t, int64(ladderTimelineWholeScope), page.TotalCount)
	require.Equal(t, "30.864000", page.TotalUSDHr)
	require.Equal(t, events[ladderTimelineWholeScope-1].ID, page.Events[ladderTimelineWholeScope-1].ID)
	require.Equal(t, int64(1), page.Events[1].Revision)
	wire, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "next_cursor", "the client must never be told to fetch a second page")
	require.NoError(t, pool.ExpectationsWereMet(), "exactly one statement")
}

func TestLadderTimelineQueryFailureIsNotEmpty(t *testing.T) {
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer pool.Close()
	store := &PostgresStore{db: pool}
	pool.ExpectQuery(regexp.QuoteMeta(ladderTimelineQuery)).
		WithArgs("account", "aws", nil, nil).
		WillReturnError(errors.New("database unavailable"))
	page, err := store.ListLadderTimeline(context.Background(), "account", "aws", nil)
	require.ErrorContains(t, err, "database unavailable")
	require.Nil(t, page)
	require.NoError(t, pool.ExpectationsWereMet())
}

func TestLadderTimelineRunsWholeScopeCursorAndFailure(t *testing.T) {
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer pool.Close()
	store := &PostgresStore{db: pool}
	stamp := time.Now().UTC()
	runs := make([]LadderTimelineRun, ladderTimelineWholeScope)
	for i := range runs {
		runs[i] = LadderTimelineRun{ID: uuid.NewString(), CreatedAt: stamp}
	}
	raw, err := json.Marshal(runs)
	require.NoError(t, err)
	pool.ExpectQuery(regexp.QuoteMeta(ladderTimelineRunsQuery)).
		WithArgs("account", "aws", nil, nil).
		WillReturnRows(pgxmock.NewRows([]string{"count", "page"}).AddRow(int64(ladderTimelineWholeScope), raw))
	page, err := store.ListLadderTimelineRuns(context.Background(), "account", "aws", nil)
	require.NoError(t, err)
	require.Len(t, page.Runs, ladderTimelineWholeScope)
	wire, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "next_cursor")
	// A cursor from the earlier paginated API is still passed through as the bound.
	cursor := &LadderTimelineCursor{CreatedAt: stamp, ID: runs[99].ID}
	pool.ExpectQuery(regexp.QuoteMeta(ladderTimelineRunsQuery)).
		WithArgs("account", "aws", stamp, runs[99].ID).
		WillReturnError(errors.New("database unavailable"))
	page, err = store.ListLadderTimelineRuns(context.Background(), "account", "aws", cursor)
	require.ErrorContains(t, err, "database unavailable")
	require.Nil(t, page)
	require.NoError(t, pool.ExpectationsWereMet())
}

func TestLadderTimelineOnDemandFromOneStatement(t *testing.T) {
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	defer pool.Close()
	store := &PostgresStore{db: pool}
	commit := LadderAmountCommitment
	rate := "2/3"
	page := []LadderTimelineEvent{
		{ID: uuid.NewString(), AmountUSDHr: "1.000001", AmountBasis: &commit, CommitmentRate: &rate},
		{ID: uuid.NewString(), AmountUSDHr: "4.000000"},
	}
	raw, err := json.Marshal(page)
	require.NoError(t, err)
	// Two rows of exactly 1.5000015 on-demand: per-row ceilings sum to 3.000004, not the rounded exact sum 3.000003.
	scope := []byte(`[["1.000001","commitment","2/3"],["1.000001","commitment","2/3"],["4.000000",null,null]]`)
	pool.ExpectQuery(regexp.QuoteMeta(ladderTimelineQuery)).
		WithArgs("account", "aws", nil, nil).
		WillReturnRows(pgxmock.NewRows([]string{"count", "sum", "page", "scope"}).AddRow(int64(3), "6.000002", raw, scope))
	got, err := store.ListLadderTimeline(context.Background(), "account", "aws", nil)
	require.NoError(t, err)
	require.Equal(t, "6.000002", got.TotalUSDHr)
	require.Equal(t, "7.000004", got.TotalODUSDHr)
	require.Equal(t, "1.500002", got.Events[0].ODUSDHr)
	require.Equal(t, "1.000001", got.Events[0].AmountUSDHr, "native amount stays native")
	require.Equal(t, "4.000000", got.Events[1].ODUSDHr, "NULL basis is assumed on-demand")
	require.NoError(t, pool.ExpectationsWereMet(), "exactly one statement")
}

func TestLadderTimelineMalformedRateFailsLoud(t *testing.T) {
	for name, scope := range map[string]string{
		"unparseable": `[["1.000000","commitment","abc"]]`,
		"zero":        `[["1.000000","commitment","0"]]`,
		"above one":   `[["1.000000","commitment","3/2"]]`,
		"unknown":     `[["1.000000","mystery",null]]`,
	} {
		t.Run(name, func(t *testing.T) {
			pool, err := pgxmock.NewPool()
			require.NoError(t, err)
			defer pool.Close()
			store := &PostgresStore{db: pool}
			pool.ExpectQuery(regexp.QuoteMeta(ladderTimelineQuery)).
				WithArgs("account", "aws", nil, nil).
				WillReturnRows(pgxmock.NewRows([]string{"count", "sum", "page", "scope"}).AddRow(int64(1), "1.000000", []byte(`[]`), []byte(scope)))
			got, err := store.ListLadderTimeline(context.Background(), "account", "aws", nil)
			require.ErrorIs(t, err, ErrLadderTimelineRate)
			require.Nil(t, got)
		})
	}
}
