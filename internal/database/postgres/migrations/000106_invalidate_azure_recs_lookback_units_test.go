//go:build integration

package migrations_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration_InvalidateAzureRecsLookbackUnits(t *testing.T) {
	ctx := context.Background()
	path := getMigrationsPath()
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 105))
	fixtures := []string{
		`{"provider":"azure","payment":"upfront","id":"azure-upfront","term":3,"count":1,"upfront_cost":38.5,"monthly_cost":12.1,"savings":40,"resource_type":"Standard_D4s_v5"}`,
		`{"provider":"azure","payment":"monthly","id":"azure-monthly","term":3,"count":1,"upfront_cost":4.2,"monthly_cost":55,"savings":40,"resource_type":"Standard_D4s_v5"}`,
		`{"provider":"aws","payment":"upfront","id":"aws-upfront","term":3,"count":1,"upfront_cost":500,"monthly_cost":0,"savings":25,"resource_type":"Standard_D4s_v5","on_demand_cost":90}`,
	}
	expected := make([]map[string]any, len(fixtures))
	for i, payload := range fixtures {
		require.NoError(t, json.Unmarshal([]byte(payload), &expected[i]))
		_, err := pool.Exec(ctx, `INSERT INTO recommendations
   (collected_at,provider,service,region,resource_type,engine,payload,upfront_cost,monthly_savings,term,payment_option)
   VALUES (NOW(),$1,'compute','westeurope','Standard_D4s_v5','',$2::jsonb,0,0,3,$3)`,
			expected[i]["provider"], payload, expected[i]["payment"])
		require.NoError(t, err)
		if expected[i]["provider"] == "azure" {
			expected[i]["upfront_cost"] = nil
			expected[i]["monthly_cost"] = nil
		}
	}
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 106))
	sql, err := os.ReadFile(filepath.Join(path, "000106_invalidate_azure_recs_lookback_units.up.sql"))
	require.NoError(t, err)
	for pass := 0; pass < 3; pass++ {
		if pass == 1 {
			_, err = pool.Exec(ctx, string(sql))
			require.NoError(t, err)
		}
		if pass == 2 {
			require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 105))
		}
		for _, payload := range expected {
			var got string
			require.NoError(t, pool.QueryRow(ctx, `SELECT payload::text FROM recommendations WHERE provider=$1 AND payment_option=$2`, payload["provider"], payload["payment"]).Scan(&got))
			want, err := json.Marshal(payload)
			require.NoError(t, err)
			assert.JSONEq(t, string(want), got)
		}
		var count int
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM recommendations`).Scan(&count))
		assert.Equal(t, 3, count)
	}
}
