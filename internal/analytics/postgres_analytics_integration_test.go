//go:build integration
// +build integration

package analytics_test

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/analytics"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresAnalyticsStore_SaveSnapshot(t *testing.T) {
	ctx := context.Background()

	// A fresh migrated database of its own, on the package's shared server
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())

	// Create store
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	t.Run("save snapshot with all fields", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		snapshot := &analytics.SavingsSnapshot{
			AccountID:          "123456789012",
			Timestamp:          now,
			Provider:           "aws",
			Service:            "rds",
			Region:             "us-east-1",
			CommitmentType:     "RI",
			TotalCommitment:    new(float64(1000.00)),
			TotalUsage:         f64ptr(800.00),
			TotalSavings:       200.00,
			CoveragePercentage: f64ptr(80.00),
			Metadata: map[string]interface{}{
				"active_purchases": 5,
				"collection_time":  now.Format(time.RFC3339),
			},
		}

		err := store.SaveSnapshot(ctx, snapshot)
		require.NoError(t, err)
		assert.NotEmpty(t, snapshot.ID)
	})

	t.Run("save snapshot without metadata", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		snapshot := &analytics.SavingsSnapshot{
			AccountID:          "123456789012",
			Timestamp:          now,
			Provider:           "gcp",
			Service:            "cloudsql",
			Region:             "us-central1",
			CommitmentType:     "RI",
			TotalCommitment:    new(float64(500.00)),
			TotalUsage:         f64ptr(400.00),
			TotalSavings:       100.00,
			CoveragePercentage: f64ptr(80.00),
		}

		err := store.SaveSnapshot(ctx, snapshot)
		require.NoError(t, err)
		assert.NotEmpty(t, snapshot.ID)
	})

	t.Run("save SavingsPlan commitment type", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		snapshot := &analytics.SavingsSnapshot{
			AccountID:          "123456789012",
			Timestamp:          now,
			Provider:           "aws",
			Service:            "SavingsPlans",
			Region:             "us-east-1",
			CommitmentType:     "SavingsPlan",
			TotalCommitment:    new(float64(2000.00)),
			TotalUsage:         f64ptr(1500.00),
			TotalSavings:       500.00,
			CoveragePercentage: f64ptr(75.00),
		}

		err := store.SaveSnapshot(ctx, snapshot)
		require.NoError(t, err)
	})
}

