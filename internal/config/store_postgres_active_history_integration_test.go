//go:build integration

package config

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/stretchr/testify/require"
)

// legacyActivePredicate is GetActivePurchaseHistory's expiry predicate before
// issue #33. The tests below hold the indexable replacement to exactly the
// rows this one returned.
const legacyActivePredicate = "term > 0 AND timestamp + make_interval(hours => term * 8760) >= $1 AND revoked_at IS NULL"

const activeExpiryIndex = "idx_purchase_history_active_expiry"

type activeHistoryRow struct {
	id      string
	account string
	ts      time.Time
	term    int
	revoked bool
}

// activeHistoryBoundaryRows returns rows around every expiry edge: exactly at
// asOf, one microsecond either side, a calendar year that crosses a leap day
// (8760 hours is not one calendar year there), timestamps written in a DST zone,
// non-positive terms, revoked rows and a purchase dated after asOf.
func activeHistoryBoundaryRows(asOf time.Time) []activeHistoryRow {
	year := 8760 * time.Hour
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	us := time.Microsecond
	return []activeHistoryRow{
		{"t1-exact", "111111111111", asOf.Add(-year), 1, false},
		{"t1-1us-before", "111111111111", asOf.Add(-year - us), 1, false},
		{"t1-1us-after", "111111111111", asOf.Add(-year + us), 1, false},
		{"t3-exact", "222222222222", asOf.Add(-3 * year), 3, false},
		{"t3-1us-before", "222222222222", asOf.Add(-3*year - us), 3, false},
		{"t3-1us-after", "222222222222", asOf.Add(-3*year + us), 3, false},
		{"t1-calendar-year", "111111111111", asOf.AddDate(-1, 0, 0), 1, false},
		{"t3-calendar-years", "222222222222", asOf.AddDate(-3, 0, 0), 3, false},
		{"t1-dst-zone", "333333333333", asOf.Add(-year).In(ny), 1, false},
		{"t1-dst-zone-1us-before", "333333333333", asOf.Add(-year - us).In(ny), 1, false},
		{"t0", "111111111111", asOf, 0, false},
		{"t-negative", "111111111111", asOf, -1, false},
		{"t1-revoked", "111111111111", asOf.Add(-time.Hour), 1, true},
		{"t1-future", "222222222222", asOf.Add(48 * time.Hour), 1, false},
		{"t1-long-expired", "333333333333", asOf.AddDate(-10, 0, 0), 1, false},
		{"t50", "333333333333", asOf.AddDate(-40, 0, 0), 50, false},
	}
}

func insertActiveHistoryRows(ctx context.Context, t *testing.T, conn *database.Connection, rows []activeHistoryRow) {
	t.Helper()
	for _, r := range rows {
		var revokedAt *time.Time
		if r.revoked {
			at := r.ts.Add(time.Minute)
			revokedAt = &at
		}
		_, err := conn.Exec(ctx, `
			INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region,
			                              resource_type, term, payment, ramp_step, revoked_at)
			VALUES ($1, $2, $3, 'aws', 'ec2', 'us-east-1', 'm5.large', $4, 'all-upfront', 0, $5)`,
			r.account, r.id, r.ts, r.term, revokedAt)
		require.NoError(t, err, r.id)
	}
}

// legacyActiveIDs runs the pre-#33 predicate with the same account scope.
func legacyActiveIDs(ctx context.Context, t *testing.T, conn *database.Connection, asOf time.Time, externalIDsByProvider map[string][]string) []string {
	t.Helper()
	conds, args := appendAccountPredicate([]string{legacyActivePredicate}, []any{asOf}, nil, externalIDsByProvider)
	rows, err := conn.Query(ctx, "SELECT purchase_id FROM purchase_history WHERE "+strings.Join(conds, " AND "), args...)
	require.NoError(t, err)
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	sort.Strings(ids)
	return ids
}

func recordIDs(records []PurchaseHistoryRecord) []string {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.PurchaseID)
	}
	sort.Strings(ids)
	return ids
}

