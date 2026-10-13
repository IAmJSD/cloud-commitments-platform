//go:build integration
// +build integration

package config

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgresStoreDB_LadderArming runs against a dedicated database named pr2_*
// (LADDER_ARMING_TEST_DATABASE_URL): a fresh migrate leaves both ladder switches
// OFF with NULL stamps, the stamps survive a whole-config write from another
// path, and every cap LadderCapMicros accepts is stored as the same micro-dollars
// it compared (NUMERIC(20,6)).
func TestPostgresStoreDB_LadderArming(t *testing.T) {
	dsn := os.Getenv("LADDER_ARMING_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set LADDER_ARMING_TEST_DATABASE_URL to a dedicated pr2_* test database")
	}
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(strings.TrimPrefix(parsed.Path, "/"), "pr2_"), "database must be dedicated to PR2 verification")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, migrations.RunMigrations(ctx, pool, getTestMigrationsPath(), "", ""))
	store := &PostgresStore{db: pool}

	cfg, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	assert.False(t, cfg.LadderingEnabled, "a fresh migrate leaves planning OFF")
	assert.False(t, cfg.LadderExecutionEnabled, "a fresh migrate leaves execution OFF")
	assert.Nil(t, cfg.LadderExecutionChangedBy)
	assert.Nil(t, cfg.LadderExecutionChangedAt)
	assert.Nil(t, cfg.LadderExecutionChangedVia)

	at := time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC)
	by, via := "user-1", "user"
	_, err = store.UpdateGlobalConfigAtomic(ctx, func(existing *GlobalConfig) error {
		existing.LadderExecutionEnabled = true
		existing.LadderExecutionChangedBy, existing.LadderExecutionChangedVia, existing.LadderExecutionChangedAt = &by, &via, &at
		return nil
	})
	require.NoError(t, err)

	// The RI-exchange style whole-config write must not wipe the stamps.
	_, err = store.UpdateGlobalConfigAtomic(ctx, func(existing *GlobalConfig) error {
		existing.RIExchangeEnabled = true
		existing.RIExchangeMode = "manual"
		return nil
	})
	require.NoError(t, err)
	cfg, err = store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	assert.True(t, cfg.LadderExecutionEnabled)
	require.NotNil(t, cfg.LadderExecutionChangedBy)
	assert.Equal(t, "user-1", *cfg.LadderExecutionChangedBy)
	require.NotNil(t, cfg.LadderExecutionChangedVia)
	assert.Equal(t, "user", *cfg.LadderExecutionChangedVia)
	require.NotNil(t, cfg.LadderExecutionChangedAt)
	assert.True(t, cfg.LadderExecutionChangedAt.Equal(at))

	account := &CloudAccount{Name: "pr2 fixture", Provider: "aws", ExternalID: "111111111111", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, account))
	base := LadderConfigDB{CloudAccountID: account.ID, Provider: "aws", Mode: "auto_approve", Cadence: "daily", TargetCoverage: 80, BufferFraction: 0.1, BaselinePercentile: 5, LookbackDays: 30, BufferUtilizationThreshold: 50, MaxActionsPerRun: 5, RampSchedule: []byte(`{"steps":[{"after_days":0,"fraction":1}]}`)}
	caps := []float64{0.000001, 0.1, 0.3, 0.123456, 1.000001, 123.456789, 999999999999.999999, 1e12}
	for _, v := range caps {
		sent, err := LadderCapMicros(v)
		require.NoError(t, err, "cap %v", v)
		c := base
		v := v
		c.MaxHourlyCommitPerRun = &v
		_, err = store.UpsertLadderConfig(ctx, &c)
		require.NoError(t, err, "cap %v", v)
		var micros int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT (max_hourly_commit_per_run * 1000000)::bigint FROM ladder_configs WHERE cloud_account_id = $1`, account.ID).Scan(&micros))
		require.Equal(t, sent, micros, "stored micros must equal compared micros for cap %v", v)
	}
}
