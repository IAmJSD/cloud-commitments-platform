package analytics

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockAnalyticsStore implements AnalyticsStore for testing.
type mockAnalyticsStore struct {
	saveSnapshotFunc             func(ctx context.Context, snapshot *SavingsSnapshot) error
	bulkInsertSnapshotsFunc      func(ctx context.Context, snapshots []SavingsSnapshot) error
	querySavingsFunc             func(ctx context.Context, req QueryRequest) ([]SavingsSnapshot, error)
	queryMonthlyTotalsFunc       func(ctx context.Context, accountUUIDs []string, accountExternalIDsByProvider map[string][]string, months int) ([]MonthlySummary, error)
	queryByProviderFunc          func(ctx context.Context, accountUUIDs []string, accountExternalIDsByProvider map[string][]string, startDate, endDate time.Time) ([]ProviderBreakdown, error)
	queryByServiceFunc           func(ctx context.Context, accountUUIDs []string, accountExternalIDsByProvider map[string][]string, provider string, startDate, endDate time.Time) ([]ServiceBreakdown, error)
	createPartitionFunc          func(ctx context.Context, forMonth time.Time) error
	createFuturePartitionsFunc   func(ctx context.Context, monthsAhead int) error
	dropOldPartitionsFunc        func(ctx context.Context, retentionMonths int) error
	createPartitionsForRangeFunc func(ctx context.Context, startDate, endDate time.Time) error
	refreshMaterializedViewsFunc func(ctx context.Context) error
	closeFunc                    func() error

	savedSnapshots []SavingsSnapshot
}

func (m *mockAnalyticsStore) SaveSnapshot(ctx context.Context, snapshot *SavingsSnapshot) error {
	if m.saveSnapshotFunc != nil {
		return m.saveSnapshotFunc(ctx, snapshot)
	}
	m.savedSnapshots = append(m.savedSnapshots, *snapshot)
	return nil
}

func (m *mockAnalyticsStore) BulkInsertSnapshots(ctx context.Context, snapshots []SavingsSnapshot) error {
	if m.bulkInsertSnapshotsFunc != nil {
		return m.bulkInsertSnapshotsFunc(ctx, snapshots)
	}
	m.savedSnapshots = append(m.savedSnapshots, snapshots...)
	return nil
}

func (m *mockAnalyticsStore) QuerySavings(ctx context.Context, req QueryRequest) ([]SavingsSnapshot, error) {
	if m.querySavingsFunc != nil {
		return m.querySavingsFunc(ctx, req)
	}
	return nil, nil
}

func (m *mockAnalyticsStore) QueryMonthlyTotals(ctx context.Context, accountUUIDs []string, accountExternalIDsByProvider map[string][]string, months int) ([]MonthlySummary, error) {
	if m.queryMonthlyTotalsFunc != nil {
		return m.queryMonthlyTotalsFunc(ctx, accountUUIDs, accountExternalIDsByProvider, months)
	}
	return nil, nil
}

func (m *mockAnalyticsStore) QueryByProvider(ctx context.Context, accountUUIDs []string, accountExternalIDsByProvider map[string][]string, startDate, endDate time.Time) ([]ProviderBreakdown, error) {
	if m.queryByProviderFunc != nil {
		return m.queryByProviderFunc(ctx, accountUUIDs, accountExternalIDsByProvider, startDate, endDate)
	}
	return nil, nil
}

func (m *mockAnalyticsStore) QueryByService(ctx context.Context, accountUUIDs []string, accountExternalIDsByProvider map[string][]string, provider string, startDate, endDate time.Time) ([]ServiceBreakdown, error) {
	if m.queryByServiceFunc != nil {
		return m.queryByServiceFunc(ctx, accountUUIDs, accountExternalIDsByProvider, provider, startDate, endDate)
	}
	return nil, nil
}

func (m *mockAnalyticsStore) CreatePartition(ctx context.Context, forMonth time.Time) error {
	if m.createPartitionFunc != nil {
		return m.createPartitionFunc(ctx, forMonth)
	}
	return nil
}

func (m *mockAnalyticsStore) CreateFuturePartitions(ctx context.Context, monthsAhead int) error {
	if m.createFuturePartitionsFunc != nil {
		return m.createFuturePartitionsFunc(ctx, monthsAhead)
	}
	return nil
}

