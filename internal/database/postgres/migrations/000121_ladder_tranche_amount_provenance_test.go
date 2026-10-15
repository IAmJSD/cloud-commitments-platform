//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration_LadderTrancheAmountProvenance(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { require.NoError(t, container.Cleanup(context.Background())) })
	pool, path := container.DB.Pool(), getMigrationsPath()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 120))
	var id string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ladder_tranches
 (layer_type,amount_usd_hr,term,payment_option,scheduled_date,status)
 VALUES ('compute-sp',12.5,'1y','no_upfront',NOW(),'scheduled') RETURNING id`).Scan(&id))
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 121))
	var validated bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname='ladder_tranche_amount_provenance'`).Scan(&validated))
	require.False(t, validated)
	var amount string
	var basis, rate *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT amount_usd_hr::text,amount_basis,commitment_rate
 FROM ladder_tranches WHERE id=$1`, id).Scan(&amount, &basis, &rate))
	require.Equal(t, "12.500000", amount)
	require.Nil(t, basis)
	require.Nil(t, rate)
	for _, pair := range [][2]any{{"on_demand_equivalent", nil}, {"commitment", "1/3"}, {nil, nil}} {
		_, err := pool.Exec(ctx, `UPDATE ladder_tranches SET amount_basis=$2,commitment_rate=$3 WHERE id=$1`, id, pair[0], pair[1])
		require.NoError(t, err)
	}
	for _, pair := range [][2]any{{nil, "1/3"}, {"on_demand_equivalent", "1/3"}, {"commitment", nil}, {"commitment", ""}, {"unknown", nil}} {
		_, err := pool.Exec(ctx, `UPDATE ladder_tranches SET amount_basis=$2,commitment_rate=$3 WHERE id=$1`, id, pair[0], pair[1])
		require.Error(t, err, "invalid provenance pair %v", pair)
	}
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 122))
	require.NoError(t, pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname='ladder_tranche_amount_provenance'`).Scan(&validated))
	require.True(t, validated)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 121))
	require.NoError(t, pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname='ladder_tranche_amount_provenance'`).Scan(&validated))
	require.False(t, validated)
	_, err := pool.Exec(ctx, `UPDATE ladder_tranches SET amount_basis='commitment' WHERE id=$1`, id)
	require.Error(t, err)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 120))
	var columns int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
 WHERE table_name='ladder_tranches' AND column_name IN ('amount_basis','commitment_rate')`).Scan(&columns))
	require.Zero(t, columns)
	require.NoError(t, pool.QueryRow(ctx, `SELECT amount_usd_hr::text FROM ladder_tranches WHERE id=$1`, id).Scan(&amount))
	require.Equal(t, "12.500000", amount)
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 122))
	require.NoError(t, pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname='ladder_tranche_amount_provenance'`).Scan(&validated))
	require.True(t, validated)
	require.NoError(t, pool.QueryRow(ctx, `SELECT amount_usd_hr::text,amount_basis,commitment_rate FROM ladder_tranches WHERE id=$1`, id).Scan(&amount, &basis, &rate))
	require.Equal(t, "12.500000", amount)
	require.Nil(t, basis)
	require.Nil(t, rate)
}
