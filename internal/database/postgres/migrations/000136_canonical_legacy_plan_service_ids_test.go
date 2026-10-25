//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	m136Version         = 136
	m136ImmediateRamp   = `{"type":"immediate","percent_per_step":100,"total_steps":1}`
	m136ProtectedRamp   = `{"type":"weekly","percent_per_step":25,"total_steps":4,"current_step":0,"step_interval_days":7}`
	m136Baseline        = `{"aws|ec2|us-east-1|m5.large": {"count": 2}}`
	m136EnabledEC2Entry = `"aws/ec2": {"provider":"aws","service":"ec2","enabled":true,"term":12,"payment":"no-upfront","coverage":80}`
)

type m136Row struct {
	services  string
	state     string
	baseline  *string
	updatedAt time.Time
}

func m136Insert(ctx context.Context, t *testing.T, pool *pgxpool.Pool, services, schedule string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO purchase_plans (name, services, ramp_schedule, ramp_ledger_state)
		VALUES ('legacy service plan', $1::jsonb, $2::jsonb, 'new')
		RETURNING id::text`, services, schedule).Scan(&id))
	return id
}

// m136Trust freezes a baseline the way SetPlanRampBaselineTx does, so the row
// carries the ledger state a sized plan has.
func m136Trust(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		UPDATE purchase_plans SET ramp_baseline = $2::jsonb,
		       ramp_ledger_state = CASE WHEN ramp_ledger_protected(ramp_schedule) THEN 'trusted' ELSE ramp_ledger_state END
		 WHERE id = $1`, id, m136Baseline)
	require.NoError(t, err)
}