func (m *mockAnalyticsStore) DropOldPartitions(ctx context.Context, retentionMonths int) error {
	if m.dropOldPartitionsFunc != nil {
		return m.dropOldPartitionsFunc(ctx, retentionMonths)
	}
	return nil
}

func (m *mockAnalyticsStore) CreatePartitionsForRange(ctx context.Context, startDate, endDate time.Time) error {
	if m.createPartitionsForRangeFunc != nil {
		return m.createPartitionsForRangeFunc(ctx, startDate, endDate)
	}
	return nil
}

func (m *mockAnalyticsStore) RefreshMaterializedViews(ctx context.Context) error {
	if m.refreshMaterializedViewsFunc != nil {
		return m.refreshMaterializedViewsFunc(ctx)
	}
	return nil
}

func (m *mockAnalyticsStore) Close() error {
	if m.closeFunc != nil {
		return m.closeFunc()
	}
	return nil
}

// mockConfigStore is the shared mocks.MockConfigStore with GetActivePurchaseHistory
// made injectable: that is the only config-store read the collector makes. Every
// other config.StoreInterface method is promoted from the shared mock, so a new
// store method needs a stub in internal/mocks only, not here as well (issue #76).
type mockConfigStore struct {
	mocks.MockConfigStore
	getActivePurchaseHistoryFunc func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error)
}

func (m *mockConfigStore) GetActivePurchaseHistory(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
	if m.getActivePurchaseHistoryFunc != nil {
		return m.getActivePurchaseHistoryFunc(ctx, asOf, accountIDs, externalIDsByProvider)
	}
	return nil, nil
}

// strPtr is a test helper for *string fields.
func strPtr(s string) *string { return &s }

// activeRecord returns a still-active purchase made 3 months ago with the given
// fields; helper to keep the table-driven tests terse.
func activeRecord(provider, service, region string, term int, savings, upfront float64) config.PurchaseHistoryRecord {
	return config.PurchaseHistoryRecord{
		AccountID:        "123456789012",
		PurchaseID:       "p-" + service + "-" + region,
		Timestamp:        time.Now().AddDate(0, -3, 0),
		Provider:         provider,
		Service:          service,
		Region:           region,
		Term:             term,
		EstimatedSavings: savings,
		UpfrontCost:      new(float64(upfront)),
	}
}

func newTestCollector(t *testing.T, store *mockAnalyticsStore, cfgStore *mockConfigStore) *Collector {
	t.Helper()
	collector, err := NewCollector(CollectorConfig{AnalyticsStore: store}, cfgStore)
	require.NoError(t, err)
	return collector
}

// TestNewCollector tests the NewCollector function.
func TestNewCollector(t *testing.T) {
	t.Run("returns error when analytics store is nil", func(t *testing.T) {
		collector, err := NewCollector(CollectorConfig{AnalyticsStore: nil}, &mockConfigStore{})
		assert.Nil(t, collector)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "analytics store is required")
	})

	t.Run("returns error when config store is nil", func(t *testing.T) {
		collector, err := NewCollector(CollectorConfig{AnalyticsStore: &mockAnalyticsStore{}}, nil)
		assert.Nil(t, collector)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "config store is required")
	})

	t.Run("creates collector successfully with valid inputs", func(t *testing.T) {
		collector, err := NewCollector(CollectorConfig{AnalyticsStore: &mockAnalyticsStore{}}, &mockConfigStore{})
		require.NoError(t, err)
		assert.NotNil(t, collector)
	})
}

