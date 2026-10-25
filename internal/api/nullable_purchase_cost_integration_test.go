//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/analytics"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNullablePurchaseCostPostgres(t *testing.T) {
	ctx := context.Background()
	// Empty, not migrated: the test walks the chain to 100 and then 101 itself.
	container := testhelpers.RequireEmptyDatabase(ctx, t)
	pool := container.DB.Pool()
	path := filepath.Join("..", "database", "postgres", "migrations")
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 100))
	store := config.NewPostgresStore(container.DB)
	now := time.Now().UTC().Truncate(time.Second)
	legacy := &config.PurchaseHistoryRecord{AccountID: "legacy", PurchaseID: "legacy", Timestamp: now, Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large", Count: 1, Term: 1, Payment: "no-upfront", UpfrontCost: new(float64), MonthlyCost: new(float64(40))}
	require.NoError(t, store.SavePurchaseHistory(ctx, legacy))
	require.NoError(t, migrations.MigrateToVersion(ctx, pool, path, 101))
	up, err := os.ReadFile(filepath.Join(path, "000101_nullable_purchase_upfront_cost.up.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err)
	loaded, err := store.GetPurchaseHistoryByPurchaseID(ctx, legacy.PurchaseID)
	require.NoError(t, err)
	require.NotNil(t, loaded.UpfrontCost)
	assert.Zero(t, *loaded.UpfrontCost)
	assert.Equal(t, legacy.MonthlyCost, loaded.MonthlyCost)

	client := NewPostgresAnalyticsClient(container.DB)
	for _, tc := range []struct {
		name  string
		costs []*float64
		want  *float64
	}{
		{"known", []*float64{new(float64(120)), new(float64(30))}, new(float64(150))},
		{"zero", []*float64{new(float64)}, new(float64)},
		{"unknown", []*float64{nil}, nil},
		{"mixed", []*float64{new(float64(120)), nil}, nil},
		{"empty", nil, new(float64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, cost := range tc.costs {
				record := *legacy
				record.AccountID = tc.name
				record.PurchaseID = fmt.Sprintf("%s-%d", tc.name, i)
				record.UpfrontCost = cost
				record.EstimatedSavings = 10
				require.NoError(t, store.SavePurchaseHistory(ctx, &record))
				single, singleErr := store.GetPurchaseHistoryByPurchaseID(ctx, record.PurchaseID)
				require.NoError(t, singleErr)
				assert.Equal(t, cost, single.UpfrontCost)
				assert.Equal(t, legacy.MonthlyCost, single.MonthlyCost)
			}
			rows, rowsErr := store.GetPurchaseHistory(ctx, tc.name, 100)
			require.NoError(t, rowsErr)
			require.Len(t, rows, len(tc.costs))
			assert.Equal(t, tc.want, summarizePurchaseHistory(rows).TotalUpfront)
			scope := map[string][]string{"aws": {tc.name}}
			active, activeErr := store.GetActivePurchaseHistory(ctx, now, nil, scope)
			require.NoError(t, activeErr)
			require.Len(t, active, len(tc.costs))
			points, summary, queryErr := client.QueryHistory(ctx, nil, scope, "aws", now.Add(-time.Hour), now.Add(time.Hour), "daily")
			require.NoError(t, queryErr)
			assert.Equal(t, tc.want, summary.TotalUpfront)
			assert.Equal(t, len(tc.costs), summary.TotalPurchases)
			assert.Equal(t, float64(len(tc.costs))*10, summary.TotalMonthlySavings)
			breakdown, breakdownErr := client.QueryBreakdown(ctx, nil, scope, now.Add(-time.Hour), now.Add(time.Hour), "service")
			require.NoError(t, breakdownErr)
			if len(tc.costs) > 0 {
				require.Len(t, points, 1)
				assert.Equal(t, tc.want, points[0].TotalUpfront)
				assert.Equal(t, tc.want, breakdown["ec2"].TotalUpfront)
			}
		})
	}
	for _, acrossDays := range []bool{false, true} {
		account := fmt.Sprintf("groups-%t", acrossDays)
		for i, service := range []string{"ec2", "rds"} {
			record := *legacy
			record.AccountID, record.PurchaseID, record.Service = account, account+service, service
			record.UpfrontCost = new(float64(120))
			if i == 1 {
				record.UpfrontCost = nil
				if acrossDays {
					record.Timestamp = now.Add(-24 * time.Hour)
				}
			}
			require.NoError(t, store.SavePurchaseHistory(ctx, &record))
		}
		points, summary, queryErr := client.QueryHistory(ctx, nil, map[string][]string{"aws": {account}}, "aws", now.Add(-48*time.Hour), now.Add(time.Hour), "daily")
		require.NoError(t, queryErr)
		assert.Nil(t, summary.TotalUpfront)
		if !acrossDays {
			require.Len(t, points, 1)
			assert.Nil(t, points[0].TotalUpfront)
		} else {
			require.Len(t, points, 2)
		}
	}
	_, err = pool.Exec(ctx, `UPDATE purchase_history SET revocation_in_flight=true WHERE purchase_id='unknown-0'`)
	require.NoError(t, err)
	inFlight, err := store.GetPurchaseHistoryInFlight(ctx)
	require.NoError(t, err)
	require.Len(t, inFlight, 1)
	assert.Nil(t, inFlight[0].UpfrontCost)

	snapshots := analytics.NewPostgresAnalyticsStore(container.DB)
	for i, cost := range []*float64{nil, nil, new(float64), new(float64(12.5))} {
		snapshot := analytics.SavingsSnapshot{Timestamp: now, AccountID: fmt.Sprintf("snapshot-%d", i), Provider: "aws", Service: "ec2", Region: "us-east-1", CommitmentType: "RI", TotalCommitment: cost, TotalSavings: 10}
		if i == 0 {
			require.NoError(t, snapshots.SaveSnapshot(ctx, &snapshot))
		} else {
			require.NoError(t, snapshots.BulkInsertSnapshots(ctx, []analytics.SavingsSnapshot{snapshot}))
		}
		loadedSnapshots, queryErr := snapshots.QuerySavings(ctx, analytics.QueryRequest{StartDate: now.Add(-time.Hour), EndDate: now.Add(time.Hour), AccountExternalIDsByProvider: map[string][]string{"aws": {snapshot.AccountID}}})
		require.NoError(t, queryErr)
		require.Len(t, loadedSnapshots, 1)
		assert.Equal(t, cost, loadedSnapshots[0].TotalCommitment)
		body, marshalErr := json.Marshal(loadedSnapshots[0])
		require.NoError(t, marshalErr)
		if cost == nil {
			assert.Contains(t, string(body), `"total_commitment":null`)
		}
	}
	_, err = pool.Exec(ctx, `INSERT INTO purchase_history (account_id,purchase_id,timestamp,provider,service,region,resource_type,term,payment) VALUES ('omitted','omitted',NOW(),'aws','ec2','us-east-1','m5.large',1,'all-upfront')`)
	require.NoError(t, err)
	var omitted *float64
	require.NoError(t, pool.QueryRow(ctx, `SELECT upfront_cost::float8 FROM purchase_history WHERE purchase_id='omitted'`).Scan(&omitted))
	assert.Nil(t, omitted)

	down, err := os.ReadFile(filepath.Join(path, "000101_nullable_purchase_upfront_cost.down.sql"))
	require.NoError(t, err)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, string(down))
	require.Error(t, err, "rollback must refuse to replace unknown costs with zero")
	_, err = conn.Exec(ctx, "ROLLBACK")
	require.NoError(t, err)
	var unknownCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM purchase_history WHERE upfront_cost IS NULL`).Scan(&unknownCount))
	assert.Equal(t, 5, unknownCount)
}
