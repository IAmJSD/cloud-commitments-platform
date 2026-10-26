//go:build integration

package analytics

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCollector_GCPCommittedUse_DB is the #339 (A06-019) end-to-end check: the
// collector writes active GCP, AWS Savings Plans and AWS RI purchases through
// the real Postgres store, and each snapshot lands with its pkg/common
// commitment_type. Before the fix all three were stored as "RI".
func TestCollector_GCPCommittedUse_DB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	_, file, _, _ := runtime.Caller(0)
	migrationsPath := filepath.Join(filepath.Dir(file), "..", "database", "postgres", "migrations")
	require.NoError(t, migrations.RunMigrations(ctx, container.DB.Pool(), migrationsPath, "", ""))

	store := NewPostgresAnalyticsStore(container.DB)
	cfgStore := &mockConfigStore{
		getActivePurchaseHistoryFunc: func(context.Context, time.Time, []string, map[string][]string) ([]config.PurchaseHistoryRecord, error) {
			return []config.PurchaseHistoryRecord{
				activeRecord("gcp", "compute", "us-central1", 1, 300, 1200),
				activeRecord("aws", "savingsplans", "us-east-1", 1, 500, 2000),
				activeRecord("aws", "ec2", "us-east-1", 1, 100, 400),
			}, nil
		},
	}
	collector, err := NewCollector(CollectorConfig{AnalyticsStore: store}, cfgStore)
	require.NoError(t, err)
	require.NoError(t, collector.Collect(ctx))

	rows, err := container.DB.Pool().Query(ctx, `SELECT provider, service, commitment_type FROM savings_snapshots`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var provider, service, commitmentType string
		require.NoError(t, rows.Scan(&provider, &service, &commitmentType))
		got[provider+"/"+service] = commitmentType
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]string{
		"gcp/compute":      string(common.CommitmentCUD),
		"aws/savingsplans": string(common.CommitmentSavingsPlan),
		"aws/ec2":          string(common.CommitmentReservedInstance),
	}, got)
}
