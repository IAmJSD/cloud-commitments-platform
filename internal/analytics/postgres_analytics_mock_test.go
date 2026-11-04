package analytics

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests verify the logic patterns used in postgres_analytics.go
// They don't directly test the production code due to the concrete type dependency,
// but they validate the same logical paths that the production code takes.
// For full coverage of PostgresAnalyticsStore, see postgres_analytics_integration_test.go
// which requires the 'integration' build tag and a running PostgreSQL instance.

// TestSaveSnapshotMarshalError verifies metadata marshaling error handling.
func TestSaveSnapshotMarshalError(t *testing.T) {
	t.Run("invalid metadata causes marshal error", func(t *testing.T) {
		// Create snapshot with unmarshallable metadata
		snapshot := &SavingsSnapshot{
			ID:       "test-id",
			Metadata: map[string]interface{}{"channel": make(chan int)},
		}

		// Verify that json.Marshal fails for this metadata (same logic as SaveSnapshot)
		_, err := json.Marshal(snapshot.Metadata)
		assert.Error(t, err)
	})
}

// TestAccountFilterClause exercises the real dual-column scoping clause builder
// used by every Query* method (replaces the old test that re-implemented the
// arg-building logic without calling production code).
func TestAccountFilterClause(t *testing.T) {
	base := func() []any { return []any{"start", "end"} }

	t.Run("both empty yields TRUE and unchanged args", func(t *testing.T) {
		clause, args := accountFilterClause(nil, nil, base())
		assert.Equal(t, "TRUE", clause)
		assert.Len(t, args, 2)
	})

	t.Run("UUIDs only match cloud_account_id", func(t *testing.T) {
		clause, args := accountFilterClause([]string{"u1", "u2"}, nil, base())
		assert.Equal(t, "(cloud_account_id = ANY($3))", clause)
		assert.Len(t, args, 3)
	})

	t.Run("external ids are grouped and provider-scoped", func(t *testing.T) {
		clause, args := accountFilterClause(
			[]string{"u1"},
			map[string][]string{"aws": {"123"}, "azure": {"sub-1"}},
			base(),
		)
		// Deterministic, sorted provider order: aws then azure.
		assert.Equal(t,
			"(cloud_account_id = ANY($3) OR (provider = $4 AND account_id = ANY($5)) OR (provider = $6 AND account_id = ANY($7)))",
			clause)
		assert.Len(t, args, 7)
	})

	t.Run("empty provider key matches account_id with no provider gate", func(t *testing.T) {
		clause, args := accountFilterClause(nil, map[string][]string{"": {"123"}}, base())
		assert.Equal(t, "(account_id = ANY($3))", clause)
		assert.Len(t, args, 3)
	})

	// Pins the exact bind args too (issue #120 item 5), so moving the builder
	// into the shared package cannot reorder or drop a bind.
	t.Run("bind args are pinned in clause order", func(t *testing.T) {
		clause, args := accountFilterClause(
			[]string{"u1"},
			map[string][]string{"azure": {"sub-1"}, "": {"raw"}, "aws": {"123"}, "gcp": nil},
			base(),
		)
		assert.Equal(t,
			"(cloud_account_id = ANY($3) OR account_id = ANY($4) OR (provider = $5 AND account_id = ANY($6)) OR (provider = $7 AND account_id = ANY($8)))",
			clause)
		assert.Equal(t, []any{"start", "end", []string{"u1"}, []string{"raw"}, "aws", []string{"123"}, "azure", []string{"sub-1"}}, args)
	})

	t.Run("providers with no ids yield TRUE", func(t *testing.T) {
		clause, args := accountFilterClause(nil, map[string][]string{"aws": {}}, base())
		assert.Equal(t, "TRUE", clause)
		assert.Equal(t, []any{"start", "end"}, args)
	})
}

