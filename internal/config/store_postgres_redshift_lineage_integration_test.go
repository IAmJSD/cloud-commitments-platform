//go:build integration
// +build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lineageTestHorizon = 24 * time.Hour

var lineageTuple = RedshiftTuple{Region: "us-east-1", NodeType: "ra3.xlplus", Term: 1, Payment: "all-upfront", Count: 2}

// seedLineageRow inserts an execution holding one selected Redshift rec that
// matches lineageTuple, in the given lineage family, status and stamp age.
func seedLineageRow(ctx context.Context, t *testing.T, store *PostgresStore, family, status string, stampAge *time.Duration) *PurchaseExecution {
	t.Helper()
	exec := newNoSubmissionExec(ctx, t, store, status)
	exec.Recommendations = []RecommendationRecord{{
		Provider: "aws", Service: "redshift", Region: lineageTuple.Region, ResourceType: lineageTuple.NodeType,
		Term: lineageTuple.Term, Payment: lineageTuple.Payment, Count: lineageTuple.Count, Selected: true,
	}}
	require.NoError(t, store.SavePurchaseExecution(ctx, exec))
	var stamp *time.Time
	if stampAge != nil {
		ts := time.Now().Add(-*stampAge)
		stamp = &ts
	}
	_, err := store.db.Exec(ctx, `UPDATE purchase_executions SET idempotency_key = $2, first_dispatch_at = $3 WHERE execution_id = $1`,
		exec.ExecutionID, family, stamp)
	require.NoError(t, err)
	return exec
}

func findLineageConflicts(ctx context.Context, t *testing.T, store *PostgresStore, self, family string) []RedshiftLineageConflict {
	t.Helper()
	var got []RedshiftLineageConflict
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = store.FindOpenRedshiftLineageConflictsTx(ctx, tx, self, family, []RedshiftTuple{lineageTuple}, nil, lineageTestHorizon)
		return err
	}))
	return got
}

func ptrDur(d time.Duration) *time.Duration { return &d }

func conflictIDs(cs []RedshiftLineageConflict) []string {
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ExecutionID)
	}
	return ids
}