func TestPostgresAnalyticsStore_QuerySavings(t *testing.T) {
	ctx := context.Background()

	// A fresh migrated database of its own, on the package's shared server
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())

	// Create store
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	// Insert test data
	now := time.Now().UTC().Truncate(time.Microsecond)
	testSnapshots := []*analytics.SavingsSnapshot{
		{
			AccountID:          "123456789012",
			Timestamp:          now.Add(-1 * time.Hour),
			Provider:           "aws",
			Service:            "rds",
			Region:             "us-east-1",
			CommitmentType:     "RI",
			TotalCommitment:    new(float64(1000.00)),
			TotalUsage:         f64ptr(800.00),
			TotalSavings:       200.00,
			CoveragePercentage: f64ptr(80.00),
		},
		{
			AccountID:          "123456789012",
			Timestamp:          now.Add(-2 * time.Hour),
			Provider:           "aws",
			Service:            "elasticache",
			Region:             "us-east-1",
			CommitmentType:     "RI",
			TotalCommitment:    new(float64(500.00)),
			TotalUsage:         f64ptr(400.00),
			TotalSavings:       100.00,
			CoveragePercentage: f64ptr(80.00),
		},
		{
			AccountID:          "123456789012",
			Timestamp:          now.Add(-3 * time.Hour),
			Provider:           "gcp",
			Service:            "cloudsql",
			Region:             "us-central1",
			CommitmentType:     "RI",
			TotalCommitment:    new(float64(750.00)),
			TotalUsage:         f64ptr(600.00),
			TotalSavings:       150.00,
			CoveragePercentage: f64ptr(80.00),
		},
	}

	for _, snapshot := range testSnapshots {
		err := store.SaveSnapshot(ctx, snapshot)
		require.NoError(t, err)
	}

	t.Run("query all snapshots for account", func(t *testing.T) {
		req := analytics.QueryRequest{
			AccountExternalIDsByProvider: map[string][]string{"aws": {"123456789012"}, "gcp": {"123456789012"}},
			StartDate:                    now.Add(-24 * time.Hour),
			EndDate:                      now,
		}

		results, err := store.QuerySavings(ctx, req)
		require.NoError(t, err)
		assert.Len(t, results, 3)
	})

	t.Run("query with provider filter", func(t *testing.T) {
		req := analytics.QueryRequest{
			AccountExternalIDsByProvider: map[string][]string{"aws": {"123456789012"}, "gcp": {"123456789012"}},
			Provider:                     "aws",
			StartDate:                    now.Add(-24 * time.Hour),
			EndDate:                      now,
		}

		results, err := store.QuerySavings(ctx, req)
		require.NoError(t, err)
		assert.Len(t, results, 2)
		for _, r := range results {
			assert.Equal(t, "aws", r.Provider)
		}
	})

	t.Run("query with service filter", func(t *testing.T) {
		req := analytics.QueryRequest{
			AccountExternalIDsByProvider: map[string][]string{"aws": {"123456789012"}, "gcp": {"123456789012"}},
			Service:                      "rds",
			StartDate:                    now.Add(-24 * time.Hour),
			EndDate:                      now,
		}

		results, err := store.QuerySavings(ctx, req)
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "rds", results[0].Service)
	})

	t.Run("query with limit", func(t *testing.T) {
		req := analytics.QueryRequest{
			AccountExternalIDsByProvider: map[string][]string{"aws": {"123456789012"}, "gcp": {"123456789012"}},
			StartDate:                    now.Add(-24 * time.Hour),
			EndDate:                      now,
			Limit:                        2,
		}

		results, err := store.QuerySavings(ctx, req)
		require.NoError(t, err)
		assert.Len(t, results, 2)
	})

	t.Run("query returns empty for non-existent account", func(t *testing.T) {
		req := analytics.QueryRequest{
			AccountExternalIDsByProvider: map[string][]string{"aws": {"999999999999"}, "gcp": {"999999999999"}},
			StartDate:                    now.Add(-24 * time.Hour),
			EndDate:                      now,
		}

		results, err := store.QuerySavings(ctx, req)
		require.NoError(t, err)
		assert.Empty(t, results)
	})
}

func TestPostgresAnalyticsStore_QueryMonthlyTotals(t *testing.T) {
	ctx := context.Background()

	// A fresh migrated database of its own, on the package's shared server
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())

	// Create store
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	// Insert test data
	now := time.Now().UTC().Truncate(time.Microsecond)
	testSnapshots := []*analytics.SavingsSnapshot{
		{
			AccountID:          "123456789012",
			Timestamp:          now.Add(-1 * time.Hour),
			Provider:           "aws",
			Service:            "rds",
			Region:             "us-east-1",
			CommitmentType:     "RI",
			TotalCommitment:    new(float64(1000.00)),
			TotalUsage:         f64ptr(800.00),
			TotalSavings:       200.00,
			CoveragePercentage: f64ptr(80.00),
		},
	}

	for _, snapshot := range testSnapshots {
		err := store.SaveSnapshot(ctx, snapshot)
		require.NoError(t, err)
	}

	// Refresh materialized views so data is available
	err := store.RefreshMaterializedViews(ctx)
	require.NoError(t, err)

	t.Run("query monthly totals from materialized view", func(t *testing.T) {
		summaries, err := store.QueryMonthlyTotals(ctx, nil, map[string][]string{"aws": {"123456789012"}}, 6)
		require.NoError(t, err)
		// RefreshMaterializedViews above is synchronous, so the seeded aws/rds
		// row is in the view. It carries no cloud_account_id, so it can only be
		// matched through the provider-scoped external account_id (#32).
		require.Len(t, summaries, 1)
		s := summaries[0]
		assert.Equal(t, "123456789012", s.AccountID)
		assert.Nil(t, s.CloudAccountID)
		assert.Equal(t, "aws", s.Provider)
		assert.Equal(t, "rds", s.Service)
		assert.InDelta(t, 200.00, s.TotalSavings, 0.01)
		assert.Equal(t, 1, s.SnapshotCount)
		// Issue #30: committed spend and usage reach the monthly series.
		require.NotNil(t, s.TotalCommitment)
		assert.InDelta(t, 1000.00, *s.TotalCommitment, 0.01)
		require.NotNil(t, s.TotalUsage)
		assert.InDelta(t, 800.00, *s.TotalUsage, 0.01)
	})

	t.Run("query monthly totals for non-existent account", func(t *testing.T) {
		summaries, err := store.QueryMonthlyTotals(ctx, nil, map[string][]string{"aws": {"999999999999"}, "gcp": {"999999999999"}}, 6)
		require.NoError(t, err)
		assert.Empty(t, summaries)
	})
}

