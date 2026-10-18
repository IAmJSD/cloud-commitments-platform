//go:build integration

package config

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestLadderTimelineScopedPagesAndSnapshots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfig(ctx, t, store)
	var accountID string
	require.NoError(t, store.db.QueryRow(ctx, "SELECT cloud_account_id FROM ladder_configs WHERE id=$1", cfgID).Scan(&accountID))
	run, err := store.SaveLadderRun(ctx, &LadderRunDB{
		ConfigID: &cfgID, Status: ladder.RunStatusPlanned, StartedAt: time.Now().UTC(),
		ApprovalTokenHash: stringPtr("never-serialize-this"),
		Plan:              []byte(`{"secret":"never-serialize-this","actions":[{"action":"Hold","layer":"compute-sp","rationale":"Baseline already covered","data_sources":["aws-ce"],"secret":"never-serialize-this"}]}`),
	})
	require.NoError(t, err)
	tranches := make([]LadderTrancheDB, LadderTimelinePageSize+1)
	for i := range tranches {
		tranches[i] = LadderTrancheDB{
			ID: uuid.NewString(), ConfigID: &cfgID, RunID: &run.ID,
			LayerType: ladder.LayerComputeSP, Term: ladder.Term1Year, PaymentOption: ladder.PaymentNoUpfront,
			Status: ladder.TrancheStatusScheduled, AmountUSDHr: 0.123456, ScheduledDate: time.Now().UTC().Add(time.Hour),
		}
	}
	require.NoError(t, store.SaveLadderTranches(ctx, tranches))
	first, err := store.ListLadderTimeline(ctx, accountID, "aws", nil)
	require.NoError(t, err)
	require.Len(t, first.Events, 100)
	require.Equal(t, int64(101), first.TotalCount)
	require.Equal(t, "12.469056", first.TotalUSDHr)
	require.NotNil(t, first.NextCursor)
	second, err := store.ListLadderTimeline(ctx, accountID, "aws", first.NextCursor)
	require.NoError(t, err)
	require.Len(t, second.Events, 1)
	require.Nil(t, second.NextCursor)
	ids := make(map[string]bool)
	for _, event := range append(first.Events, second.Events...) {
		require.False(t, ids[event.ID], "cursor must never duplicate an event")
		ids[event.ID] = true
		require.Zero(t, event.Revision, "untouched tranches report revision 0")
	}
	require.Len(t, ids, 101)
	_, err = store.db.Exec(ctx, `UPDATE ladder_tranches SET revision = 7 WHERE id = $1`, first.Events[0].ID)
	require.NoError(t, err)
	revised, err := store.ListLadderTimeline(ctx, accountID, "aws", nil)
	require.NoError(t, err)
	require.Equal(t, int64(7), revised.Events[0].Revision, "revision must project from the tranche row")
	for _, provider := range []string{"azure", "gcp"} {
		page, queryErr := store.ListLadderTimeline(ctx, accountID, provider, nil)
		require.NoError(t, queryErr)
		require.Empty(t, page.Events)
		require.Zero(t, page.TotalCount)
	}
	missing, err := store.ListLadderTimeline(ctx, uuid.NewString(), "aws", nil)
	require.NoError(t, err)
	require.Empty(t, missing.Events)
	require.Equal(t, "0.000000", missing.TotalUSDHr)
	runs, err := store.ListLadderTimelineRuns(ctx, accountID, "aws", nil)
	require.NoError(t, err)
	require.Len(t, runs.Runs, 1)
	require.Nil(t, runs.Runs[0].BaselineUSDHr)
	require.Equal(t, "Baseline already covered", runs.Runs[0].Actions[0].Rationale)
	raw, err := json.Marshal(runs)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "never-serialize-this")
	require.Contains(t, string(raw), `"baseline_usd_hr":null`)
	_, err = store.db.Exec(ctx, `INSERT INTO ladder_runs(config_id,status,created_at)
 SELECT $1,'planned',$2 FROM generate_series(1,100)`, cfgID, runs.Runs[0].CreatedAt)
	require.NoError(t, err)
	runs, err = store.ListLadderTimelineRuns(ctx, accountID, "aws", nil)
	require.NoError(t, err)
	require.Equal(t, int64(101), runs.TotalCount)
	require.Len(t, runs.Runs, 100)
	lastRunPage, err := store.ListLadderTimelineRuns(ctx, accountID, "aws", runs.NextCursor)
	require.NoError(t, err)
	require.Len(t, lastRunPage.Runs, 1)
	for _, item := range runs.Runs {
		require.NotEqual(t, lastRunPage.Runs[0].ID, item.ID)
	}
	otherConfig := seedLadderConfigWithExtID(ctx, t, store, "222222222222")
	otherRun, err := store.SaveLadderRun(ctx, &LadderRunDB{ConfigID: &otherConfig, Status: ladder.RunStatusPlanned, StartedAt: time.Now().UTC(), Plan: []byte(`{}`)})
	require.NoError(t, err)
	_, err = store.db.Exec(ctx, `INSERT INTO ladder_tranches(config_id,run_id,layer_type,amount_usd_hr,term,payment_option,scheduled_date,status)
 VALUES ($1,$2,'compute-sp',99,'1yr','no-upfront',NOW(),'scheduled'),
 (NULL,$2,'compute-sp',99,'1yr','no-upfront',NOW(),'scheduled'),
	 ($3,NULL,'compute-sp',99,'1yr','no-upfront',NOW(),'scheduled'),
	 ($3,$4,'compute-sp',99,'1yr','no-upfront',NOW(),'scheduled')`, otherConfig, run.ID, cfgID, otherRun.ID)
	require.NoError(t, err)
	unchanged, err := store.ListLadderTimeline(ctx, accountID, "aws", nil)
	require.NoError(t, err)
	require.Equal(t, int64(101), unchanged.TotalCount)
	require.Equal(t, "12.469056", unchanged.TotalUSDHr)
}

