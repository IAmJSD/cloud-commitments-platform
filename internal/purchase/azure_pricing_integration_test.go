//go:build integration

package purchase

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

type azurePricingPostgresStore struct {
	*MockConfigStore
	pricing *config.PostgresStore
}

func (s *azurePricingPostgresStore) ListStoredRecommendations(ctx context.Context, filter config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return s.pricing.ListStoredRecommendations(ctx, filter)
}

func TestApproveAzurePricingRequiresMigrationAndRecollection(t *testing.T) {
	ctx := context.Background()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() {
		if cleanupErr := container.Cleanup(ctx); cleanupErr != nil {
			t.Logf("container cleanup: %v", cleanupErr)
		}
	})
	pool := container.DB.Pool()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, rampMigrationsPath(), 105))
	pricing := config.NewPostgresStore(container.DB)
	rec := armedExecution("azure", "compute", 0, "pending").Recommendations[0]
	rec.ID, rec.UpfrontCost = "azure-old-units", 70
	rec.Payment = "upfront"
	collects := []config.SuccessfulCollect{{Provider: "azure"}}
	// Insert the pre-106 row directly: the store's upsert targets the availability_zone
	// column added by migration 000109, which does not exist at version 105.
	payload, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO recommendations
		    (collected_at, cloud_account_id, provider, service, region, resource_type, engine, payload,
		     upfront_cost, monthly_savings, term, payment_option)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		time.Now(), rec.CloudAccountID, rec.Provider, rec.Service, rec.Region, rec.ResourceType, rec.Engine, payload,
		rec.UpfrontCost, rec.Savings, rec.Term, rec.Payment)
	require.NoError(t, err)

	approve := func(t *testing.T, expected error, purchases int) {
		t.Helper()
		exec := armedExecution("azure", "compute", 0, "pending")
		exec.PlanID = ""
		exec.Recommendations = []config.RecommendationRecord{rec}
		mgr, store, recorder := armedHarness(t, common.ServiceCompute)
		mgr.config = &azurePricingPostgresStore{MockConfigStore: store, pricing: pricing}
		expectClaim(store, exec, []string{"pending", "notified"})
		var saved *config.PurchaseExecution
		store.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
			copy := *e
			saved = &copy
			return nil
		}
		_, _, approveErr := mgr.ApproveAndExecute(ctx, exec.ExecutionID, "operator@example.com", nil, nil)
		require.Equal(t, purchases, recorder.count())
		require.NotNil(t, saved)
		if purchases == 1 {
			require.NoError(t, approveErr)
			require.Equal(t, "completed", saved.Status)
		} else {
			require.Error(t, approveErr)
			require.Equal(t, "failed", saved.Status)
			if expected != nil {
				require.ErrorIs(t, approveErr, expected)
			} else {
				require.NotErrorIs(t, approveErr, ErrStaleAzurePricing)
			}
		}
		store.AssertExpectations(t)
	}

	t.Run("identical old positive prices at 105", func(t *testing.T) { approve(t, ErrStaleAzurePricing, 0) })
	_, err = pool.Exec(ctx, `UPDATE schema_migrations SET version = 106, dirty = true`)
	require.NoError(t, err)
	t.Run("dirty 106", func(t *testing.T) { approve(t, config.ErrAzurePricingNotReady, 0) })
	_, err = pool.Exec(ctx, `TRUNCATE schema_migrations`)
	require.NoError(t, err)
	t.Run("missing migration row", func(t *testing.T) { approve(t, config.ErrAzurePricingNotReady, 0) })
	_, err = pool.Exec(ctx, `ALTER TABLE schema_migrations RENAME TO azure_pricing_fixture_migrations`)
	require.NoError(t, err)
	t.Run("migration query failure", func(t *testing.T) { approve(t, nil, 0) })
	_, err = pool.Exec(ctx, `ALTER TABLE azure_pricing_fixture_migrations RENAME TO schema_migrations`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO schema_migrations (version, dirty) VALUES (105, false)`)
	require.NoError(t, err)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, rampMigrationsPath(), 106))
	t.Run("106 invalidated old prices", func(t *testing.T) { approve(t, ErrStaleAzurePricing, 0) })
	rec.UpfrontCost = 700
	// The store's upsert needs the availability_zone, platform and tenancy columns and the
	// 11-column index from migration 000111, and resets
	// recommendations_state.last_collection_partial, added by migration 000129 (issue #87).
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, rampMigrationsPath(), 129))
	require.NoError(t, pricing.UpsertRecommendations(ctx, time.Now().Add(time.Minute), []config.RecommendationRecord{rec}, collects))
	t.Run("recollected prices purchase once", func(t *testing.T) { approve(t, nil, 1) })
}