// TestPostgresAnalyticsStore_QueryMonthlyTotals_UnknownStaysUnknown pins issue
// #30 on real Postgres: QueryMonthlyTotals returns the view's total_commitment
// and total_usage, a bucket with one region lacking a figure reads as unknown
// (nil) rather than the known regions' sum, and a known 0 stays a real 0.
func TestPostgresAnalyticsStore_QueryMonthlyTotals_UnknownStaysUnknown(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	now := time.Now().UTC().Truncate(time.Microsecond)
	snap := func(service, region string, commitment, usage *float64) *analytics.SavingsSnapshot {
		return &analytics.SavingsSnapshot{
			AccountID: "123456789012", Timestamp: now, Provider: "aws", Service: service, Region: region,
			CommitmentType: "RI", TotalCommitment: commitment, TotalUsage: usage, TotalSavings: 10,
		}
	}
	for _, s := range []*analytics.SavingsSnapshot{
		// rds: commitment known in both regions; usage unknown in one.
		snap("rds", "us-east-1", f64ptr(300), f64ptr(250)),
		snap("rds", "eu-west-1", f64ptr(100), nil),
		// ec2: commitment unknown in one region; usage known in both.
		snap("ec2", "us-east-1", nil, f64ptr(40)),
		snap("ec2", "eu-west-1", f64ptr(70), f64ptr(60)),
		// elasticache: a known zero in both columns.
		snap("elasticache", "us-east-1", f64ptr(0), f64ptr(0)),
	} {
		require.NoError(t, store.SaveSnapshot(ctx, s))
	}
	require.NoError(t, store.RefreshMaterializedViews(ctx))

	// Two months so a run just after a month boundary still sees the rows.
	summaries, err := store.QueryMonthlyTotals(ctx, nil, map[string][]string{"aws": {"123456789012"}}, 2)
	require.NoError(t, err)
	byService := make(map[string]analytics.MonthlySummary, len(summaries))
	for _, s := range summaries {
		byService[s.Service] = s
	}
	require.Len(t, byService, 3)

	rds := byService["rds"]
	require.NotNil(t, rds.TotalCommitment)
	assert.InDelta(t, 400.0, *rds.TotalCommitment, 0.001, "regions at one timestamp sum")
	assert.Nil(t, rds.TotalUsage, "one region without usage makes the bucket's usage unknown, not 250")

	ec2 := byService["ec2"]
	assert.Nil(t, ec2.TotalCommitment, "one region without a commitment makes it unknown, not 70")
	require.NotNil(t, ec2.TotalUsage)
	assert.InDelta(t, 100.0, *ec2.TotalUsage, 0.001)

	zero := byService["elasticache"]
	require.NotNil(t, zero.TotalCommitment, "a known 0 is not unknown")
	require.NotNil(t, zero.TotalUsage, "a known 0 is not unknown")
	assert.Zero(t, *zero.TotalCommitment)
	assert.Zero(t, *zero.TotalUsage)
}

