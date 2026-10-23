//go:build integration

package config

// Real PostgreSQL tests for the operator resolve action (platform#118 PR5b).

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type resolveNode struct {
	status       string
	noSubmission bool
	age          time.Duration // how long ago the row last changed
}

type resolveRig struct {
	store     *PostgresStore
	trancheID string
	accountID string
	external  string
}

// newResolveRig fires one tranche whose execution chain is nodes (head first,
// each linked to the next by retry_execution_id), as the executor and Retry do.
func newResolveRig(ctx context.Context, t *testing.T, store *PostgresStore, nodes []resolveNode) *resolveRig {
	t.Helper()
	ext := "73" + uuid.NewString()[:10]
	cfgID := seedLadderConfigWithExtID(ctx, t, store, ext)
	r := &resolveRig{store: store, external: ext}
	require.NoError(t, store.db.QueryRow(ctx, `SELECT cloud_account_id::text FROM ladder_configs WHERE id = $1`, cfgID).Scan(&r.accountID))
	tr := lifecycleTranche(cfgID, 5, ladder.TrancheStatusScheduled, time.Now().Add(-time.Hour))
	require.NoError(t, store.SaveLadderTranches(ctx, []LadderTrancheDB{tr}))
	r.trancheID = tr.ID
	var ids []string
	_, err := store.ClaimDueLadderTranches(ctx, cfgID, time.Now(), 1, func(ctx context.Context, tx pgx.Tx, _ LadderTrancheDB) (string, error) {
		id, err := insertResolveNode(ctx, tx, tr.ID, nodes[0])
		ids = append(ids, id)
		return id, err
	})
	require.NoError(t, err)
	for _, n := range nodes[1:] {
		require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
			id, err := insertResolveNode(ctx, tx, tr.ID, n)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE purchase_executions SET retry_execution_id = $2 WHERE execution_id = $1`, ids[len(ids)-1], id)
			ids = append(ids, id)
			return err
		}))
	}
	for i, n := range nodes { // age rows after linking, with the updated_at trigger off
		_, err := store.db.Exec(ctx, `ALTER TABLE purchase_executions DISABLE TRIGGER update_purchase_executions_updated_at`)
		require.NoError(t, err)
		_, err = store.db.Exec(ctx, `UPDATE purchase_executions SET updated_at = now() - make_interval(secs => $2) WHERE execution_id = $1`, ids[i], n.age.Seconds())
		require.NoError(t, err)
		_, err = store.db.Exec(ctx, `ALTER TABLE purchase_executions ENABLE TRIGGER update_purchase_executions_updated_at`)
		require.NoError(t, err)
	}
	return r
}

func insertResolveNode(ctx context.Context, tx pgx.Tx, key string, n resolveNode) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO purchase_executions (scheduled_date, status, no_submission, error, idempotency_key)
		VALUES (NOW(), $1, $2, '', $3) RETURNING execution_id::text`, n.status, n.noSubmission, key).Scan(&id)
	return id, err
}

func (r *resolveRig) req(reason string, ack *LadderCloudCheck) LadderResolveFailed {
	return LadderResolveFailed{TrancheID: r.trancheID, AccountID: r.accountID, StatusReason: reason,
		Acknowledge: ack != nil, Check: ack, Now: time.Now().UTC(), StaleAfter: 6 * time.Hour}
}

func (r *resolveRig) check(at time.Time) *LadderCloudCheck {
	return &LadderCloudCheck{AccountID: r.accountID, Region: "us-east-1", Service: string(ladder.LayerComputeSP), CheckedAt: at}
}

func TestResolveLadderTrancheFailed_ProvenChain(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	for name, nodes := range map[string][]resolveNode{
		"no_submission head":   {{"failed", true, time.Minute}},
		"canceled after proof": {{"failed", true, time.Hour}, {"canceled", false, time.Minute}},
		"expired":              {{"expired", false, time.Minute}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newResolveRig(ctx, t, store, nodes)
			res, err := r.store.ResolveLadderTrancheFailed(ctx, r.req("operator:failed:proven: reason=x", nil))
			require.NoError(t, err)
			assert.False(t, res.Replayed)
			assert.Equal(t, ladder.TrancheStatusFailed, trancheStatus(ctx, t, store, r.trancheID))
			var reason string
			require.NoError(t, store.db.QueryRow(ctx, `SELECT status_reason FROM ladder_tranches WHERE id = $1`, r.trancheID).Scan(&reason))
			assert.Equal(t, "operator:failed:proven: reason=x", reason)
		})
	}
}

// Anything that may still run, may have bought, or is not a known status blocks
// the resolve, and the tranche stays fired.
func TestResolveLadderTrancheFailed_Refusals(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	blocked := map[string][]resolveNode{}
	for _, s := range []string{"pending", "notified", "scheduled", "approved", "running"} {
		blocked["live "+s+" tail"] = []resolveNode{{"failed", true, 8 * time.Hour}, {s, false, 7 * time.Hour}}
		blocked["live "+s+" head"] = []resolveNode{{s, false, 7 * time.Hour}}
	}
	blocked["completed head"] = []resolveNode{{"completed", false, 7 * time.Hour}}
	blocked["partially_completed"] = []resolveNode{{"partially_completed", false, 7 * time.Hour}}
	blocked["completed then failed retry"] = []resolveNode{{"completed", false, 8 * time.Hour}, {"failed", true, 7 * time.Hour}}
	long := make([]resolveNode, LadderChainMaxDepth+1)
	for i := range long {
		long[i] = resolveNode{"failed", true, 8 * time.Hour}
	}
	blocked["chain longer than the depth cap"] = long
	for name, nodes := range blocked {
		t.Run(name, func(t *testing.T) {
			r := newResolveRig(ctx, t, store, nodes)
			c := r.check(time.Now().Add(-time.Minute))
			_, err := r.store.ResolveLadderTrancheFailed(ctx, r.req("operator:failed:unverified: x", c))
			require.ErrorIs(t, err, ErrLadderResolveConflict)
			assert.Equal(t, ladder.TrancheStatusFired, trancheStatus(ctx, t, store, r.trancheID))
		})
	}
}