func m136Read(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) m136Row {
	t.Helper()
	var r m136Row
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT services::text, ramp_ledger_state, ramp_baseline::text, updated_at
		  FROM purchase_plans WHERE id = $1`, id).Scan(&r.services, &r.state, &r.baseline, &r.updatedAt))
	return r
}

func m136Entry(provider, service string, enabled bool) string {
	b, err := json.Marshal(map[string]any{
		"provider": provider, "service": service, "enabled": enabled, "term": 36, "payment": "no-upfront",
		"coverage": 70, "ramp_schedule": "", "include_regions": []string{"eastus"}, "min_count": 2,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

type m136Case struct {
	name, before, after string
}

func m136RewriteCases() []m136Case {
	vm, compute := m136Entry("azure", "vm", true), m136Entry("azure", "compute", true)
	rewritten := []m136Case{
		{"azure/vm", `{"azure/vm":` + vm + `}`, `{"azure/vm":` + compute + `}`},
		{"azure/sql", `{"azure/sql":` + m136Entry("azure", "sql", true) + `}`, `{"azure/sql":` + m136Entry("azure", "relational-db", true) + `}`},
		{"azure/redis", `{"azure/redis":` + m136Entry("azure", "redis", true) + `}`, `{"azure/redis":` + m136Entry("azure", "cache", true) + `}`},
		{"gcp/cloudsql", `{"gcp/cloudsql":` + m136Entry("gcp", "cloudsql", true) + `}`, `{"gcp/cloudsql":` + m136Entry("gcp", "relational-db", true) + `}`},
		{"gcp/memorystore", `{"gcp/memorystore":` + m136Entry("gcp", "memorystore", false) + `}`, `{"gcp/memorystore":` + m136Entry("gcp", "cache", false) + `}`},
		{"legacy entry next to an unrelated service",
			`{"azure/vm":` + vm + `,` + m136EnabledEC2Entry + `}`,
			`{"azure/vm":` + compute + `,` + m136EnabledEC2Entry + `}`},
		{"several legacy ids with distinct targets",
			`{"azure/vm":` + vm + `,"azure/sql":` + m136Entry("azure", "sql", true) + `,"azure/redis":` + m136Entry("azure", "redis", true) + `}`,
			`{"azure/vm":` + compute + `,"azure/sql":` + m136Entry("azure", "relational-db", true) + `,"azure/redis":` + m136Entry("azure", "cache", true) + `}`},
		{"only the legacy id without a canonical sibling",
			`{"azure/vm":` + vm + `,"azure/compute":` + m136Entry("azure", "compute", false) + `,"azure/sql":` + m136Entry("azure", "sql", true) + `}`,
			`{"azure/vm":` + vm + `,"azure/compute":` + m136Entry("azure", "compute", false) + `,"azure/sql":` + m136Entry("azure", "relational-db", true) + `}`},
	}
	unchanged := []m136Case{
		{name: "canonical value", before: `{"azure/compute":` + compute + `}`},
		{name: "canonical value under a legacy key (#1123 edit)", before: `{"azure/vm":` + compute + `}`},
		{name: "legacy AWS slug", before: `{` + m136EnabledEC2Entry + `}`},
		{name: "Azure id under another provider", before: `{"aws/sql":` + m136Entry("aws", "sql", true) + `,"gcp/vm":` + m136Entry("gcp", "vm", true) + `}`},
		{name: "GCP id under another provider", before: `{"azure/cloudsql":` + m136Entry("azure", "cloudsql", true) + `}`},
		{name: "split Savings Plans keys", before: `{"aws:savings-plans-compute":` + m136Entry("aws", "savings-plans", true) + `}`},
		{name: "canonical sibling", before: `{"azure/vm":` + vm + `,"azure/compute":` + compute + `}`},
		{name: "same legacy id under two keys", before: `{"gcp/cloudsql":` + m136Entry("gcp", "cloudsql", true) + `,"gcp:cloudsql":` + m136Entry("gcp", "cloudsql", true) + `}`},
		{name: "empty services", before: `{}`},
		{name: "JSON null services", before: `null`},
	}
	cases := make([]m136Case, 0, len(rewritten)+len(unchanged))
	cases = append(cases, rewritten...)
	for _, c := range unchanged {
		c.after = c.before
		cases = append(cases, c)
	}
	return cases
}

// TestMigration_CanonicalLegacyPlanServiceIDs locks down migration 000136
// (issue #1124): pre-#608 Azure/GCP service ids stored in purchase_plans are
// rewritten to the canonical slugs plan step resolution filters on, keys and
// every other field stay, and the ledger follows the same rules as an API edit.
func TestMigration_CanonicalLegacyPlanServiceIDs(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()

	prev := previousMigrationVersion(t, m136Version)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))

	cases := m136RewriteCases()
	ids := make([]string, len(cases))
	before := make([]m136Row, len(cases))
	for i, c := range cases {
		ids[i] = m136Insert(ctx, t, pool, c.before, m136ImmediateRamp)
		before[i] = m136Read(ctx, t, pool, ids[i])
	}

	ledgerNew := m136Insert(ctx, t, pool, `{"azure/vm":`+m136Entry("azure", "vm", true)+`}`, m136ProtectedRamp)
	ledgerTrusted := m136Insert(ctx, t, pool, `{"azure/vm":`+m136Entry("azure", "vm", true)+`,`+m136EnabledEC2Entry+`}`, m136ProtectedRamp)
	m136Trust(ctx, t, pool, ledgerTrusted)
	ledgerDisabled := m136Insert(ctx, t, pool, `{"azure/vm":`+m136Entry("azure", "vm", false)+`,`+m136EnabledEC2Entry+`}`, m136ProtectedRamp)
	m136Trust(ctx, t, pool, ledgerDisabled)
	singleStep := m136Insert(ctx, t, pool, `{"azure/vm":`+m136Entry("azure", "vm", true)+`,`+m136EnabledEC2Entry+`}`, m136ImmediateRamp)
	m136Trust(ctx, t, pool, singleStep)
	require.Equal(t, "new", m136Read(ctx, t, pool, ledgerNew).state)
	require.Equal(t, "trusted", m136Read(ctx, t, pool, ledgerTrusted).state)
	require.Equal(t, "trusted", m136Read(ctx, t, pool, ledgerDisabled).state)
	require.NotNil(t, m136Read(ctx, t, pool, singleStep).baseline)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, m136Version))

	for i, c := range cases {
		after := m136Read(ctx, t, pool, ids[i])
		assert.JSONEq(t, c.after, after.services, c.name)
		if c.before == c.after {
			assert.True(t, before[i].updatedAt.Equal(after.updatedAt), "%s: an untouched row keeps updated_at", c.name)
		} else {
			assert.True(t, after.updatedAt.After(before[i].updatedAt), "%s: a rewritten row bumps updated_at", c.name)
		}
	}

	t.Run("never-started ramp stays new", func(t *testing.T) {
		r := m136Read(ctx, t, pool, ledgerNew)
		assert.JSONEq(t, `{"azure/vm":`+m136Entry("azure", "compute", true)+`}`, r.services)
		assert.Equal(t, "new", r.state)
		assert.Nil(t, r.baseline)
	})
	t.Run("sized ramp drops its stale baseline and fails closed", func(t *testing.T) {
		r := m136Read(ctx, t, pool, ledgerTrusted)
		assert.JSONEq(t, `{"azure/vm":`+m136Entry("azure", "compute", true)+`,`+m136EnabledEC2Entry+`}`, r.services)
		assert.Nil(t, r.baseline, "the enabled-service shape changed, as rampBaselineStale would report")
		assert.Equal(t, "legacy", r.state)
	})
	t.Run("disabled legacy entry keeps the ledger", func(t *testing.T) {
		r := m136Read(ctx, t, pool, ledgerDisabled)
		assert.JSONEq(t, `{"azure/vm":`+m136Entry("azure", "compute", false)+`,`+m136EnabledEC2Entry+`}`, r.services)
		require.NotNil(t, r.baseline)
		assert.JSONEq(t, m136Baseline, *r.baseline)
		assert.Equal(t, "trusted", r.state)
	})
	t.Run("single-step plan drops its stale baseline", func(t *testing.T) {
		r := m136Read(ctx, t, pool, singleStep)
		assert.JSONEq(t, `{"azure/vm":`+m136Entry("azure", "compute", true)+`,`+m136EnabledEC2Entry+`}`, r.services)
		assert.Nil(t, r.baseline)
	})

	// The down migration is a no-op and the up migration is idempotent: a
	// second pass changes nothing, not even updated_at.
	snapshot := map[string]m136Row{}
	all := append(append([]string{}, ids...), ledgerNew, ledgerTrusted, ledgerDisabled, singleStep)
	for _, id := range all {
		snapshot[id] = m136Read(ctx, t, pool, id)
	}
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, prev))
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, m136Version))
	for _, id := range all {
		assert.Equal(t, snapshot[id], m136Read(ctx, t, pool, id), "down + up must leave plan %s unchanged", id)
	}
	var leftover bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'canonical_plan_services_000136')`).Scan(&leftover))
	assert.False(t, leftover, "the helper function must not outlive the migration")
}

