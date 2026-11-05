//go:build integration

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// txCountingStore counts the transactions the purchase path opens.
type txCountingStore struct {
	*config.PostgresStore
	txs int
}

func (s *txCountingStore) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	s.txs++
	return s.PostgresStore.WithTx(ctx, fn)
}

// Without the az_config expand migrations the new code must refuse loudly:
// the purchase path before it opens a transaction, collection and suppression
// listing with the typed error, not a SQL failure.
func TestAZConfigGate_RefusesBeforeExpandMigrations(t *testing.T) {
	ctx := context.Background()
	pg := testhelpers.RequireEmptyDatabase(ctx, t)
	pool, path := pg.DB.Pool(), getMigrationsPath()
	store := &txCountingStore{PostgresStore: config.NewPostgresStore(pg.DB)}
	h := &Handler{config: store}
	exec := &config.PurchaseExecution{ExecutionID: uuid.NewString()}

	for _, version := range []uint{154, 155} { // before both expands, then after only the first
		require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))

		_, err := h.persistExecutionAndSuppressions(ctx, exec, nil, "creator", "key")
		ce, ok := IsClientError(err)
		require.True(t, ok, "version %d: %v", version, err)
		assert.Equal(t, 503, ce.code)
		assert.Contains(t, ce.message, "000155 and 000156")
		assert.Zero(t, store.txs, "no transaction may open before the gate")

		rec := expandOrderRDSRec("single-az", 100)
		err = store.UpsertRecommendations(ctx, time.Now(), []config.RecommendationRecord{rec}, []config.SuccessfulCollect{{Provider: "aws"}})
		assert.True(t, errors.Is(err, config.ErrAZConfigMigrationNotReady), "collection: %v", err)

		_, err = store.ListActiveSuppressions(ctx)
		assert.True(t, errors.Is(err, config.ErrAZConfigMigrationNotReady), "suppression listing: %v", err)

		sup := &config.PurchaseSuppression{ExecutionID: uuid.NewString(), Provider: "aws", Service: "rds", Region: "us-east-1",
			ResourceType: "db.r5.large", SuppressedCount: 1, ExpiresAt: time.Now().Add(time.Hour)}
		err = store.CreateSuppression(ctx, sup)
		assert.True(t, errors.Is(err, config.ErrAZConfigMigrationNotReady), "CreateSuppression: %v", err)
		err = store.WithTx(ctx, func(tx pgx.Tx) error { return store.CreateSuppressionTx(ctx, tx, sup) })
		assert.True(t, errors.Is(err, config.ErrAZConfigMigrationNotReady), "CreateSuppressionTx: %v", err)
		store.txs = 0 // the direct WithTx above is not the purchase path
	}

	for _, version := range []uint{156, 157} {
		require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, version))
		require.NoError(t, h.requireAZConfigSchema(ctx), "version %d", version)
		_, err := store.ListActiveSuppressions(ctx)
		require.NoError(t, err, "version %d", version)
	}
}
