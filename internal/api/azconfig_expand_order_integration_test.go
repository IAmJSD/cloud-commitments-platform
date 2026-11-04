//go:build integration

package api

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// contractMigrationVersion is the version of the migration that drops the
// 11-column recommendations key (#408). It can only run after the release that
// upserts on the 12-column key, so every expand step the new code needs must
// sort BEFORE it.
func contractMigrationVersion(t *testing.T) uint {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(getMigrationsPath(), "*_drop_recommendations_11col_natural_key.up.sql"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	n, err := strconv.ParseUint(strings.SplitN(filepath.Base(paths[0]), "_", 2)[0], 10, 64)
	require.NoError(t, err)
	return uint(n)
}

// previousVersion returns the highest migration version below version, so the
// test stops at the last migration a deploy applies before the contract step.
func previousVersion(t *testing.T, version uint) uint {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(getMigrationsPath(), "*.up.sql"))
	require.NoError(t, err)
	var best uint
	for _, p := range paths {
		n, err := strconv.ParseUint(strings.SplitN(filepath.Base(p), "_", 2)[0], 10, 64)
		require.NoError(t, err)
		if uint(n) < version && uint(n) > best {
			best = uint(n)
		}
	}
	require.NotZero(t, best)
	return best
}

func expandOrderRDSRec(azConfig string, upfront float64) config.RecommendationRecord {
	rec := rdsAZConfigRec(azConfig, upfront)
	rec.ID = ""
	return rec
}

// exerciseNewCode runs the four paths the #408 code touches against whatever
// schema the database currently has.
// Before the contract step the 11-column index is still in place, so a
// Single-AZ and Multi-AZ pair of one class cannot coexist yet (the documented
// limitation the contract step resolves, and the same failure the previous
// release had); only after it may both variants be upserted in one batch.
func exerciseNewCode(ctx context.Context, t *testing.T, store *config.PostgresStore, recs []config.RecommendationRecord, priced string, wantUpfront float64) {
	t.Helper()
	single := expandOrderRDSRec("single-az", 100)

	require.NoError(t, store.UpsertRecommendations(ctx, time.Now(), recs, []config.SuccessfulCollect{{Provider: "aws"}}),
		"upsert of RDS variants")

	purchase := single
	purchase.Count = 2
	sups := config.BuildSuppressions([]config.RecommendationRecord{purchase}, uuid.NewString(), &config.GlobalConfig{}, time.Now())
	require.Len(t, sups, 1)
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error { return store.CreateSuppressionTx(ctx, tx, &sups[0]) }),
		"suppression write inside the purchase transaction")

	active, err := store.ListActiveSuppressions(ctx)
	require.NoError(t, err, "ListActiveSuppressions")
	assert.NotEmpty(t, active)

	h := &Handler{config: store}
	req := []config.RecommendationRecord{{
		Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: "db.r5.large", Engine: "mysql",
		Term: 1, Payment: "all-upfront", Count: 1, Details: rdsDetails(priced),
	}}
	require.NoError(t, h.priceRecommendationsFromStore(ctx, req), "purchase pricing index lookup")
	assert.Equal(t, wantUpfront, req[0].UpfrontCost)
}

// Issue #408 deploy order: the new code ships after the expand migrations and
// before the contract one. A database stopped just below the contract step
// must already serve every new-code path, and still do so once the contract
// step has run.
func TestAZConfigNewCodeWorksBeforeAndAfterContractMigration(t *testing.T) {
	ctx := context.Background()
	pg := testhelpers.RequireEmptyDatabase(ctx, t)
	pool, path := pg.DB.Pool(), getMigrationsPath()
	contract := contractMigrationVersion(t)
	store := config.NewPostgresStore(pg.DB)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousVersion(t, contract)))
	single, multi := expandOrderRDSRec("single-az", 100), expandOrderRDSRec("multi-az", 300)
	exerciseNewCode(ctx, t, store, []config.RecommendationRecord{single}, "single-az", 100)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, contract))
	_, err := pool.Exec(ctx, `TRUNCATE recommendations, purchase_suppressions`)
	require.NoError(t, err)
	exerciseNewCode(ctx, t, store, []config.RecommendationRecord{single, multi}, "multi-az", 300)
}