// TestMigration_CanonicalLegacyPlanServiceIDs_ConcurrentWriters covers the two
// ways an application instance can race the migration on a cold start:
//
//   - A writer that holds the row lock and commits first: the migration's
//     UPDATE waits, then rewrites the committed version, so the writer's change
//     survives and the service still ends up canonical.
//   - A writer holding a snapshot read before the migration: its optimistic
//     update fails with ErrPurchasePlanConflict instead of writing the legacy
//     id back, while an untouched plan's snapshot still saves.
func TestMigration_CanonicalLegacyPlanServiceIDs_ConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	migrationsPath := getMigrationsPath()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	defer container.Cleanup(ctx)
	pool := container.DB.Pool()
	store := config.NewPostgresStore(container.DB)

	require.NoError(t, migrations.MigrateToVersion(ctx, pool, migrationsPath, previousMigrationVersion(t, m136Version)))

	locked := m136Insert(ctx, t, pool, `{"azure/vm":`+m136Entry("azure", "vm", true)+`}`, m136ImmediateRamp)
	staleLegacy := m136Insert(ctx, t, pool, `{"gcp/memorystore":`+m136Entry("gcp", "memorystore", true)+`}`, m136ImmediateRamp)
	staleCanonical := m136Insert(ctx, t, pool, `{"azure/compute":`+m136Entry("azure", "compute", true)+`}`, m136ImmediateRamp)
	legacySnap, err := store.GetPurchasePlan(ctx, staleLegacy)
	require.NoError(t, err)
	canonicalSnap, err := store.GetPurchasePlan(ctx, staleCanonical)
	require.NoError(t, err)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT 1 FROM purchase_plans WHERE id = $1 FOR UPDATE`, locked)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE purchase_plans SET services = jsonb_set(services, '{azure/vm,coverage}', '55') WHERE id = $1`, locked)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- migrations.MigrateToVersion(ctx, pool, migrationsPath, m136Version) }()
	require.Eventually(t, func() bool {
		var waiting bool
		qErr := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock')`).Scan(&waiting)
		return qErr == nil && waiting
	}, 30*time.Second, 50*time.Millisecond, "the migration must block on the writer's row lock")
	select {
	case migErr := <-done:
		t.Fatalf("migration finished while the row was locked: %v", migErr)
	default:
	}
	require.NoError(t, tx.Commit(ctx))
	select {
	case migErr := <-done:
		require.NoError(t, migErr)
	case <-time.After(60 * time.Second):
		t.Fatal("migration did not finish after the writer committed")
	}

	want := m136Entry("azure", "compute", true)
	var wantMap map[string]any
	require.NoError(t, json.Unmarshal([]byte(want), &wantMap))
	wantMap["coverage"] = 55
	wantJSON, err := json.Marshal(map[string]any{"azure/vm": wantMap})
	require.NoError(t, err)
	assert.JSONEq(t, string(wantJSON), m136Read(ctx, t, pool, locked).services,
		"the migration rewrites the committed version: the writer's coverage and the canonical service both survive")

	legacySnap.Name = "stale writer"
	require.ErrorIs(t, store.UpdatePurchasePlan(ctx, legacySnap), config.ErrPurchasePlanConflict,
		"a snapshot from before the migration must not write the legacy id back")
	assert.JSONEq(t, `{"gcp/memorystore":`+m136Entry("gcp", "cache", true)+`}`, m136Read(ctx, t, pool, staleLegacy).services)

	canonicalSnap.Name = "untouched writer"
	require.NoError(t, store.UpdatePurchasePlan(ctx, canonicalSnap), "a plan the migration did not touch keeps a valid snapshot")
}