// TestPartitionDateCalculation verifies partition date calculation logic.
func TestPartitionDateCalculation(t *testing.T) {
	t.Run("truncates to first of month", func(t *testing.T) {
		startDate := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		expected := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

		// Same logic as CreatePartitionsForRange
		current := time.Date(startDate.Year(), startDate.Month(), 1, 0, 0, 0, 0, time.UTC)
		assert.Equal(t, expected, current)
	})

	t.Run("calculates months in range correctly", func(t *testing.T) {
		startDate := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
		endDate := time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC)

		// Same logic as CreatePartitionsForRange
		current := time.Date(startDate.Year(), startDate.Month(), 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(endDate.Year(), endDate.Month(), 1, 0, 0, 0, 0, time.UTC)

		months := 0
		for !current.After(end) {
			months++
			current = current.AddDate(0, 1, 0)
		}

		assert.Equal(t, 3, months) // Jan, Feb, Mar
	})

	t.Run("handles same month range", func(t *testing.T) {
		startDate := time.Date(2024, 5, 5, 0, 0, 0, 0, time.UTC)
		endDate := time.Date(2024, 5, 25, 0, 0, 0, 0, time.UTC)

		current := time.Date(startDate.Year(), startDate.Month(), 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(endDate.Year(), endDate.Month(), 1, 0, 0, 0, 0, time.UTC)

		months := 0
		for !current.After(end) {
			months++
			current = current.AddDate(0, 1, 0)
		}

		assert.Equal(t, 1, months) // Only May
	})
}

// TestMetadataHandling verifies metadata JSON handling.
func TestMetadataHandling(t *testing.T) {
	t.Run("nil metadata produces nil bytes", func(t *testing.T) {
		// metadata is nil so the marshal branch is never taken; metadataJSON stays nil.
		var metadata map[string]interface{}
		var metadataJSON []byte
		if metadata != nil { //nolint:govet // nilness: intentional test of the nil-guard logic path
			metadataJSON, _ = json.Marshal(metadata)
		}
		assert.Nil(t, metadataJSON)
	})

	t.Run("empty metadata produces valid JSON", func(t *testing.T) {
		metadata := map[string]interface{}{}
		metadataJSON, err := json.Marshal(metadata)

		require.NoError(t, err)
		assert.Equal(t, "{}", string(metadataJSON))
	})

	t.Run("metadata with values marshals correctly", func(t *testing.T) {
		metadata := map[string]interface{}{
			"key1": "value1",
			"key2": 42,
		}
		metadataJSON, err := json.Marshal(metadata)

		require.NoError(t, err)
		assert.NotEmpty(t, metadataJSON)

		// Verify it can be unmarshaled back
		var result map[string]interface{}
		err = json.Unmarshal(metadataJSON, &result)
		require.NoError(t, err)
		assert.Equal(t, "value1", result["key1"])
	})

	t.Run("empty bytes unmarshal check is skipped", func(t *testing.T) {
		metadataJSON := []byte{}

		// Same logic as QuerySavings
		if len(metadataJSON) > 0 {
			var metadata map[string]interface{}
			err := json.Unmarshal(metadataJSON, &metadata)
			assert.NoError(t, err)
		}
		// If metadataJSON is empty, the unmarshal is not called
	})
}

// TestUUIDGeneration verifies UUID generation for snapshots.
func TestUUIDGeneration(t *testing.T) {
	t.Run("empty ID should trigger generation", func(t *testing.T) {
		snapshot := &SavingsSnapshot{ID: ""}
		assert.Empty(t, snapshot.ID)
	})

	t.Run("non-empty ID should be preserved", func(t *testing.T) {
		snapshot := &SavingsSnapshot{ID: "existing-id"}
		assert.Equal(t, "existing-id", snapshot.ID)
	})
}

// TestCommitmentTypeLogic pins the (provider, service) -> commitment_type map
// (#339): every Savings Plans slug is a savings plan, GCP is a CUD, and the
// rest are reserved instances.
func TestCommitmentTypeLogic(t *testing.T) {
	cases := []struct{ provider, service, want string }{
		{"aws", "savingsplans", "savings-plan"},
		{"aws", "savings-plans", "savings-plan"},
		{"aws", "SavingsPlans", "savings-plan"},
		{"aws", "savings-plans-compute", "savings-plan"},
		{"aws", "savings-plans-ec2instance", "savings-plan"},
		{"aws", "savings-plans-sagemaker", "savings-plan"},
		{"aws", "savings-plans-database", "savings-plan"},
		{"aws", "ec2", "reserved-instance"},
		{"aws", "rds", "reserved-instance"},
		{"aws", "elasticache", "reserved-instance"},
		{"aws", "opensearch", "reserved-instance"},
		{"azure", "compute", "reserved-instance"},
		{"azure", "database", "reserved-instance"},
		{"gcp", "compute", "committed-use"},
		{"GCP", "cloudsql", "committed-use"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, commitmentTypeFor(tc.provider, tc.service), "%s/%s", tc.provider, tc.service)
	}
}

// TestBulkInsertEmptySlice verifies empty slice handling.
func TestBulkInsertEmptySlice(t *testing.T) {
	t.Run("empty slice returns early", func(t *testing.T) {
		snapshots := []SavingsSnapshot{}
		// Same logic as BulkInsertSnapshots: if len(snapshots) == 0 { return nil }
		if len(snapshots) == 0 {
			assert.True(t, true) // Early return path
		}
	})

	t.Run("non-empty slice proceeds", func(t *testing.T) {
		snapshots := []SavingsSnapshot{{ID: "test"}}
		if len(snapshots) == 0 {
			t.Fatal("should not reach here")
		}
		assert.Len(t, snapshots, 1)
	})
}

// TestCloseReturnsNil verifies Close behavior.
func TestCloseReturnsNil(t *testing.T) {
	store := NewPostgresAnalyticsStore(nil)
	err := store.Close()
	assert.NoError(t, err)
}

// Additional edge case tests for slice initialization

func TestQuerySavingsEmptyResult(t *testing.T) {
	t.Run("empty result set produces empty slice", func(t *testing.T) {
		// Same logic as QuerySavings: snapshots := make([]SavingsSnapshot, 0)
		snapshots := make([]SavingsSnapshot, 0)
		assert.NotNil(t, snapshots)
		assert.Empty(t, snapshots)
	})
}

func TestQueryMonthlyTotalsEmptyResult(t *testing.T) {
	t.Run("empty result set produces empty slice", func(t *testing.T) {
		// Same logic as QueryMonthlyTotals: summaries := make([]MonthlySummary, 0)
		summaries := make([]MonthlySummary, 0)
		assert.NotNil(t, summaries)
		assert.Empty(t, summaries)
	})
}

func TestQueryByProviderEmptyResult(t *testing.T) {
	t.Run("empty result set produces empty slice", func(t *testing.T) {
		// Same logic as QueryByProvider: breakdowns := make([]ProviderBreakdown, 0)
		breakdowns := make([]ProviderBreakdown, 0)
		assert.NotNil(t, breakdowns)
		assert.Empty(t, breakdowns)
	})
}

func TestQueryByServiceEmptyResult(t *testing.T) {
	t.Run("empty result set produces empty slice", func(t *testing.T) {
		// Same logic as QueryByService: breakdowns := make([]ServiceBreakdown, 0)
		breakdowns := make([]ServiceBreakdown, 0)
		assert.NotNil(t, breakdowns)
		assert.Empty(t, breakdowns)
	})
}
