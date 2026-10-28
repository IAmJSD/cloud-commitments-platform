//go:build integration

package config

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #8: service_configs rows expose their created_at/updated_at stamps,
// read-only. These run against real Postgres so the trigger, the upsert's
// RETURNING clause and the column precision are the real ones.

func newStampsStore(ctx context.Context, t *testing.T) *PostgresStore {
	t.Helper()
	return NewPostgresStore(testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath()).DB)
}

// TestServiceConfigStamps_ReadsAndSavesAgree: every read returns the row's
// stamps, the save reports exactly the stamps it wrote (not the caller's), a
// later save advances updated_at and leaves created_at alone.
func TestServiceConfigStamps_ReadsAndSavesAgree(t *testing.T) {
	ctx := context.Background()
	store := newStampsStore(ctx, t)

	forged := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	first := &ServiceConfig{
		Provider: "aws", Service: "rds", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 50,
		RampSchedule: RampImmediate, CreatedAt: &forged, UpdatedAt: &forged,
	}
	require.NoError(t, store.SaveServiceConfig(ctx, first))
	require.NotNil(t, first.CreatedAt)
	require.NotNil(t, first.UpdatedAt)
	assert.False(t, first.CreatedAt.Equal(forged), "created_at comes from the database, not the caller")
	assert.False(t, first.UpdatedAt.Equal(forged), "updated_at comes from the database, not the caller")

	got, err := store.GetServiceConfig(ctx, "aws", "rds")
	require.NoError(t, err)
	require.NotNil(t, got.CreatedAt)
	require.NotNil(t, got.UpdatedAt)
	assert.True(t, first.CreatedAt.Equal(*got.CreatedAt), "save %v, get %v", *first.CreatedAt, *got.CreatedAt)
	assert.True(t, first.UpdatedAt.Equal(*got.UpdatedAt), "save %v, get %v", *first.UpdatedAt, *got.UpdatedAt)

	// NOW() is the transaction start, so the second save needs a later
	// transaction to get a later stamp.
	time.Sleep(5 * time.Millisecond)
	second := *got
	second.Coverage = 70
	require.NoError(t, store.SaveServiceConfig(ctx, &second))
	assert.True(t, first.CreatedAt.Equal(*second.CreatedAt), "an update keeps created_at")
	assert.True(t, second.UpdatedAt.After(*first.UpdatedAt), "an update advances updated_at: %v -> %v", *first.UpdatedAt, *second.UpdatedAt)

	list, err := store.ListServiceConfigs(ctx)
	require.NoError(t, err)
	var listed *ServiceConfig
	for i := range list {
		if list[i].Provider == "aws" && list[i].Service == "rds" {
			listed = &list[i]
		}
	}
	require.NotNil(t, listed, "aws/rds must be listed")
	require.NotNil(t, listed.UpdatedAt)
	assert.True(t, second.CreatedAt.Equal(*listed.CreatedAt))
	assert.True(t, second.UpdatedAt.Equal(*listed.UpdatedAt))
	for _, cfg := range list {
		assert.NotNil(t, cfg.CreatedAt, "%s/%s has created_at", cfg.Provider, cfg.Service)
		assert.NotNil(t, cfg.UpdatedAt, "%s/%s has updated_at", cfg.Provider, cfg.Service)
	}
}

// TestUpdateServiceConfigAtomic_ConcurrentSavesReportTheirOwnStamps fires
// concurrent atomic updates of one row. Each must report the stamps of the
// row version it wrote, so the row left in the table carries exactly the
// updated_at its writer was told, and created_at never moves.
func TestUpdateServiceConfigAtomic_ConcurrentSavesReportTheirOwnStamps(t *testing.T) {
	ctx := context.Background()
	store := newStampsStore(ctx, t)
	seed := &ServiceConfig{Provider: "aws", Service: "rds", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 10, RampSchedule: RampImmediate}
	require.NoError(t, store.SaveServiceConfig(ctx, seed))

	const writers = 8
	type outcome struct {
		coverage float64
		saved    *ServiceConfig
		err      error
	}
	results := make([]outcome, writers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			coverage := float64(20 + i)
			saved, err := store.UpdateServiceConfigAtomic(ctx, "aws", "rds", func(existing *ServiceConfig) (*ServiceConfig, error) {
				next := *existing
				next.Coverage = coverage
				return &next, nil
			})
			results[i] = outcome{coverage: coverage, saved: saved, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	final, err := store.GetServiceConfig(ctx, "aws", "rds")
	require.NoError(t, err)
	matched := false
	for _, r := range results {
		require.NoError(t, r.err)
		require.NotNil(t, r.saved.CreatedAt)
		require.NotNil(t, r.saved.UpdatedAt)
		assert.True(t, seed.CreatedAt.Equal(*r.saved.CreatedAt), "created_at must not move under concurrent updates")
		assert.False(t, r.saved.UpdatedAt.Before(*seed.UpdatedAt), "an update's stamp is not older than the seed's")
		if r.coverage == final.Coverage {
			matched = true
			assert.True(t, r.saved.UpdatedAt.Equal(*final.UpdatedAt),
				"the last writer was told %v but the row says %v", *r.saved.UpdatedAt, *final.UpdatedAt)
		}
	}
	require.True(t, matched, "the final coverage %v must be one writer's", final.Coverage)
	assert.True(t, seed.CreatedAt.Equal(*final.CreatedAt))
}

// TestPurchasePlanServices_StampsNeverStored: stamps on a plan's services map
// entries are dropped before the JSONB write, on create and on update.
func TestPurchasePlanServices_StampsNeverStored(t *testing.T) {
	ctx := context.Background()
	store := newStampsStore(ctx, t)

	forged := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	svc := ServiceConfig{
		Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80,
		CreatedAt: &forged, UpdatedAt: &forged,
	}
	plan := &PurchasePlan{
		Name:         "stamps",
		Services:     map[string]ServiceConfig{"aws/ec2": svc},
		RampSchedule: PresetRampSchedules["immediate"],
	}
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))

	rawServices := func() string {
		var raw string
		require.NoError(t, store.db.QueryRow(ctx, `SELECT services::text FROM purchase_plans WHERE id = $1`, plan.ID).Scan(&raw))
		return raw
	}
	created := rawServices()
	assert.Contains(t, created, `"payment": "no-upfront"`)
	assert.NotContains(t, created, "created_at")
	assert.NotContains(t, created, "updated_at")

	stored, err := store.GetPurchasePlan(ctx, plan.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.Services["aws/ec2"].CreatedAt)
	assert.Nil(t, stored.Services["aws/ec2"].UpdatedAt)

	stored.Services["aws/ec2"] = svc
	require.NoError(t, store.UpdatePurchasePlan(ctx, stored))
	updated := rawServices()
	assert.NotContains(t, updated, "created_at")
	assert.NotContains(t, updated, "updated_at")
}