func TestLadderTimelineMixedBasisOnDemand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := setupLadderStore(ctx, t)
	cfgID := seedLadderConfig(ctx, t, store)
	var accountID string
	require.NoError(t, store.db.QueryRow(ctx, "SELECT cloud_account_id FROM ladder_configs WHERE id=$1", cfgID).Scan(&accountID))
	run, err := store.SaveLadderRun(ctx, &LadderRunDB{ConfigID: &cfgID, Status: ladder.RunStatusPlanned, StartedAt: time.Now().UTC(), Plan: []byte(`{}`)})
	require.NoError(t, err)
	// No writer produces commitment rows yet, so seed them directly.
	_, err = store.db.Exec(ctx, `INSERT INTO ladder_tranches(config_id,run_id,layer_type,amount_usd_hr,amount_basis,commitment_rate,term,payment_option,scheduled_date,status,created_at)
 VALUES ($1,$2,'compute-sp',5,'commitment','1/2','1yr','no-upfront',NOW(),'scheduled',NOW()+interval '1 second'),
 ($1,$2,'compute-sp',1,'commitment','1/3','1yr','no-upfront',NOW(),'scheduled',NOW()+interval '2 second'),
 ($1,$2,'compute-sp',2.5,'on_demand_equivalent',NULL,'1yr','no-upfront',NOW(),'scheduled',NOW()+interval '3 second'),
 ($1,$2,'compute-sp',4,NULL,NULL,'1yr','no-upfront',NOW(),'scheduled',NOW()+interval '4 second'),
 ($1,$2,'compute-sp',1.000001,'commitment','2/3','1yr','no-upfront',NOW(),'scheduled',NOW()+interval '5 second'),
 ($1,$2,'compute-sp',1.000001,'commitment','2/3','1yr','no-upfront',NOW(),'scheduled',NOW()+interval '6 second')`, cfgID, run.ID)
	require.NoError(t, err)
	page, err := store.ListLadderTimeline(ctx, accountID, "aws", nil)
	require.NoError(t, err)
	require.Equal(t, "14.500002", page.TotalUSDHr)
	require.Equal(t, "22.500004", page.TotalODUSDHr)
	od, native := make([]string, 0, len(page.Events)), make([]string, 0, len(page.Events))
	for _, e := range page.Events {
		od, native = append(od, e.ODUSDHr), append(native, e.AmountUSDHr)
	}
	require.Equal(t, []string{"10.000000", "3.000000", "2.500000", "4.000000", "1.500002", "1.500002"}, od)
	require.Equal(t, []string{"5.000000", "1.000000", "2.500000", "4.000000", "1.000001", "1.000001"}, native)
	require.Nil(t, page.Events[3].AmountBasis, "NULL basis stays unknown on the wire")

	_, err = store.db.Exec(ctx, `UPDATE ladder_tranches SET commitment_rate='abc' WHERE id=$1`, page.Events[0].ID)
	require.NoError(t, err)
	_, err = store.ListLadderTimeline(ctx, accountID, "aws", nil)
	require.ErrorIs(t, err, ErrLadderTimelineRate)
}