func TestPostgresAnalyticsStore_QueryByProvider(t *testing.T) {
	ctx := context.Background()

	// A fresh migrated database of its own, on the package's shared server
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())

	// Create store
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	// Insert test data
	now := time.Now().UTC().Truncate(time.Microsecond)
	testSnapshots := []*analytics.SavingsSnapshot{
		{
			AccountID:          "123456789012",
			Timestamp:          now.Add(-1 * time.Hour),
			Provider:           "aws",
			Service:            "rds",
			Region:             "us-east-1",
			CommitmentType:     "RI",
			TotalSavings:       200.00,
			CoveragePercentage: f64ptr(80.00),
		},
		{
			AccountID:          "123456789012",
			Timestamp:          now.Add(-2 * time.Hour),
			Provider:           "aws",
			Service:            "rds",
			Region:             "us-east-1",
			CommitmentType:     "RI",
			TotalSavings:       100.00,
			CoveragePercentage: f64ptr(75.00),
		},
		{
			AccountID:          "123456789012",
			Timestamp:          now.Add(-3 * time.Hour),
			Provider:           "gcp",
			Service:            "cloudsql",
			Region:             "us-central1",
			CommitmentType:     "RI",
			TotalSavings:       150.00,
			CoveragePercentage: f64ptr(70.00),
		},
	}

	for _, snapshot := range testSnapshots {
		err := store.SaveSnapshot(ctx, snapshot)
		require.NoError(t, err)
	}

	t.Run("query by provider aggregates correctly", func(t *testing.T) {
		breakdowns, err := store.QueryByProvider(ctx, nil, map[string][]string{"aws": {"123456789012"}, "gcp": {"123456789012"}}, now.Add(-24*time.Hour), now)
		require.NoError(t, err)
		// One bucket per provider: the gcp row shares the account number and
		// is matched through the "gcp" key, the aws rows through "aws".
		require.Len(t, breakdowns, 2)

		// Find aws/rds breakdown
		var found bool
		for _, b := range breakdowns {
			if b.Provider == "aws" && b.Service == "rds" {
				found = true
				// Snapshots are run-rates: aggregation over time uses AVG,
				// not SUM (see migration 000074 / PR #1127).
				assert.InDelta(t, 150.00, b.TotalSavings, 0.01) // AVG(200, 100)
				require.NotNil(t, b.AvgCoverage)
				assert.InDelta(t, 77.5, *b.AvgCoverage, 0.01) // (80 + 75) / 2
			}
		}
		assert.True(t, found, "aws/rds breakdown not found")

		var gcpFound bool
		for _, b := range breakdowns {
			if b.Provider == "gcp" && b.Service == "cloudsql" {
				gcpFound = true
				assert.InDelta(t, 150.00, b.TotalSavings, 0.01)
			}
		}
		assert.True(t, gcpFound, "gcp/cloudsql breakdown not found")
	})
}

func TestPostgresAnalyticsStore_QueryByService(t *testing.T) {
	ctx := context.Background()

	// A fresh migrated database of its own, on the package's shared server
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())

	// Create store
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	// Insert test data
	now := time.Now().UTC().Truncate(time.Microsecond)
	testSnapshots := []*analytics.SavingsSnapshot{
		{
			AccountID:          "123456789012",
			Timestamp:          now.Add(-1 * time.Hour),
			Provider:           "aws",
			Service:            "rds",
			Region:             "us-east-1",
			CommitmentType:     "RI",
			TotalSavings:       200.00,
			CoveragePercentage: f64ptr(80.00),
		},
		{
			AccountID:          "123456789012",
			Timestamp:          now.Add(-2 * time.Hour),
			Provider:           "aws",
			Service:            "rds",
			Region:             "us-west-2",
			CommitmentType:     "RI",
			TotalSavings:       150.00,
			CoveragePercentage: f64ptr(75.00),
		},
	}

	for _, snapshot := range testSnapshots {
		err := store.SaveSnapshot(ctx, snapshot)
		require.NoError(t, err)
	}

	t.Run("query by service groups by region", func(t *testing.T) {
		breakdowns, err := store.QueryByService(ctx, nil, map[string][]string{"aws": {"123456789012"}}, "aws", now.Add(-24*time.Hour), now)
		require.NoError(t, err)
		assert.Len(t, breakdowns, 2) // Two regions

		regions := make(map[string]float64)
		for _, b := range breakdowns {
			regions[b.Region] = b.TotalSavings
		}

		assert.InDelta(t, 200.00, regions["us-east-1"], 0.01)
		assert.InDelta(t, 150.00, regions["us-west-2"], 0.01)
	})
}

