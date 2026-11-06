package config

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// RedshiftTuple identifies one Redshift reserved-node purchase. Go's dedupe
// window matches on exactly these fields (account is implicit in the client).
type RedshiftTuple struct {
	Region   string `json:"region"`
	NodeType string `json:"node_type"`
	Term     int    `json:"term"`
	Payment  string `json:"payment"`
	Count    int    `json:"count"`
	// CloudAccountID is the rec's own account; nil falls back to the accountID argument.
	CloudAccountID *string `json:"cloud_account_id,omitempty"`
}

// LockKey is the advisory lock key of the tuple. It leaves the account out
// because a NULL account on either side of the conflict query matches any.
func (t RedshiftTuple) LockKey() string {
	return fmt.Sprintf("redshift-tuple:%s|%s|%d|%s|%d", t.Region, t.NodeType, t.Term, t.Payment, t.Count)
}

// LockRedshiftTuplesTx takes a transaction-scoped advisory lock per tuple in
// sorted key order, so two dispatches in different sessions cannot deadlock.
// It must be the first thing run in tx (the isolation level can only be set
// before any query). Parallels planStepLockKeys + LockPlanProvidersTx.
func (s *PostgresStore) LockRedshiftTuplesTx(ctx context.Context, tx pgx.Tx, tuples []RedshiftTuple) error {
	keys := make([]string, 0, len(tuples))
	for _, t := range tuples {
		keys = append(keys, t.LockKey())
	}
	sort.Strings(keys)
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return fmt.Errorf("lock redshift tuples: set isolation: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", ladderLockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("lock redshift tuples: set lock_timeout: %w", err)
	}
	for _, k := range keys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, k); err != nil {
			return fmt.Errorf("lock redshift tuple %q: %w", k, err)
		}
	}
	// The timeout was for the advisory waits only; do not leak it into later statements.
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout TO DEFAULT"); err != nil {
		return fmt.Errorf("lock redshift tuples: reset lock_timeout: %w", err)
	}
	return nil
}

// RedshiftLineageConflict is an open execution whose Redshift rec matches a
// candidate tuple. SameFamily is true when it shares the candidate's lineage
// root (idempotency key up to the first colon).
type RedshiftLineageConflict struct {
	ExecutionID     string
	Status          string
	FirstDispatchAt *time.Time
	SameFamily      bool
	Error           string
	Recommendations []RecommendationRecord
}

// FindOpenRedshiftLineageConflictsTx returns the other open executions that
// could buy or adopt a node for one of tuples, so a second lineage must not
// dispatch. The row side account is the row's, else the rec's; the candidate
// side is the tuple's, else accountID. A NULL on either side matches any; a rec account that is not a uuid counts as NULL.
// A stamped running row is open at any age (the reaper bounds it); the other
// open statuses only while their stamp is within horizon, judged on the
// database clock. Same-family rows conflict only while running.
func (s *PostgresStore) FindOpenRedshiftLineageConflictsTx(ctx context.Context, tx pgx.Tx, selfID, family string, tuples []RedshiftTuple, accountID *string, horizon time.Duration) ([]RedshiftLineageConflict, error) {
	if family == "" {
		return nil, fmt.Errorf("find open redshift lineage conflicts: empty family")
	}
	if len(tuples) == 0 {
		return nil, nil
	}
	tuplesJSON, err := json.Marshal(tuples)
	if err != nil {
		return nil, fmt.Errorf("marshal redshift tuples: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT e.execution_id, e.status, e.first_dispatch_at, COALESCE(e.error, ''), e.recommendations,
		       split_part(COALESCE(NULLIF(e.idempotency_key, ''), e.execution_id::text), ':', 1) = $2
		FROM purchase_executions e
		WHERE e.execution_id <> $1
		  AND e.first_dispatch_at IS NOT NULL
		  AND (
		        e.status = 'running'
		        OR (split_part(COALESCE(NULLIF(e.idempotency_key, ''), e.execution_id::text), ':', 1) <> $2
		            AND e.status IN ('approved', 'scheduled', 'pending', 'notified', 'paused')
		            AND e.first_dispatch_at > now() - make_interval(secs => $3))
		      )
		  AND EXISTS (
		        SELECT 1
		        FROM jsonb_array_elements(CASE WHEN jsonb_typeof(e.recommendations) = 'array' THEN e.recommendations ELSE '[]'::jsonb END) r
		        JOIN jsonb_to_recordset($5::jsonb) AS t(region text, node_type text, term int, payment text, count int, cloud_account_id uuid)
		          ON r->>'region' = t.region AND r->>'resource_type' = t.node_type
		         AND r->>'term' = t.term::text AND r->>'payment' = t.payment AND r->>'count' = t.count::text
		         AND (COALESCE(e.cloud_account_id, CASE WHEN r->>'cloud_account_id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN (r->>'cloud_account_id')::uuid END) IS NULL
		              OR COALESCE(t.cloud_account_id, $4::uuid) IS NULL
		              OR COALESCE(e.cloud_account_id, CASE WHEN r->>'cloud_account_id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN (r->>'cloud_account_id')::uuid END) = COALESCE(t.cloud_account_id, $4::uuid))
		        WHERE r->>'selected' = 'true'
		          AND COALESCE(r->>'provider', '') IN ('', 'aws')
		          AND r->>'service' IN ('redshift', 'data-warehouse'))
		ORDER BY e.created_at, e.execution_id`,
		selfID, family, horizon.Seconds(), accountID, tuplesJSON)
	if err != nil {
		return nil, fmt.Errorf("find open redshift lineage conflicts: %w", err)
	}
	defer rows.Close()

	var out []RedshiftLineageConflict
	for rows.Next() {
		var c RedshiftLineageConflict
		var recsJSON []byte
		if err := rows.Scan(&c.ExecutionID, &c.Status, &c.FirstDispatchAt, &c.Error, &recsJSON, &c.SameFamily); err != nil {
			return nil, fmt.Errorf("scan redshift lineage conflict: %w", err)
		}
		if err := json.Unmarshal(recsJSON, &c.Recommendations); err != nil {
			return nil, fmt.Errorf("decode recommendations of execution %s: %w", c.ExecutionID, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