// TestGetActivePurchaseHistory_MatchesLegacyPredicate checks that the
// indexable expiry predicate (issue #33) returns exactly the rows the old
// make_interval predicate did. Every row's own expiry, one microsecond either
// side of it, and asOf values given in a non-UTC zone are all compared, both
// unscoped and account-scoped.
func TestGetActivePurchaseHistory_MatchesLegacyPredicate(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	ctx := context.Background()

	// Microsecond precision, and 2024-02-29 falls inside both the 1-year and
	// the 3-year calendar window, so a calendar year is longer than 8760 hours.
	base := time.Date(2024, 3, 1, 12, 0, 0, 123456000, time.UTC)
	rows := activeHistoryBoundaryRows(base)
	insertActiveHistoryRows(ctx, t, conn, rows)

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	asOfs := []time.Time{base, base.In(tokyo), base.Add(-time.Hour), base.AddDate(1, 0, 0)}
	for _, r := range rows {
		if r.term <= 0 {
			continue
		}
		expiry := r.ts.Add(time.Duration(r.term) * 8760 * time.Hour)
		asOfs = append(asOfs, expiry.Add(-time.Microsecond), expiry, expiry.Add(time.Microsecond))
	}

	scopes := []map[string][]string{
		nil,
		{"aws": {"111111111111"}},
		{"": {"222222222222"}, "aws": {"333333333333"}},
	}
	sawActive, sawExcluded := false, false
	for _, scope := range scopes {
		for _, asOf := range asOfs {
			want := legacyActiveIDs(ctx, t, conn, asOf, scope)
			got, getErr := store.GetActivePurchaseHistory(ctx, asOf, nil, scope)
			require.NoError(t, getErr)
			require.Equal(t, want, recordIDs(got), "asOf=%s scope=%v", asOf.Format(time.RFC3339Nano), scope)
			sawActive = sawActive || len(want) > 0
			sawExcluded = sawExcluded || len(want) < len(rows)
		}
	}
	require.True(t, sawActive && sawExcluded, "the fixture must exercise both outcomes")

	// The exact-boundary rows pin the inclusive edge, independent of the legacy query.
	got, err := store.GetActivePurchaseHistory(ctx, base, nil, nil)
	require.NoError(t, err)
	ids := recordIDs(got)
	for _, id := range []string{"t1-exact", "t1-1us-after", "t3-exact", "t3-1us-after", "t1-dst-zone", "t50", "t1-future"} {
		require.Contains(t, ids, id)
	}
	for _, id := range []string{"t1-1us-before", "t3-1us-before", "t1-dst-zone-1us-before", "t1-calendar-year", "t3-calendar-years", "t0", "t-negative", "t1-revoked", "t1-long-expired"} {
		require.NotContains(t, ids, id)
	}
}

// TestGetActivePurchaseHistory_SessionTimeZoneIndependent runs the exact query
// under a session TimeZone with DST: the epoch expression must not depend on it.
func TestGetActivePurchaseHistory_SessionTimeZoneIndependent(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	ctx := context.Background()

	base := time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC) // US DST starts this day
	rows := activeHistoryBoundaryRows(base)
	insertActiveHistoryRows(ctx, t, conn, rows)

	pc, err := conn.Pool().Acquire(ctx)
	require.NoError(t, err)
	defer pc.Release()
	_, err = pc.Exec(ctx, "SET TimeZone = 'America/New_York'")
	require.NoError(t, err)
	defer func() { _, _ = pc.Exec(ctx, "RESET TimeZone") }()

	query, args := activePurchaseHistoryQuery(base, nil, nil)
	r, err := pc.Query(ctx, "SELECT purchase_id FROM ("+query+") q", args...)
	require.NoError(t, err)
	var got []string
	for r.Next() {
		var id string
		require.NoError(t, r.Scan(&id))
		got = append(got, id)
	}
	require.NoError(t, r.Err())
	sort.Strings(got)
	require.Equal(t, legacyActiveIDs(ctx, t, conn, base, nil), got)
}

// TestGetActivePurchaseHistory_UsesExpiryIndex is the issue #33 regression: on
// a table where nearly all history has expired, the planner must reach the
// active rows through idx_purchase_history_active_expiry rather than reading
// and filtering every row. Before the fix the predicate could not use any index.
func TestGetActivePurchaseHistory_UsesExpiryIndex(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	ctx := context.Background()

	asOf := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	// 50,000 purchases spread over the 20 years before asOf, alternating 1y and
	// 3y terms: about 10% are still active.
	_, err := conn.Exec(ctx, `
		INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region,
		                              resource_type, term, payment)
		SELECT lpad((g % 50)::text, 12, '0'), 'bulk-' || g,
		       $1::timestamptz - interval '20 years' + (g::double precision / 50000) * interval '20 years',
		       'aws', 'ec2', 'us-east-1', 'm5.large', CASE WHEN g % 2 = 0 THEN 1 ELSE 3 END, 'all-upfront'
		FROM generate_series(1, 50000) g`, asOf)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "ANALYZE purchase_history")
	require.NoError(t, err)

	for _, scope := range []map[string][]string{nil, {"aws": {"000000000007"}}} {
		query, args := activePurchaseHistoryQuery(asOf, nil, scope)
		var plan json.RawMessage
		require.NoError(t, conn.Pool().QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+query, args...).Scan(&plan))
		require.True(t, planUsesIndex(t, plan, activeExpiryIndex),
			"scope=%v: the active filter must use %s, got plan:\n%s", scope, activeExpiryIndex, plan)
	}
}

// planUsesIndex reports whether any node of an EXPLAIN (FORMAT JSON) plan
// scans the named index with it as an index condition.
func planUsesIndex(t *testing.T, raw json.RawMessage, index string) bool {
	t.Helper()
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &plans))
	var walk func(node map[string]any) bool
	walk = func(node map[string]any) bool {
		if node["Index Name"] == index && fmt.Sprint(node["Index Cond"]) != "<nil>" {
			return true
		}
		children, _ := node["Plans"].([]any)
		for _, c := range children {
			if m, ok := c.(map[string]any); ok && walk(m) {
				return true
			}
		}
		return false
	}
	return len(plans) == 1 && walk(plans[0].Plan)
}
