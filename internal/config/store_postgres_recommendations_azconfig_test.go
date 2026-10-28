//go:build integration
// +build integration

package config_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rdsAZConfigStoreRec(azConfig string, savings float64) config.RecommendationRecord {
	rec := awsRec("rds-"+azConfig, "rds", "us-east-1", "db.r5.large", savings)
	rec.Engine = "mysql"
	rec.Details = json.RawMessage(`{"engine":"mysql","az_config":"` + azConfig + `","instance_class":"db.r5.large"}`)
	return rec
}

// Issue #408: Cost Explorer reports Single-AZ and Multi-AZ RDS reservations of
// the same class, engine, term and payment as separate recommendations. Once
// the store upserts on the 12-column index (with the generated az_config
// column) and migration 000155 drops the 11-column index, both rows upsert in
// ONE batch (the shape persistCollection writes) and both persist. Before,
// the batch failed with SQLSTATE 21000 ("ON CONFLICT DO UPDATE command cannot
// affect row a second time") and took the whole collection write down.
func TestPostgresStore_UpsertRecommendations_AZConfigBatch(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	pool := container.DB.Pool()
	store := config.NewPostgresStore(container.DB)
	t0 := time.Now().UTC().Truncate(time.Second)
	collect := []config.SuccessfulCollect{{Provider: "aws"}}

	var oldIdx bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'recommendations_natural_key_az_tp_idx')`).Scan(&oldIdx))
	require.False(t, oldIdx, "11-column index must be gone after the full migration chain (000155)")

	savingsByAZConfig := func(t *testing.T) map[string]float64 {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT az_config, monthly_savings FROM recommendations`)
		require.NoError(t, err)
		defer rows.Close()
		out := map[string]float64{}
		for rows.Next() {
			var k string
			var v float64
			require.NoError(t, rows.Scan(&k, &v))
			_, dup := out[k]
			require.False(t, dup, "two stored rows share az_config %q", k)
			out[k] = v
		}
		require.NoError(t, rows.Err())
		return out
	}
	reset := func(t *testing.T) {
		t.Helper()
		_, err := pool.Exec(ctx, `DELETE FROM recommendations`)
		require.NoError(t, err)
	}

	t.Run("single-az and multi-az in one batch both persist", func(t *testing.T) {
		reset(t)
		require.NoError(t, store.UpsertRecommendations(ctx, t0,
			[]config.RecommendationRecord{rdsAZConfigStoreRec("single-az", 10), rdsAZConfigStoreRec("multi-az", 20)}, collect))
		assert.Equal(t, map[string]float64{"single-az": 10, "multi-az": 20}, savingsByAZConfig(t))
	})

	t.Run("same az_config re-upsert updates in place and leaves the sibling alone", func(t *testing.T) {
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour),
			[]config.RecommendationRecord{rdsAZConfigStoreRec("single-az", 11), rdsAZConfigStoreRec("multi-az", 20)}, collect))
		assert.Equal(t, map[string]float64{"single-az": 11, "multi-az": 20}, savingsByAZConfig(t))
	})

	t.Run("cross-run single-az then multi-az both persist", func(t *testing.T) {
		reset(t)
		require.NoError(t, store.UpsertRecommendations(ctx, t0, []config.RecommendationRecord{rdsAZConfigStoreRec("single-az", 10)}, collect))
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour),
			[]config.RecommendationRecord{rdsAZConfigStoreRec("single-az", 10), rdsAZConfigStoreRec("multi-az", 20)}, collect))
		assert.Equal(t, map[string]float64{"single-az": 10, "multi-az": 20}, savingsByAZConfig(t))
	})

	t.Run("non-database rows alongside are unaffected", func(t *testing.T) {
		reset(t)
		ec2 := awsRec("e1", "ec2", "us-east-1", "m5.large", 30)
		batch := []config.RecommendationRecord{rdsAZConfigStoreRec("single-az", 10), rdsAZConfigStoreRec("multi-az", 20), ec2}
		require.NoError(t, store.UpsertRecommendations(ctx, t0, batch, collect))
		assert.Equal(t, map[string]float64{"single-az": 10, "multi-az": 20, "": 30}, savingsByAZConfig(t))
	})

	t.Run("row written before the switch is found and updated", func(t *testing.T) {
		reset(t)
		// What the pre-#408 writer stored: az_config only inside details. The
		// generated column picks it up, so the new arbiter matches it.
		_, err := pool.Exec(ctx, `
			INSERT INTO recommendations
			    (collected_at, provider, service, region, resource_type, engine, payload,
			     upfront_cost, monthly_savings, term, payment_option)
			VALUES ($1, 'aws', 'rds', 'us-east-1', 'db.r5.large', 'mysql',
			        '{"details":{"engine":"mysql","az_config":"multi-az"}}'::jsonb, 100, 5, 12, 'no-upfront')`, t0)
		require.NoError(t, err)
		require.NoError(t, store.UpsertRecommendations(ctx, t0.Add(time.Hour), []config.RecommendationRecord{rdsAZConfigStoreRec("multi-az", 40)}, collect))
		assert.Equal(t, map[string]float64{"multi-az": 40}, savingsByAZConfig(t), "the legacy row must be the conflict target, not duplicated")
	})
}

// Concurrent collections (overlapping scheduled runs, or a manual refresh
// racing the cron) re-upsert an already stored Single-AZ/Multi-AZ pair at
// once. Every writer must succeed and exactly one row per az_config must
// remain: the 12-column arbiter routes each writer to its own variant's row.
//
// The rows are seeded first on purpose. Two writers inserting the same NEW
// row at the same instant can still fail with 23505 on the non-arbiter
// source-account unique index; that race predates #408 (migration 000120's
// recommendations_natural_key_source_idx on main has it too) and is out of
// this test's scope.
func TestPostgresStore_UpsertRecommendations_AZConfigConcurrent(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	pool := container.DB.Pool()
	store := config.NewPostgresStore(container.DB)
	t0 := time.Now().UTC().Truncate(time.Second)
	collect := []config.SuccessfulCollect{{Provider: "aws"}}
	require.NoError(t, store.UpsertRecommendations(ctx, t0,
		[]config.RecommendationRecord{rdsAZConfigStoreRec("single-az", 1), rdsAZConfigStoreRec("multi-az", 2)}, collect))

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			batch := []config.RecommendationRecord{
				rdsAZConfigStoreRec("single-az", float64(10+i)),
				rdsAZConfigStoreRec("multi-az", float64(20+i)),
			}
			errs[i] = store.UpsertRecommendations(ctx, t0.Add(time.Duration(i+1)*time.Second), batch, collect)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "writer %d", i)
	}

	var n, distinct int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*), COUNT(DISTINCT az_config) FROM recommendations`).Scan(&n, &distinct))
	assert.Equal(t, 2, n)
	assert.Equal(t, 2, distinct)
}