// TestCollectorCollect tests the Collect method.
func TestCollectorCollect(t *testing.T) {
	t.Run("returns error when GetAllPurchaseHistory fails", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return nil, errors.New("database connection failed")
			},
		}
		err := newTestCollector(t, store, cfgStore).Collect(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get active purchase history")
		assert.Contains(t, err.Error(), "database connection failed")
	})

	t.Run("handles empty purchase history", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		assert.Empty(t, store.savedSnapshots)
	})

	t.Run("skips expired purchases", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		expired := config.PurchaseHistoryRecord{
			AccountID: "123456789012", Timestamp: time.Now().AddDate(-3, 0, 0),
			Provider: "aws", Service: "rds", Region: "us-east-1",
			Term: 1, EstimatedSavings: 100, UpfrontCost: new(float64(500)),
		}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{expired}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		assert.Empty(t, store.savedSnapshots)
	})

	t.Run("processes active purchases and creates snapshots", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{activeRecord("aws", "rds", "us-east-1", 1, 720, 1000)}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		require.Len(t, store.savedSnapshots, 1)
		s := store.savedSnapshots[0]
		assert.Equal(t, "123456789012", s.AccountID)
		assert.Equal(t, "aws", s.Provider)
		assert.Equal(t, "rds", s.Service)
		assert.Equal(t, string(common.CommitmentReservedInstance), s.CommitmentType)
		assert.Greater(t, s.TotalSavings, 0.0)
		require.NotNil(t, s.TotalCommitment)
		assert.Greater(t, *s.TotalCommitment, 0.0)
	})

	t.Run("aggregates multiple purchases for same bucket", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{
					activeRecord("aws", "rds", "us-east-1", 1, 100, 500),
					activeRecord("aws", "rds", "us-east-1", 1, 200, 1000),
				}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		require.Len(t, store.savedSnapshots, 1)
		assert.Equal(t, 2, store.savedSnapshots[0].Metadata["active_purchases"])
	})

	t.Run("creates separate snapshots per region and provider", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{
					activeRecord("aws", "rds", "us-east-1", 1, 100, 500),
					activeRecord("aws", "rds", "us-west-2", 1, 150, 700),
					activeRecord("gcp", "cloudsql", "us-east1", 1, 150, 700),
				}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		assert.Len(t, store.savedSnapshots, 3)
	})

	// #339 (A06-019): the type comes from (provider, service). Purchase history
	// stores Savings Plans as "savingsplans" or a per-plan-type slug, never the
	// "SavingsPlans" the old mapper matched, and GCP purchases are CUDs.
	t.Run("derives commitment type from provider and service", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{
					activeRecord("aws", "savingsplans", "us-east-1", 1, 500, 2000),
					activeRecord("aws", "savings-plans-compute", "us-east-1", 1, 500, 2000),
					activeRecord("gcp", "compute", "us-central1", 1, 300, 0),
					activeRecord("azure", "compute", "eastus", 1, 200, 900),
					activeRecord("aws", "ec2", "us-east-1", 1, 100, 400),
				}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		got := make(map[string]string, len(store.savedSnapshots))
		for _, s := range store.savedSnapshots {
			got[s.Provider+"/"+s.Service] = s.CommitmentType
			require.NoError(t, validateCommitmentType(s.CommitmentType), "the store CHECK must admit what the collector emits")
		}
		assert.Equal(t, map[string]string{
			"aws/savingsplans":          string(common.CommitmentSavingsPlan),
			"aws/savings-plans-compute": string(common.CommitmentSavingsPlan),
			"gcp/compute":               string(common.CommitmentCUD),
			"azure/compute":             string(common.CommitmentReservedInstance),
			"aws/ec2":                   string(common.CommitmentReservedInstance),
		}, got)
	})

	t.Run("returns error when BulkInsertSnapshots fails", func(t *testing.T) {
		store := &mockAnalyticsStore{
			bulkInsertSnapshotsFunc: func(ctx context.Context, snapshots []SavingsSnapshot) error {
				return errors.New("bulk insert failed")
			},
		}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{activeRecord("aws", "rds", "us-east-1", 1, 100, 500)}, nil
			},
		}
		err := newTestCollector(t, store, cfgStore).Collect(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to save snapshots")
	})

	t.Run("calculates monthly savings run-rate and amortized commitment correctly", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		const monthlySavings, upfront = 720.0, 8760.0
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{activeRecord("aws", "rds", "us-east-1", 1, monthlySavings, upfront)}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		require.Len(t, store.savedSnapshots, 1)
		assert.InDelta(t, monthlySavings, store.savedSnapshots[0].TotalSavings, 0.001)
		assert.InDelta(t, upfront/(1*MonthsPerYear), *store.savedSnapshots[0].TotalCommitment, 0.001)
	})

	// ── Regression tests for the latent data bugs (#1023) ──

	t.Run("H1: Term<=0 row is skipped and does not corrupt aggregates", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		good := activeRecord("aws", "rds", "us-east-1", 1, 100, 500)
		badTerm := activeRecord("aws", "rds", "us-east-1", 0, 999, 999) // Term==0 -> +Inf commitment pre-fix
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{good, badTerm}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		require.Len(t, store.savedSnapshots, 1)
		s := store.savedSnapshots[0]
		// Only the good row contributed: no +Inf/NaN, exactly one active purchase.
		assert.Equal(t, 1, s.Metadata["active_purchases"])
		require.NotNil(t, s.TotalCommitment)
		assert.False(t, math.IsInf(*s.TotalCommitment, 0), "commitment must not be Inf")
		assert.False(t, math.IsNaN(*s.TotalCommitment), "commitment must not be NaN")
		assert.InDelta(t, 500.0/(1*MonthsPerYear), *s.TotalCommitment, 0.001)
	})

	t.Run("H1: a negative Term is also skipped", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{activeRecord("aws", "rds", "us-east-1", -1, 100, 500)}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		assert.Empty(t, store.savedSnapshots, "a negative-term-only history yields no snapshot")
	})

	t.Run("H2: usage reflects real MonthlyCost; absent stays NULL not 0", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		withCost := activeRecord("aws", "rds", "us-east-1", 1, 100, 500)
		withCost.MonthlyCost = func() *float64 { v := 360.0; return &v }() // $360/mo
		noCost := activeRecord("aws", "ec2", "us-east-1", 1, 100, 500)     // MonthlyCost nil

		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{withCost, noCost}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		require.Len(t, store.savedSnapshots, 2)

		byService := map[string]SavingsSnapshot{}
		for _, s := range store.savedSnapshots {
			byService[s.Service] = s
		}
		// rds carried a recurring cost -> real, non-nil usage (monthly run-rate).
		require.NotNil(t, byService["rds"].TotalUsage)
		assert.InDelta(t, 360.0, *byService["rds"].TotalUsage, 0.001)
		// ec2 had no recurring cost -> usage is NULL (nil), never a placeholder 0.
		assert.Nil(t, byService["ec2"].TotalUsage)
		// coverage is never a placeholder 0 (no on-demand baseline source).
		assert.Nil(t, byService["rds"].CoveragePercentage)
		assert.Nil(t, byService["ec2"].CoveragePercentage)
	})

	t.Run("H3: cloud_account_id is populated and partitions the tenant boundary", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		tenantA := activeRecord("aws", "rds", "us-east-1", 1, 100, 500)
		tenantA.CloudAccountID = strPtr("11111111-1111-1111-1111-111111111111")
		tenantB := activeRecord("aws", "rds", "us-east-1", 1, 200, 800)
		tenantB.CloudAccountID = strPtr("22222222-2222-2222-2222-222222222222")
		// Same provider account string, different cloud_account_id -> must NOT merge.
		tenantB.AccountID = tenantA.AccountID

		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{tenantA, tenantB}, nil
			},
		}
		require.NoError(t, newTestCollector(t, store, cfgStore).Collect(context.Background()))
		require.Len(t, store.savedSnapshots, 2, "distinct cloud_account_id must not be merged")
		ids := map[string]bool{}
		for _, s := range store.savedSnapshots {
			require.NotNil(t, s.CloudAccountID)
			ids[*s.CloudAccountID] = true
		}
		assert.True(t, ids["11111111-1111-1111-1111-111111111111"])
		assert.True(t, ids["22222222-2222-2222-2222-222222222222"])
	})

	t.Run("ctx cancellation is terminal and surfaces an error", func(t *testing.T) {
		store := &mockAnalyticsStore{}
		cfgStore := &mockConfigStore{
			getActivePurchaseHistoryFunc: func(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]config.PurchaseHistoryRecord, error) {
				return []config.PurchaseHistoryRecord{activeRecord("aws", "rds", "us-east-1", 1, 100, 500)}, nil
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := newTestCollector(t, store, cfgStore).Collect(ctx)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, store.savedSnapshots, "no snapshots written on a canceled run")
	})
}

// TestConstants tests the exported constants.
func TestConstants(t *testing.T) {
	t.Run("HoursPerYear is correct", func(t *testing.T) {
		assert.Equal(t, 365*24, HoursPerYear)
		assert.Equal(t, 8760, HoursPerYear)
	})

	t.Run("MonthsPerYear is correct", func(t *testing.T) {
		assert.Equal(t, 12, MonthsPerYear)
	})
}