func TestFindOpenRedshiftLineageConflicts(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	recent, old := ptrDur(time.Hour), ptrDur(48*time.Hour)
	// Rows of one subtest must not leak into the next: they all share a tuple.
	reset := func(t *testing.T) {
		t.Helper()
		_, err := store.db.Exec(ctx, `DELETE FROM purchase_executions`)
		require.NoError(t, err)
	}

	t.Run("same_family_running_other_row_conflicts", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-a", "running", recent)
		other := seedLineageRow(ctx, t, store, "fam-a:acct", "running", recent)
		got := findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-a")
		require.Len(t, got, 1)
		assert.Equal(t, other.ExecutionID, got[0].ExecutionID)
		assert.True(t, got[0].SameFamily)
		assert.Equal(t, "running", got[0].Status)
		require.NotNil(t, got[0].FirstDispatchAt)
		require.Len(t, got[0].Recommendations, 1)
	})
	t.Run("same_family_self_excluded", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-b", "running", recent)
		assert.Empty(t, findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-b"))
	})
	t.Run("same_family_non_running_excluded", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-c", "running", recent)
		for _, st := range []string{"failed", "pending", "approved"} {
			seedLineageRow(ctx, t, store, "fam-c:"+st, st, recent)
		}
		assert.Empty(t, findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-c"))
	})
	t.Run("other_family_running_any_stamp_age_conflicts", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-d", "running", recent)
		other := seedLineageRow(ctx, t, store, "fam-d-other", "running", old)
		got := findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-d")
		require.Len(t, got, 1)
		assert.Equal(t, []string{other.ExecutionID}, conflictIDs(got))
		assert.False(t, got[0].SameFamily)
	})
	t.Run("other_family_pending_with_stamp_conflicts", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-e", "running", recent)
		other := seedLineageRow(ctx, t, store, "fam-e-other", "pending", recent)
		assert.Equal(t, []string{other.ExecutionID}, conflictIDs(findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-e")))
	})
	t.Run("paused_conflicts", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-f", "running", recent)
		other := seedLineageRow(ctx, t, store, "fam-f-other", "paused", recent)
		assert.Equal(t, []string{other.ExecutionID}, conflictIDs(findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-f")))
	})
	t.Run("approved_and_scheduled_conflict", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-g", "running", recent)
		a := seedLineageRow(ctx, t, store, "fam-g-a", "approved", recent)
		s := seedLineageRow(ctx, t, store, "fam-g-s", "scheduled", recent)
		n := seedLineageRow(ctx, t, store, "fam-g-n", "notified", recent)
		assert.ElementsMatch(t, []string{a.ExecutionID, s.ExecutionID, n.ExecutionID},
			conflictIDs(findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-g")))
	})
	t.Run("stamp_older_than_H_does_not_conflict_when_not_running", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-h", "running", recent)
		seedLineageRow(ctx, t, store, "fam-h-other", "pending", old)
		assert.Empty(t, findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-h"))
	})
	t.Run("row_without_stamp_ignored", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-i", "running", recent)
		seedLineageRow(ctx, t, store, "fam-i-other", "pending", nil)
		assert.Empty(t, findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-i"))
	})
	t.Run("UnstampedRunningRowIsNotAConflict", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-j", "running", recent)
		seedLineageRow(ctx, t, store, "fam-j-other", "running", nil)
		assert.Empty(t, findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-j"))
	})
	t.Run("row_account_null_falls_back_to_rec_account", func(t *testing.T) {
		reset(t)
		a1, a2 := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
		self := seedLineageRow(ctx, t, store, "fam-k", "running", recent)
		other := seedLineageRow(ctx, t, store, "fam-k-other", "running", recent)
		_, err := store.db.Exec(ctx, `UPDATE purchase_executions SET recommendations = jsonb_set(recommendations, '{0,cloud_account_id}', to_jsonb($2::text)) WHERE execution_id = $1`, other.ExecutionID, a1)
		require.NoError(t, err)
		find := func(acct string) []RedshiftLineageConflict {
			tp := lineageTuple
			tp.CloudAccountID = &acct
			var got []RedshiftLineageConflict
			require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
				var e error
				got, e = store.FindOpenRedshiftLineageConflictsTx(ctx, tx, self.ExecutionID, "fam-k", []RedshiftTuple{tp}, nil, lineageTestHorizon)
				return e
			}))
			return got
		}
		assert.Empty(t, find(a2))
		assert.Equal(t, []string{other.ExecutionID}, conflictIDs(find(a1)))
	})
	t.Run("null_recommendations_json_does_not_error", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-l", "running", recent)
		other := seedLineageRow(ctx, t, store, "fam-l-other", "running", recent)
		_, err := store.db.Exec(ctx, `UPDATE purchase_executions SET recommendations = 'null'::jsonb WHERE execution_id = $1`, other.ExecutionID)
		require.NoError(t, err)
		assert.Empty(t, findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-l"))
	})
	// candidate runs the query for self/family with one tuple and account arg.
	candidate := func(self, family string, tp RedshiftTuple, arg *string) []RedshiftLineageConflict {
		var got []RedshiftLineageConflict
		require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
			var e error
			got, e = store.FindOpenRedshiftLineageConflictsTx(ctx, tx, self, family, []RedshiftTuple{tp}, arg, lineageTestHorizon)
			return e
		}))
		return got
	}
	setRec := func(t *testing.T, id, expr string, args ...any) {
		t.Helper()
		_, err := store.db.Exec(ctx, `UPDATE purchase_executions SET recommendations = `+expr+` WHERE execution_id = $1`, append([]any{id}, args...)...)
		require.NoError(t, err)
	}

	t.Run("tuple_field_mismatch_does_not_conflict", func(t *testing.T) {
		cases := map[string]func(*RedshiftTuple){
			"region":     func(tp *RedshiftTuple) { tp.Region = "eu-west-1" },
			"node_type":  func(tp *RedshiftTuple) { tp.NodeType = "ra3.4xlarge" },
			"term":       func(tp *RedshiftTuple) { tp.Term = 3 },
			"payment":    func(tp *RedshiftTuple) { tp.Payment = "no-upfront" },
			"count_up":   func(tp *RedshiftTuple) { tp.Count = 3 },
			"count_down": func(tp *RedshiftTuple) { tp.Count = 1 },
		}
		for name, mutate := range cases {
			reset(t)
			self := seedLineageRow(ctx, t, store, "fam-m", "running", recent)
			seedLineageRow(ctx, t, store, "fam-m-other", "running", recent)
			tp := lineageTuple
			mutate(&tp)
			assert.Empty(t, candidate(self.ExecutionID, "fam-m", tp, nil), name)
		}
	})
	t.Run("row_count_3_vs_candidate_2_and_reverse_do_not_conflict", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-n", "running", recent)
		other := seedLineageRow(ctx, t, store, "fam-n-other", "running", recent)
		setRec(t, other.ExecutionID, `jsonb_set(recommendations, '{0,count}', '3')`)
		assert.Empty(t, findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-n"))
		tp := lineageTuple
		tp.Count = 3
		assert.Equal(t, []string{other.ExecutionID}, conflictIDs(candidate(self.ExecutionID, "fam-n", tp, nil)))
		tp.Count = 4
		assert.Empty(t, candidate(self.ExecutionID, "fam-n", tp, nil))
		setRec(t, other.ExecutionID, `jsonb_set(recommendations, '{0,count}', '2')`)
		tp.Count = 3
		assert.Empty(t, candidate(self.ExecutionID, "fam-n", tp, nil))
	})
	t.Run("unselected_rec_does_not_conflict", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-o", "running", recent)
		other := seedLineageRow(ctx, t, store, "fam-o-other", "running", recent)
		setRec(t, other.ExecutionID, `jsonb_set(recommendations, '{0,selected}', 'false')`)
		assert.Empty(t, findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-o"))
	})
	t.Run("provider_and_service_filters", func(t *testing.T) {
		reset(t)
		self := seedLineageRow(ctx, t, store, "fam-p", "running", recent)
		az := seedLineageRow(ctx, t, store, "fam-p-az", "running", recent)
		setRec(t, az.ExecutionID, `jsonb_set(jsonb_set(recommendations, '{0,provider}', '"azure"'), '{0,service}', '"data-warehouse"')`)
		other := seedLineageRow(ctx, t, store, "fam-p-ec2", "running", recent)
		setRec(t, other.ExecutionID, `jsonb_set(recommendations, '{0,service}', '"ec2"')`)
		assert.Empty(t, findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-p"))
		aws := seedLineageRow(ctx, t, store, "fam-p-aws", "running", recent)
		setRec(t, aws.ExecutionID, `jsonb_set(recommendations, '{0,service}', '"data-warehouse"')`)
		assert.Equal(t, []string{aws.ExecutionID}, conflictIDs(findLineageConflicts(ctx, t, store, self.ExecutionID, "fam-p")))
	})
	t.Run("account_matrix", func(t *testing.T) {
		a1, a2 := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
		for _, a := range []string{a1, a2} {
			_, err := store.db.Exec(ctx, `INSERT INTO cloud_accounts (id, name, provider, external_id) VALUES ($1::uuid, $1::text, 'aws', $1::text) ON CONFLICT DO NOTHING`, a)
			require.NoError(t, err)
		}
		// Each of the four slots is nil, a1 or a2. The row account, else the rec
		// account, is compared with the tuple account, else the argument.
		slots := []*string{nil, &a1, &a2}
		eff := func(first, second *string) *string {
			if first != nil {
				return first
			}
			return second
		}
		for _, rowA := range slots {
			for _, recA := range slots {
				for _, tupA := range slots {
					for _, argA := range slots {
						reset(t)
						self := seedLineageRow(ctx, t, store, "fam-q", "running", recent)
						other := seedLineageRow(ctx, t, store, "fam-q-other", "running", recent)
						_, err := store.db.Exec(ctx, `UPDATE purchase_executions SET cloud_account_id = $2 WHERE execution_id = $1`, other.ExecutionID, rowA)
						require.NoError(t, err)
						if recA != nil {
							setRec(t, other.ExecutionID, `jsonb_set(recommendations, '{0,cloud_account_id}', to_jsonb($2::text))`, *recA)
						}
						tp := lineageTuple
						tp.CloudAccountID = tupA
						l, r := eff(rowA, recA), eff(tupA, argA)
						want := l == nil || r == nil || *l == *r
						got := candidate(self.ExecutionID, "fam-q", tp, argA)
						assert.Equal(t, want, len(got) == 1, "row=%v rec=%v tuple=%v arg=%v", rowA, recA, tupA, argA)
					}
				}
			}
		}
	})
	t.Run("non_uuid_rec_account_is_a_wildcard_not_an_error", func(t *testing.T) {
		reset(t)
		a1 := "11111111-1111-1111-1111-111111111111"
		self := seedLineageRow(ctx, t, store, "fam-r", "running", recent)
		other := seedLineageRow(ctx, t, store, "fam-r-other", "running", recent)
		setRec(t, other.ExecutionID, `jsonb_set(recommendations, '{0,cloud_account_id}', '"not-a-uuid"')`)
		tp := lineageTuple
		tp.CloudAccountID = &a1
		assert.Equal(t, []string{other.ExecutionID}, conflictIDs(candidate(self.ExecutionID, "fam-r", tp, nil)))
	})
	t.Run("empty_family_is_rejected", func(t *testing.T) {
		reset(t)
		require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
			_, err := store.FindOpenRedshiftLineageConflictsTx(ctx, tx, "x", "", []RedshiftTuple{lineageTuple}, nil, lineageTestHorizon)
			assert.Error(t, err)
			return nil
		}))
	})
}

func TestLockRedshiftTuplesTx_BlocksSameTupleAndSetsReadCommitted(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	other := lineageTuple
	other.Count = 9
	tx1, err := store.db.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx1.Rollback(ctx) }()
	_, err = tx1.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ")
	require.NoError(t, err, "start above read committed so the lock helper's own SET is what is proven")
	require.NoError(t, store.LockRedshiftTuplesTx(ctx, tx1, []RedshiftTuple{lineageTuple, other}))
	var iso string
	require.NoError(t, tx1.QueryRow(ctx, "SHOW transaction_isolation").Scan(&iso))
	assert.Equal(t, "read committed", iso)

	tx2, err := store.db.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx2.Rollback(ctx) }()
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	assert.Error(t, store.LockRedshiftTuplesTx(short, tx2, []RedshiftTuple{lineageTuple}), "a held tuple blocks a second transaction")

	tx3, err := store.db.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx3.Rollback(ctx) }()
	third := lineageTuple
	third.Count = 11
	assert.NoError(t, store.LockRedshiftTuplesTx(ctx, tx3, []RedshiftTuple{third}), "a different tuple does not block")
}