func TestResolveLadderTrancheFailed_UnknownOutcome(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	old := []resolveNode{{"failed", false, 7 * time.Hour}} // reaped: no proof
	cases := []struct {
		name  string
		nodes []resolveNode
		tweak func(*LadderResolveFailed, *resolveRig)
		want  error
	}{
		{"no acknowledgement", old, func(q *LadderResolveFailed, _ *resolveRig) { q.Acknowledge, q.Check = false, nil }, ErrLadderResolveConflict},
		{"acknowledged without check", old, func(q *LadderResolveFailed, _ *resolveRig) { q.Check = nil }, ErrLadderResolveConflict},
		{"changed 5m ago though created long ago", []resolveNode{{"failed", false, 5 * time.Minute}}, nil, ErrLadderResolveConflict},
		{"unproven predecessor with proven tail", []resolveNode{{"failed", false, 8 * time.Hour}, {"failed", true, 7 * time.Hour}}, nil, nil},
		{"wrong account", old, func(q *LadderResolveFailed, _ *resolveRig) { q.Check.AccountID = "acct-other" }, ErrLadderResolveInvalid},
		{"wrong service", old, func(q *LadderResolveFailed, _ *resolveRig) { q.Check.Service = "rds" }, ErrLadderResolveInvalid},
		{"checked in the future", old, func(q *LadderResolveFailed, _ *resolveRig) { q.Check.CheckedAt = q.Now.Add(time.Hour) }, ErrLadderResolveInvalid},
		{"checked before the last change", old, func(q *LadderResolveFailed, _ *resolveRig) { q.Check.CheckedAt = q.Now.Add(-8 * time.Hour) }, ErrLadderResolveInvalid},
		{"valid", old, nil, nil},
		{"valid with provider account number", old, func(q *LadderResolveFailed, r *resolveRig) { q.Check.AccountID = r.external }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newResolveRig(ctx, t, store, tc.nodes)
			q := r.req("operator:failed:unverified: x", r.check(time.Now().Add(-time.Minute)))
			if tc.tweak != nil {
				tc.tweak(&q, r)
			}
			_, err := r.store.ResolveLadderTrancheFailed(ctx, q)
			if tc.want == nil {
				require.NoError(t, err)
				assert.Equal(t, ladder.TrancheStatusFailed, trancheStatus(ctx, t, store, r.trancheID))
				return
			}
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, ladder.TrancheStatusFired, trancheStatus(ctx, t, store, r.trancheID))
		})
	}
}

func TestResolveLadderTrancheFailed_ReplayAndScope(t *testing.T) {
	ctx := context.Background()
	store := setupLadderStore(ctx, t)
	r := newResolveRig(ctx, t, store, []resolveNode{{"failed", true, time.Minute}})

	t.Run("another account's tranche is not found", func(t *testing.T) {
		q := r.req("operator:failed:proven: reason=x", nil)
		q.AccountID = uuid.NewString()
		_, err := store.ResolveLadderTrancheFailed(ctx, q)
		require.ErrorIs(t, err, ErrLadderResolveNotFound)
		assert.Equal(t, ladder.TrancheStatusFired, trancheStatus(ctx, t, store, r.trancheID))
	})
	t.Run("unknown tranche", func(t *testing.T) {
		q := r.req("operator:failed:proven: reason=x", nil)
		q.TrancheID = uuid.NewString()
		_, err := store.ResolveLadderTrancheFailed(ctx, q)
		require.ErrorIs(t, err, ErrLadderResolveNotFound)
	})
	t.Run("oversized status reason", func(t *testing.T) {
		_, err := store.ResolveLadderTrancheFailed(ctx, r.req(string(make([]byte, ladderStatusReasonMax+1)), nil))
		require.ErrorIs(t, err, ErrLadderResolveInvalid)
	})
	t.Run("repeat is a no-op, a different reason conflicts", func(t *testing.T) {
		first, err := store.ResolveLadderTrancheFailed(ctx, r.req("operator:failed:proven: reason=a", nil))
		require.NoError(t, err)
		assert.False(t, first.Replayed)
		again, err := store.ResolveLadderTrancheFailed(ctx, r.req("operator:failed:proven: reason=a", nil))
		require.NoError(t, err)
		assert.True(t, again.Replayed)
		_, err = store.ResolveLadderTrancheFailed(ctx, r.req("operator:failed:proven: reason=b", nil))
		require.ErrorIs(t, err, ErrLadderResolveConflict)
		var reason string
		require.NoError(t, store.db.QueryRow(ctx, `SELECT status_reason FROM ladder_tranches WHERE id = $1`, r.trancheID).Scan(&reason))
		assert.Equal(t, "operator:failed:proven: reason=a", reason)
	})
	t.Run("a tranche completed elsewhere conflicts", func(t *testing.T) {
		done := newResolveRig(ctx, t, store, []resolveNode{{"completed", false, time.Minute}})
		moved, err := store.TransitionLadderTranche(ctx, done.trancheID, ladder.TrancheStatusFired, ladder.TrancheStatusCompleted, "")
		require.NoError(t, err)
		require.True(t, moved)
		_, err = store.ResolveLadderTrancheFailed(ctx, done.req("operator:failed:proven: reason=x", nil))
		require.ErrorIs(t, err, ErrLadderResolveConflict)
	})
}