func TestPostgresAnalyticsStore_BulkInsertSnapshots(t *testing.T) {
	ctx := context.Background()

	// A fresh migrated database of its own, on the package's shared server
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())

	// Create store
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	t.Run("bulk insert empty slice", func(t *testing.T) {
		err := store.BulkInsertSnapshots(ctx, []analytics.SavingsSnapshot{})
		assert.NoError(t, err)
	})

	t.Run("bulk insert multiple snapshots", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		snapshots := []analytics.SavingsSnapshot{
			{
				AccountID:      "123456789012",
				Timestamp:      now.Add(-1 * time.Hour),
				Provider:       "aws",
				Service:        "rds",
				Region:         "us-east-1",
				CommitmentType: "RI",
				TotalSavings:   100.00,
			},
			{
				AccountID:      "123456789012",
				Timestamp:      now.Add(-2 * time.Hour),
				Provider:       "aws",
				Service:        "elasticache",
				Region:         "us-east-1",
				CommitmentType: "RI",
				TotalSavings:   200.00,
			},
		}

		err := store.BulkInsertSnapshots(ctx, snapshots)
		require.NoError(t, err)

		// Verify data was inserted
		req := analytics.QueryRequest{
			AccountExternalIDsByProvider: map[string][]string{"aws": {"123456789012"}},
			StartDate:                    now.Add(-24 * time.Hour),
			EndDate:                      now,
		}
		results, err := store.QuerySavings(ctx, req)
		require.NoError(t, err)
		assert.Len(t, results, 2)
	})
}

func TestPostgresAnalyticsStore_PartitionManagement(t *testing.T) {
	ctx := context.Background()

	// A fresh migrated database of its own, on the package's shared server
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())

	// Create store
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	t.Run("create partition for specific month", func(t *testing.T) {
		// Create partition for a future month
		futureMonth := time.Now().AddDate(1, 0, 0)
		err := store.CreatePartition(ctx, futureMonth)
		assert.NoError(t, err)

		// Creating the same partition again should not error
		err = store.CreatePartition(ctx, futureMonth)
		assert.NoError(t, err)
	})

	t.Run("create partitions for range", func(t *testing.T) {
		startDate := time.Now().AddDate(0, 6, 0)
		endDate := time.Now().AddDate(0, 8, 0)

		err := store.CreatePartitionsForRange(ctx, startDate, endDate)
		assert.NoError(t, err)
	})

	t.Run("drop old partitions with high retention", func(t *testing.T) {
		// Use high retention value so nothing gets dropped
		err := store.DropOldPartitions(ctx, 120) // 10 years
		assert.NoError(t, err)
	})
}

func TestPostgresAnalyticsStore_RefreshMaterializedViews(t *testing.T) {
	ctx := context.Background()

	// A fresh migrated database of its own, on the package's shared server
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())

	// Create store
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	t.Run("refresh materialized views", func(t *testing.T) {
		err := store.RefreshMaterializedViews(ctx)
		assert.NoError(t, err)
	})
}

func TestPostgresAnalyticsStore_Close(t *testing.T) {
	ctx := context.Background()

	// Empty database: Close needs a connection, not a schema
	container := testhelpers.RequireEmptyDatabase(ctx, t)

	// Create store
	store := analytics.NewPostgresAnalyticsStore(container.DB)

	t.Run("close returns nil", func(t *testing.T) {
		err := store.Close()
		assert.NoError(t, err)
	})
}
