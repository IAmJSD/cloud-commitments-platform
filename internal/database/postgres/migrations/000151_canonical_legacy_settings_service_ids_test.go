//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

const (
	m151Version = 151
	m151Actor   = "migration 000151"
	// Account ids sort A < B < C, the order the migration writes events in.
	m151AccountA = "15100000-0000-4000-8000-00000000000a"
	m151AccountB = "15100000-0000-4000-8000-00000000000b"
	m151AccountC = "15100000-0000-4000-8000-00000000000c"
	m151AccountD = "15100000-0000-4000-8000-00000000000d"
)

func m151Account(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, provider string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO cloud_accounts (id, name, provider, external_id)
		VALUES ($1::uuid, 'settings ids '||$1::text, $2, $1::text)`, id, provider)
	require.NoError(t, err)
}

// m151Override inserts an override that sets only coverage.
func m151Override(ctx context.Context, t *testing.T, pool *pgxpool.Pool, account, provider, service string, coverage float64) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO account_service_overrides (account_id, provider, service, coverage)
		VALUES ($1, $2, $3, $4)`, account, provider, service, coverage)
	require.NoError(t, err)
}

type m151Row struct {
	id        string
	coverage  float64
	updatedAt time.Time
}

// m151ServiceConfigs maps "provider/service" to the stored row.
func m151ServiceConfigs(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string]m151Row {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT provider||'/'||service, id::text, coverage::float8, updated_at FROM service_configs`)
	require.NoError(t, err)
	return m151Collect(t, rows)
}

// m151Overrides maps "account/provider/service" to the stored row.
func m151Overrides(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string]m151Row {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT account_id::text||'/'||provider||'/'||service, id::text, coverage::float8, updated_at
		FROM account_service_overrides`)
	require.NoError(t, err)
	return m151Collect(t, rows)
}

func m151Collect(t *testing.T, rows pgx.Rows) map[string]m151Row {
	t.Helper()
	out := map[string]m151Row{}
	for rows.Next() {
		var key string
		var r m151Row
		require.NoError(t, rows.Scan(&key, &r.id, &r.coverage, &r.updatedAt))
		out[key] = r
	}
	require.NoError(t, rows.Err())
	return out
}

func m151Coverages(rows map[string]m151Row) map[string]float64 {
	out := make(map[string]float64, len(rows))
	for k, r := range rows {
		out[k] = r.coverage
	}
	return out
}

type m151Event struct {
	eventType, actor, account, provider, service string
	before, after, details                       *string
}

