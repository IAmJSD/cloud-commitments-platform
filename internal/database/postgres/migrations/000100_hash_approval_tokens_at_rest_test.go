//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sha256Hex mirrors config.HashApprovalToken so the test asserts against the
// same digest format the application computes, without importing internal/api
// (which would pull the whole handler graph into a migrations_test build).
func sha256Hex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func strPtr(s string) *string { return &s }

// seedPurchaseExecutionWithToken inserts a minimal purchase_executions row
// carrying approval_token (nil seeds NULL) and returns its execution_id. error
// is set to the empty string because the store scans it into a Go string.
func seedPurchaseExecutionWithToken(ctx context.Context, t *testing.T, pool *pgxpool.Pool, status string, token *string) string {
	t.Helper()
	var execID string
	err := pool.QueryRow(ctx, `
		INSERT INTO purchase_executions (status, scheduled_date, approval_token, error)
		VALUES ($1, NOW(), $2, '')
		RETURNING execution_id
	`, status, token).Scan(&execID)
	require.NoError(t, err)
	return execID
}

// tokenColumnsOf reads back one row's (approval_token, approval_token_hash)
// pair; NULL scans as nil.
func tokenColumnsOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, idCol, id string) (raw, hash *string) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT approval_token, approval_token_hash FROM `+table+` WHERE `+idCol+` = $1`, id).Scan(&raw, &hash))
	return raw, hash
}

// seedRIExchangeWithToken inserts a minimal ri_exchange_history row carrying
// approval_token (nil seeds NULL) and returns its id.
func seedRIExchangeWithToken(ctx context.Context, t *testing.T, pool *pgxpool.Pool, _ string, token *string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO ri_exchange_history (
			account_id, region, source_ri_ids, source_instance_type, source_count,
			target_offering_id, target_instance_type, target_count, approval_token
		) VALUES ('123456789012', 'us-east-1', ARRAY['ri-1'], 'm5.large', 1,
		          'offering-1', 'm5.xlarge', 1, $1)
		RETURNING id
	`, token).Scan(&id)
	require.NoError(t, err)
	return id
}

// tokenTable describes one table the migration sweeps. blankRaw is what the
// raw column holds once swept: empty on purchase_executions (pre-#103 code scans
// it into a Go string), NULL on ri_exchange_history (the column is UNIQUE).
type tokenTable struct {
	name, idCol string
	blankRaw    *string
	seed        func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, status string, token *string) string
}

var tokenTables = []tokenTable{
	{"purchase_executions", "execution_id", strPtr(""), seedPurchaseExecutionWithToken},
	{"ri_exchange_history", "id", nil, seedRIExchangeWithToken},
}

func assertMigrated(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tt tokenTable, id, rawToken, msg string) {
	t.Helper()
	raw, hash := tokenColumnsOf(ctx, t, pool, tt.name, tt.idCol, id)
	assert.Equal(t, tt.blankRaw, raw, "%s: %s: the raw approval_token must be blanked", tt.name, msg)
	if assert.NotNil(t, hash, "%s: %s: approval_token_hash must be set", tt.name, msg) {
		assert.Equal(t, sha256Hex(rawToken), *hash, "%s: %s: approval_token_hash must be the SHA-256 hex digest of the raw token exactly once", tt.name, msg)
	}
}

