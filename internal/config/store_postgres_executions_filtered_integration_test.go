//go:build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_GetExecutionsByStatusesFiltered runs the issue #632 SQL
// pushdown over every account shape an execution can have: cloud_account_id
// set, NULL with the account only on the recommendations (one account,
// several, an external number, none), and a non-array recommendations value.
func TestIntegration_GetExecutionsByStatusesFiltered(t *testing.T) {
	ctx := context.Background()
	f := newHistorySummaryFixture(ctx, t)
	a, b, user := f.accountA, f.accountB, f.userID
	legacy := "ext-legacy"
	day := func(n int) time.Time { return f.base.Add(time.Duration(n) * 24 * time.Hour) }
	recA := []RecommendationRecord{{Provider: "aws", CloudAccountID: &a}}
	recB := []RecommendationRecord{{Provider: "azure", CloudAccountID: &b}}
	mixed := []RecommendationRecord{{Provider: "aws", CloudAccountID: &a}, {Provider: "azure", CloudAccountID: &b}}

	ids := map[string]string{
		"onA":      f.saveExecution(ctx, t, day(1), "pending", "", &a, nil, recA),
		"onB":      f.saveExecution(ctx, t, day(2), "pending", "", &b, nil, recB),
		"recsA":    f.saveExecution(ctx, t, day(3), "pending", "", nil, nil, recA),
		"mixedMy":  f.saveExecution(ctx, t, day(4), "pending", "", nil, &user, mixed),
		"mixedAny": f.saveExecution(ctx, t, day(5), "pending", "", nil, nil, mixed),
		"noRecs":   f.saveExecution(ctx, t, day(6), "pending", "", nil, nil, nil),
		"external": f.saveExecution(ctx, t, day(7), "pending", "", nil, nil, []RecommendationRecord{{Provider: "aws", CloudAccountID: &legacy}}),
	}
	f.saveExecution(ctx, t, day(8), "failed", "boom", &a, nil, recA) // outside the requested statuses

	names := func(q ExecutionsByStatusesQuery) []string {
		t.Helper()
		q.Statuses = []string{"pending"}
		if q.Limit == 0 {
			q.Limit = 100
		}
		got, err := f.store.GetExecutionsByStatusesFiltered(ctx, q)
		require.NoError(t, err)
		out := make([]string, 0, len(got))
		for i := range got {
			for name, id := range ids {
				if id == got[i].ExecutionID {
					out = append(out, name)
				}
			}
		}
		require.Len(t, out, len(got), "every row is a seeded execution")
		return out
	}
	start, end := day(2), day(4)

	tests := []struct {
		name string
		q    ExecutionsByStatusesQuery
		want []string
	}{
		{"no filter", ExecutionsByStatusesQuery{}, []string{"external", "noRecs", "mixedAny", "mixedMy", "recsA", "onB", "onA"}},
		{"provider aws", ExecutionsByStatusesQuery{Provider: "aws"}, []string{"external", "mixedAny", "mixedMy", "recsA", "onA"}},
		{"provider azure", ExecutionsByStatusesQuery{Provider: "azure"}, []string{"mixedAny", "mixedMy", "onB"}},
		{"account A", ExecutionsByStatusesQuery{AccountIDs: []string{a}}, []string{"mixedAny", "mixedMy", "recsA", "onA"}},
		{"external number on recs", ExecutionsByStatusesQuery{AccountIDs: []string{legacy}}, []string{"external"}},
		{"date bounds inclusive", ExecutionsByStatusesQuery{Start: &start, End: &end}, []string{"mixedMy", "recsA", "onB"}},
		{"scope A", ExecutionsByStatusesQuery{Scope: &ExecutionScope{AccountIDs: []string{a}, UserID: user}},
			[]string{"mixedAny", "mixedMy", "recsA", "onA"}},
		{"scope nothing keeps own unattributed rows", ExecutionsByStatusesQuery{Scope: &ExecutionScope{UserID: user}}, []string{"mixedMy"}},
		{"scope with no user matches a NULL creator", ExecutionsByStatusesQuery{Scope: &ExecutionScope{}},
			[]string{"external", "noRecs", "mixedAny", "recsA"}},
		{"filters and scope combine", ExecutionsByStatusesQuery{Provider: "azure", Scope: &ExecutionScope{AccountIDs: []string{b}, UserID: user}},
			[]string{"mixedAny", "mixedMy", "onB"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, names(tt.q))
		})
	}

	// The cap applies after the scope: the newer rows outside scope B do not
	// push B's own row out of a three-row read, as they do unscoped.
	t.Run("limit counts only rows in scope", func(t *testing.T) {
		assert.Equal(t, []string{"external", "noRecs", "mixedAny"}, names(ExecutionsByStatusesQuery{Limit: 3}))
		assert.Equal(t, []string{"mixedAny", "mixedMy", "onB"}, names(ExecutionsByStatusesQuery{Limit: 3,
			Scope: &ExecutionScope{AccountIDs: []string{b}, UserID: "00000000-0000-4000-8000-000000000000"}}))
	})

	t.Run("GetExecutionsByStatuses is the unfiltered query", func(t *testing.T) {
		got, err := f.store.GetExecutionsByStatuses(ctx, []string{"pending"}, 100)
		require.NoError(t, err)
		assert.Len(t, got, len(ids))
	})
}