func m151Events(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []m151Event {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT event_type, actor, account_id::text, provider, service,
		before::text, after::text, details::text FROM config_audit_events ORDER BY seq`)
	require.NoError(t, err)
	defer rows.Close()
	var out []m151Event
	for rows.Next() {
		var e m151Event
		require.NoError(t, rows.Scan(&e.eventType, &e.actor, &e.account, &e.provider, &e.service, &e.before, &e.after, &e.details))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

// m151Snapshot is the audit JSON of an override that sets only coverage.
func m151Snapshot(coverage float64) string {
	return fmt.Sprintf(`{"enabled":null,"term":null,"payment":null,"coverage":%v,"ramp_schedule":null,
		"include_engines":null,"exclude_engines":null,"include_regions":null,"exclude_regions":null,
		"include_types":null,"exclude_types":null}`, coverage)
}

func m151Details(reason, legacy, canonical string) string {
	b, err := json.Marshal(map[string]string{"migration": "000151", "reason": reason,
		"legacy_service": legacy, "canonical_service": canonical})
	if err != nil {
		panic(err)
	}
	return string(b)
}

const (
	m151Superseded = "legacy service id superseded by the canonical override"
	m151Rekeyed    = "legacy service id re-keyed to the canonical id"
)

func m151AssertEvent(t *testing.T, got m151Event, eventType, account, provider, service string, before, after *string, details string) {
	t.Helper()
	name := eventType + " " + account + "/" + provider + "/" + service
	assert.Equal(t, eventType, got.eventType, name)
	assert.Equal(t, m151Actor, got.actor, name)
	assert.Equal(t, account+"/"+provider+"/"+service, got.account+"/"+got.provider+"/"+got.service, name)
	m151AssertJSON(t, before, got.before, name+" before")
	m151AssertJSON(t, after, got.after, name+" after")
	require.NotNil(t, got.details, name)
	assert.JSONEq(t, details, *got.details, name+" details")
}

func m151AssertJSON(t *testing.T, want, got *string, name string) {
	t.Helper()
	if want == nil {
		assert.Nil(t, got, name)
		return
	}
	require.NotNil(t, got, name)
	assert.JSONEq(t, *want, *got, name)
}

func m151Ptr(s string) *string { return &s }

func m151Setup(ctx context.Context, t *testing.T) (*testhelpers.PostgresContainer, *pgxpool.Pool, string) {
	t.Helper()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, previousMigrationVersion(t, m151Version)))
	// Start from an empty service_configs so the expected sets are exact.
	_, err := pool.Exec(ctx, `DELETE FROM service_configs`)
	require.NoError(t, err)
	return container, pool, path
}

// TestMigration_CanonicalLegacySettingsServiceIDs covers 000151 (issue #613):
// service_configs and account_service_overrides rows stored under the old
// Settings ids are re-keyed to the canonical slugs, a canonical twin wins over
// its legacy row (per account for overrides), other rows are untouched, every
// override change is in the audit trail, and a second run changes nothing.
func TestMigration_CanonicalLegacySettingsServiceIDs(t *testing.T) {
	ctx := context.Background()
	container, pool, path := m151Setup(ctx, t)
	prev := previousMigrationVersion(t, m151Version)

	for _, sc := range []struct {
		key      [2]string
		coverage float64
	}{
		{[2]string{"azure", "vm"}, 11}, {[2]string{"azure", "sql"}, 12}, {[2]string{"azure", "relational-db"}, 22},
		{[2]string{"azure", "cosmosdb"}, 13}, {[2]string{"azure", "redis"}, 14}, {[2]string{"gcp", "sql"}, 15},
		{[2]string{"gcp", "memorystore"}, 16}, {[2]string{"gcp", "cache"}, 26},
		// Untouched: canonical or unmapped ids, and a legacy name under another provider.
		{[2]string{"aws", "ec2"}, 30}, {[2]string{"azure", "search"}, 31}, {[2]string{"gcp", "compute"}, 32},
		{[2]string{"gcp", "cloudsql"}, 33}, {[2]string{"aws", "vm"}, 34},
	} {
		_, err := pool.Exec(ctx, `INSERT INTO service_configs (provider, service, coverage) VALUES ($1, $2, $3)`,
			sc.key[0], sc.key[1], sc.coverage)
		require.NoError(t, err)
	}

	m151Account(ctx, t, pool, m151AccountA, "azure")
	m151Account(ctx, t, pool, m151AccountB, "azure")
	m151Account(ctx, t, pool, m151AccountC, "gcp")
	m151Account(ctx, t, pool, m151AccountD, "azure")
	// A: one re-key (with every setting filled in), one twin, one untouched.
	_, err := pool.Exec(ctx, `INSERT INTO account_service_overrides (account_id, provider, service,
		enabled, term, payment, coverage, ramp_schedule, include_regions, exclude_types)
		VALUES ($1, 'azure', 'vm', true, 12, 'upfront', 41.5, 'immediate', '{eastus,westeurope}', '{}')`, m151AccountA)
	require.NoError(t, err)
	m151Override(ctx, t, pool, m151AccountA, "azure", "redis", 42)
	m151Override(ctx, t, pool, m151AccountA, "azure", "cache", 52)
	m151Override(ctx, t, pool, m151AccountA, "azure", "search", 43)
	// B: A's azure/cache is not B's twin, so B's redis is re-keyed.
	m151Override(ctx, t, pool, m151AccountB, "azure", "redis", 44)
	m151Override(ctx, t, pool, m151AccountB, "azure", "compute", 55)
	// C: GCP re-key, GCP twin, and gcp/cloudsql (a plan id, never a Settings id).
	m151Override(ctx, t, pool, m151AccountC, "gcp", "memorystore", 46)
	m151Override(ctx, t, pool, m151AccountC, "gcp", "sql", 47)
	m151Override(ctx, t, pool, m151AccountC, "gcp", "relational-db", 57)
	m151Override(ctx, t, pool, m151AccountC, "gcp", "cloudsql", 48)

	beforeSC := m151ServiceConfigs(ctx, t, pool)
	beforeOv := m151Overrides(ctx, t, pool)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, m151Version))

	afterSC := m151ServiceConfigs(ctx, t, pool)
	assert.Equal(t, map[string]float64{
		"azure/compute": 11, "azure/relational-db": 22, "azure/nosql": 13, "azure/cache": 14,
		"gcp/relational-db": 15, "gcp/cache": 26,
		"aws/ec2": 30, "azure/search": 31, "gcp/compute": 32, "gcp/cloudsql": 33, "aws/vm": 34,
	}, m151Coverages(afterSC), "service_configs after the migration")

	a, b, c := m151AccountA+"/", m151AccountB+"/", m151AccountC+"/"
	afterOv := m151Overrides(ctx, t, pool)
	assert.Equal(t, map[string]float64{
		a + "azure/compute": 41.5, a + "azure/cache": 52, a + "azure/search": 43,
		b + "azure/cache": 44, b + "azure/compute": 55,
		c + "gcp/cache": 46, c + "gcp/relational-db": 57, c + "gcp/cloudsql": 48,
	}, m151Coverages(afterOv), "account_service_overrides after the migration")

	t.Run("re-keyed rows keep their id and bump updated_at", func(t *testing.T) {
		for legacy, canonical := range map[string]string{
			"azure/vm": "azure/compute", "azure/cosmosdb": "azure/nosql", "azure/redis": "azure/cache",
			"gcp/sql": "gcp/relational-db",
		} {
			assert.Equal(t, beforeSC[legacy].id, afterSC[canonical].id, legacy)
			assert.True(t, afterSC[canonical].updatedAt.After(beforeSC[legacy].updatedAt), legacy)
		}
		for legacy, canonical := range map[string]string{
			a + "azure/vm": a + "azure/compute", b + "azure/redis": b + "azure/cache", c + "gcp/memorystore": c + "gcp/cache",
		} {
			assert.Equal(t, beforeOv[legacy].id, afterOv[canonical].id, legacy)
			assert.True(t, afterOv[canonical].updatedAt.After(beforeOv[legacy].updatedAt), legacy)
		}
	})
	t.Run("twins and untouched rows are left as they were", func(t *testing.T) {
		for _, k := range []string{"azure/relational-db", "gcp/cache", "aws/ec2", "azure/search", "gcp/compute", "gcp/cloudsql", "aws/vm"} {
			assert.Equal(t, beforeSC[k], afterSC[k], k)
		}
		for _, k := range []string{a + "azure/cache", a + "azure/search", b + "azure/compute", c + "gcp/relational-db", c + "gcp/cloudsql"} {
			assert.Equal(t, beforeOv[k], afterOv[k], k)
		}
	})

	fullA := m151Ptr(`{"enabled":true,"term":12,"payment":"upfront","coverage":41.5,"ramp_schedule":"immediate",
		"include_engines":null,"exclude_engines":null,"include_regions":["eastus","westeurope"],"exclude_regions":null,
		"include_types":null,"exclude_types":[]}`)
	events := m151Events(ctx, t, pool)
	t.Run("every override change is audited", func(t *testing.T) {
		require.Len(t, events, 8)
		m151AssertEvent(t, events[0], config.AuditOverrideDelete, m151AccountA, "azure", "redis",
			m151Ptr(m151Snapshot(42)), nil, m151Details(m151Superseded, "redis", "cache"))
		m151AssertEvent(t, events[1], config.AuditOverrideDelete, m151AccountC, "gcp", "sql",
			m151Ptr(m151Snapshot(47)), nil, m151Details(m151Superseded, "sql", "relational-db"))
		rekeys := []struct {
			account, provider, legacy, canonical string
			settings                             *string
		}{
			{m151AccountA, "azure", "vm", "compute", fullA},
			{m151AccountB, "azure", "redis", "cache", m151Ptr(m151Snapshot(44))},
			{m151AccountC, "gcp", "memorystore", "cache", m151Ptr(m151Snapshot(46))},
		}
		for i, r := range rekeys {
			details := m151Details(m151Rekeyed, r.legacy, r.canonical)
			m151AssertEvent(t, events[2+2*i], config.AuditOverrideDelete, r.account, r.provider, r.legacy, r.settings, nil, details)
			m151AssertEvent(t, events[3+2*i], config.AuditOverrideUpsert, r.account, r.provider, r.canonical, nil, r.settings, details)
		}
	})

	t.Run("audit snapshots match the shape the application writes", func(t *testing.T) {
		// Save the same settings through the store on another account and
		// compare its after with the migration's re-key event.
		enabled, term, payment, coverage, ramp := true, 12, "upfront", 41.5, "immediate"
		store := config.NewPostgresStore(container.DB)
		require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
			return store.SaveAccountServiceOverrideAuditedTx(ctx, tx, &config.AccountServiceOverride{
				AccountID: m151AccountD, Provider: "azure", Service: "compute",
				Enabled: &enabled, Term: &term, Payment: &payment, Coverage: &coverage, RampSchedule: &ramp,
				IncludeRegions: []string{"eastus", "westeurope"}, ExcludeTypes: []string{},
			}, config.AuditContext{Actor: config.AuditActor{Identity: "admin@example.com"}})
		}))
		var same bool
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT m.after = s.after FROM config_audit_events m, config_audit_events s
			 WHERE m.actor = $1 AND m.event_type = $2 AND m.account_id = $3 AND m.service = 'compute'
			   AND s.actor = 'admin@example.com' AND s.account_id = $4`,
			m151Actor, config.AuditOverrideUpsert, m151AccountA, m151AccountD).Scan(&same))
		assert.True(t, same, "the migration's snapshot must equal overrideSnapshotJSON's for the same settings")
	})

	// The down migration is a no-op and a second up pass changes nothing.
	snapSC, snapOv := m151ServiceConfigs(ctx, t, pool), m151Overrides(ctx, t, pool)
	snapEvents := m151Events(ctx, t, pool)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, prev))
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, m151Version))
	assert.Equal(t, snapSC, m151ServiceConfigs(ctx, t, pool), "down + up leaves service_configs alone")
	assert.Equal(t, snapOv, m151Overrides(ctx, t, pool), "down + up leaves account_service_overrides alone")
	assert.Equal(t, snapEvents, m151Events(ctx, t, pool), "down + up writes no audit events")
	var leftover bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc
		WHERE proname IN ('legacy_settings_services_000151', 'override_snapshot_000151'))`).Scan(&leftover))
	assert.False(t, leftover, "the helper functions must not outlive the migration")
}
