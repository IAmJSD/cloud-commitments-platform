package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// settlementMigrationVersion is the migration that adds the outcome-settlement
// columns to purchase_executions (issue #999).
const settlementMigrationVersion = 157

// rowQuerier is satisfied by both the store's connection and a pgx.Tx, so a
// schema check can run inside the caller's transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readSchemaVersion returns the applied migration version and its dirty flag.
// found is false when schema_migrations has no row (nothing applied yet).
func readSchemaVersion(ctx context.Context, q rowQuerier) (version int, dirty, found bool, err error) {
	err = q.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("read schema version: %w", err)
	}
	return version, dirty, true, nil
}

// settlementSchemaReady reports whether the settlement columns exist. main
// deploys on merge while prod migrations are applied separately, so the code
// must run against a schema without them: the retry link and the managed
// delete guard fall back to their pre-settlement queries, and settling is
// refused (ErrExecutionSettleNotReady). A later version means the columns
// exist even if that later migration is dirty; at exactly this version a
// dirty flag means it may not have finished.
func settlementSchemaReady(ctx context.Context, q rowQuerier) (bool, error) {
	version, dirty, found, err := readSchemaVersion(ctx, q)
	if err != nil || !found {
		return false, err
	}
	return version > settlementMigrationVersion || (version == settlementMigrationVersion && !dirty), nil
}