func runUpSQL(ctx context.Context, t *testing.T, pool *pgxpool.Pool, migrationsPath string) {
	t.Helper()
	upSQL, err := os.ReadFile(filepath.Join(migrationsPath, "000100_hash_approval_tokens_at_rest.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(upSQL))
	require.NoError(t, err, "re-running 000100 up must succeed")
}

// TestMigration_HashApprovalTokensAtRest locks down migration 000100
// (issue #103, expand-contract step 1): every raw approval_token on
// purchase_executions and ri_exchange_history moves into approval_token_hash
// as its SHA-256 hex digest and the raw column is blanked; empty and NULL raw
// values get no hash, not the digest of the empty string; re-running the up SQL sweeps rows
// that pre-#103 code wrote after the first run without double-hashing or
// destroying migrated hashes; and the no-op down keeps the hashes so a
// down-then-up is lossless.
func TestMigration_HashApprovalTokensAtRest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	prev := previousMigrationVersion(t, 100)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))

	const rawToken = "raw-approval-token-abc123"
	withToken := map[string]string{}
	emptyToken := map[string]string{}
	nullToken := map[string]string{}
	for _, tt := range tokenTables {
		withToken[tt.name] = tt.seed(ctx, t, pool, "pending", strPtr(rawToken+tt.name))
		emptyToken[tt.name] = tt.seed(ctx, t, pool, config.StatusCanceled, strPtr(""))
		nullToken[tt.name] = tt.seed(ctx, t, pool, config.StatusCanceled, nil)
	}
	resavedEmpty := seedPurchaseExecutionWithToken(ctx, t, pool, "pending", strPtr("resaved-empty-"+rawToken))
	resavedFresh := seedPurchaseExecutionWithToken(ctx, t, pool, "pending", strPtr("resaved-fresh-"+rawToken))

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 100))

	for _, tt := range tokenTables {
		assertMigrated(ctx, t, pool, tt, withToken[tt.name], rawToken+tt.name, "after up")
		raw, hash := tokenColumnsOf(ctx, t, pool, tt.name, tt.idCol, emptyToken[tt.name])
		assert.Equal(t, tt.blankRaw, raw, "%s: an empty raw token must be blanked", tt.name)
		assert.Nil(t, hash, "%s: an empty raw token must not become the digest of ''", tt.name)
		raw, hash = tokenColumnsOf(ctx, t, pool, tt.name, tt.idCol, nullToken[tt.name])
		assert.Nil(t, raw, "%s: a NULL raw token must stay NULL", tt.name)
		assert.Nil(t, hash, "%s: a NULL raw token must get no hash", tt.name)
	}

	// Pre-#103 code still running against the migrated schema writes raw
	// tokens: new rows, a migrated row re-saved with an empty raw (its upsert
	// writes an empty approval_token), and a migrated row re-saved with a fresh
	// raw token. A manual re-run of the up SQL must hash the fresh raws and
	// must never touch a hash whose raw is empty.
	lateWritten := map[string]string{}
	for _, tt := range tokenTables {
		lateWritten[tt.name] = tt.seed(ctx, t, pool, "pending", strPtr("late-"+rawToken+tt.name))
	}
	_, err := pool.Exec(ctx, `UPDATE purchase_executions SET approval_token = '' WHERE execution_id = $1`, resavedEmpty)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE purchase_executions SET approval_token = 'old-code-fresh-token' WHERE execution_id = $1`, resavedFresh)
	require.NoError(t, err)

	runUpSQL(ctx, t, pool, migrationsPath)
	for _, tt := range tokenTables {
		assertMigrated(ctx, t, pool, tt, withToken[tt.name], rawToken+tt.name, "after a re-run")
		assertMigrated(ctx, t, pool, tt, lateWritten[tt.name], "late-"+rawToken+tt.name, "row written by old code after the first run")
	}
	pe := tokenTables[0]
	assertMigrated(ctx, t, pool, pe, resavedEmpty, "resaved-empty-"+rawToken, "row old code re-saved with an empty raw keeps its hash")
	assertMigrated(ctx, t, pool, pe, resavedFresh, "old-code-fresh-token", "row old code re-saved with a fresh raw gets the new hash")

	// The down is a documented no-op (dropping the hash column would destroy
	// the only copy of every live token); down-then-up must not re-hash.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 100))
	for _, tt := range tokenTables {
		assertMigrated(ctx, t, pool, tt, withToken[tt.name], rawToken+tt.name, "after down-then-up")
	}
}

// TestMigration_HashApprovalTokensAtRest_StoreReadsTokenlessRows: rows that
// had an empty or NULL raw token (every canceled row, since clearApprovalToken
// writes the empty string) keep a NULL approval_token_hash after migration 000100. The
// store must read them back, alone and mixed with a tokened row in one list,
// or execution history, pending lists and the scheduler break. A row the new
// store writes must also stay readable by pre-#103 code.
func TestMigration_HashApprovalTokensAtRest_StoreReadsTokenlessRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrationsPath := getMigrationsPath()

	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, 100)))
	want := map[string]string{
		seedPurchaseExecutionWithToken(ctx, t, pool, "pending", strPtr("live-token")):   sha256Hex("live-token"),
		seedPurchaseExecutionWithToken(ctx, t, pool, config.StatusCanceled, strPtr("")): "",
		seedPurchaseExecutionWithToken(ctx, t, pool, config.StatusCanceled, nil):        "",
	}
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, 100))
	// The current store selects columns added after 000100 (e.g. 000113
	// no_submission), so it can only run against a fully migrated schema.
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, migrationVersionsDesc(t)[0]))

	store := config.NewPostgresStore(container.DB)
	for id, hash := range want {
		exec, getErr := store.GetExecutionByID(ctx, id)
		require.NoError(t, getErr, "GetExecutionByID(%s)", id)
		require.NotNil(t, exec)
		assert.Equal(t, hash, exec.ApprovalToken, "GetExecutionByID(%s)", id)
	}

	list, err := store.GetExecutionsByStatuses(ctx, []string{"pending", config.StatusCanceled}, 0)
	require.NoError(t, err, "a tokenless row must not fail the whole list")
	got := map[string]string{}
	for _, exec := range list {
		got[exec.ExecutionID] = exec.ApprovalToken
	}
	assert.Equal(t, want, got)

	// Pre-#103 code (a rollback, or an old revision still serving during a
	// rolling deploy) scans approval_token into a plain string, so a row the
	// new store writes must carry '' there, not NULL.
	newRow := &config.PurchaseExecution{
		Status:        "pending",
		ScheduledDate: time.Now(),
		ApprovalToken: sha256Hex("new-code-token"),
	}
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
		return store.SavePurchaseExecutionTx(ctx, tx, newRow)
	}))
	var oldCodeRaw string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT approval_token FROM purchase_executions WHERE execution_id = $1`, newRow.ExecutionID).Scan(&oldCodeRaw),
		"pre-#103 code must still read a row written by the #103 store")
	assert.Equal(t, "", oldCodeRaw)
}
