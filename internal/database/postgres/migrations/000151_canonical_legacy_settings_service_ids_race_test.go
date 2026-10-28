//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
)

// m151Waiting counts the lock requests on table that are not granted yet.
func m151Waiting(ctx context.Context, pool *pgxpool.Pool, table string, want int) func() bool {
	return func() bool {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
			WHERE NOT granted AND locktype = 'relation' AND relation = $1::regclass`, table).Scan(&n)
		return err == nil && n == want
	}
}

// TestMigration_CanonicalLegacySettingsServiceIDs_ConcurrentWriters covers an
// application instance racing 000151 on a cold start:
//
//   - A transaction that writes a legacy row and commits while the migration
//     is starting: the migration waits for it and re-keys the committed row,
//     instead of missing the uncommitted row and leaving it under the old id.
//   - Writers that arrive while the migration runs (the locked
//     read-merge-write paths, UpdateServiceConfigAtomic and
//     SaveAccountServiceOverrideAuditedTx): they wait for the migration, then
//     read the re-keyed row, so the merge base and the audit event's before
//     are the row they actually replace.
func TestMigration_CanonicalLegacySettingsServiceIDs_ConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	container, pool, path := m151Setup(ctx, t)
	store := config.NewPostgresStore(container.DB)
	m151Account(ctx, t, pool, m151AccountA, "azure")

	// Old-version writers with uncommitted legacy rows.
	overrideTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = overrideTx.Rollback(ctx) }()
	_, err = overrideTx.Exec(ctx, `INSERT INTO account_service_overrides (account_id, provider, service, coverage)
		VALUES ($1, 'azure', 'vm', 61)`, m151AccountA)
	require.NoError(t, err)
	serviceTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = serviceTx.Rollback(ctx) }()
	_, err = serviceTx.Exec(ctx, `INSERT INTO service_configs (provider, service, coverage) VALUES ('gcp', 'sql', 62)`)
	require.NoError(t, err)

	// The migration locks service_configs first, so it waits for serviceTx.
	migrated := make(chan error, 1)
	go func() { migrated <- migrations.MigrateToVersion(ctx, pool, path, m151Version) }()
	require.Eventually(t, m151Waiting(ctx, pool, "service_configs", 1), 30*time.Second, 50*time.Millisecond,
		"the migration must wait for the open service_configs writer")

	// A locked read-merge-write of the canonical row queues behind it.
	var mergeBase *config.ServiceConfig
	serviceSaved := make(chan error, 1)
	go func() {
		_, saveErr := store.UpdateServiceConfigAtomic(ctx, "gcp", "relational-db", func(existing *config.ServiceConfig) (*config.ServiceConfig, error) {
			mergeBase = existing
			next := config.ServiceConfig{Provider: "gcp", Service: "relational-db", Enabled: true, Term: 1, Payment: "monthly", Coverage: 72}
			if existing != nil {
				next = *existing
				next.Coverage = 72
			}
			return &next, nil
		})
		serviceSaved <- saveErr
	}()
	require.Eventually(t, m151Waiting(ctx, pool, "service_configs", 2), 30*time.Second, 50*time.Millisecond,
		"the service config save must queue behind the migration")

	// Once serviceTx commits, the migration holds service_configs and waits
	// for overrideTx; an audited override save of the canonical id queues
	// behind it.
	require.NoError(t, serviceTx.Commit(ctx))
	require.Eventually(t, m151Waiting(ctx, pool, "account_service_overrides", 1), 30*time.Second, 50*time.Millisecond,
		"the migration must wait for the open override writer")
	overrideSaved := make(chan error, 1)
	go func() {
		coverage := 70.0
		overrideSaved <- store.WithTx(ctx, func(tx pgx.Tx) error {
			return store.SaveAccountServiceOverrideAuditedTx(ctx, tx, &config.AccountServiceOverride{
				AccountID: m151AccountA, Provider: "azure", Service: "compute", Coverage: &coverage,
			}, config.AuditContext{Actor: config.AuditActor{Identity: "admin@example.com"}})
		})
	}()
	require.Eventually(t, m151Waiting(ctx, pool, "account_service_overrides", 2), 30*time.Second, 50*time.Millisecond,
		"the override save must queue behind the migration")
	select {
	case migErr := <-migrated:
		t.Fatalf("migration finished while a legacy write was uncommitted: %v", migErr)
	case <-serviceSaved:
		t.Fatal("the service config save finished before the migration")
	default:
	}

	require.NoError(t, overrideTx.Commit(ctx))
	for _, w := range []struct {
		name string
		ch   chan error
	}{{"migration", migrated}, {"service config save", serviceSaved}, {"override save", overrideSaved}} {
		select {
		case chErr := <-w.ch:
			require.NoError(t, chErr, w.name)
		case <-time.After(60 * time.Second):
			t.Fatalf("%s did not finish after the legacy writers committed", w.name)
		}
	}

	assert.Equal(t, map[string]float64{"gcp/relational-db": 72}, m151Coverages(m151ServiceConfigs(ctx, t, pool)),
		"the committed gcp/sql row was re-keyed and the queued save merged onto it")
	require.NotNil(t, mergeBase, "the queued save must see the re-keyed row, not an empty slot")
	assert.Equal(t, 62.0, mergeBase.Coverage)
	assert.Equal(t, map[string]float64{m151AccountA + "/azure/compute": 70}, m151Coverages(m151Overrides(ctx, t, pool)))

	events := m151Events(ctx, t, pool)
	require.Len(t, events, 3)
	details := m151Details(m151Rekeyed, "vm", "compute")
	m151AssertEvent(t, events[0], config.AuditOverrideDelete, m151AccountA, "azure", "vm", m151Ptr(m151Snapshot(61)), nil, details)
	m151AssertEvent(t, events[1], config.AuditOverrideUpsert, m151AccountA, "azure", "compute", nil, m151Ptr(m151Snapshot(61)), details)
	saved := events[2]
	assert.Equal(t, "admin@example.com", saved.actor)
	require.NotNil(t, saved.before, "the queued save replaced the re-keyed row, so its event has a before")
	assert.JSONEq(t, m151Snapshot(61), *saved.before)
}
