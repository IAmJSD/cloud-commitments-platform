//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestLadderTimelineAmendmentRoutedPersistence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	store := config.NewPostgresStore(container.DB)
	account := &config.CloudAccount{Name: "Ladder fixture", Provider: "aws", ExternalID: "111111111111", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, account))
	cfg, err := store.UpsertLadderConfig(ctx, &config.LadderConfigDB{CloudAccountID: account.ID, Provider: "aws", Mode: "email_approval", Cadence: "daily", TargetCoverage: 80, BufferFraction: 0.1, BaselinePercentile: 5, LookbackDays: 30, BufferUtilizationThreshold: 50, MaxActionsPerRun: 5, RampSchedule: []byte(`{"steps":[{"after_days":0,"fraction":1}]}`)})
	require.NoError(t, err)
	run, err := store.SaveLadderRun(ctx, &config.LadderRunDB{ConfigID: &cfg.ID, Status: ladder.RunStatusPlanned, StartedAt: time.Now(), TotalHourlyCommit: 1})
	require.NoError(t, err)
	id := uuid.NewString()
	date := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	require.NoError(t, store.SaveLadderTranches(ctx, []config.LadderTrancheDB{{ID: id, ConfigID: &cfg.ID, RunID: &run.ID, LayerType: ladder.LayerComputeSP, Term: ladder.Term1Year, PaymentOption: ladder.PaymentNoUpfront, Status: ladder.TrancheStatusScheduled, AmountUSDHr: 1, ScheduledDate: date}}))
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.grantScoped(account.ID)
	router := NewRouter(&Handler{config: store, auth: mockAuth})
	read := ladderScopedReq("")
	read.QueryStringParameters = map[string]string{"account_id": account.ID, "provider": "aws"}
	initial, err := router.Route(ctx, "GET", "/api/ladder/tranches", read)
	require.NoError(t, err)
	require.Equal(t, "1.000000", initial.(*config.LadderTimelinePage).Events[0].AmountUSDHr)
	raw, err := json.Marshal(initial.(*config.LadderTimelinePage).Events[0])
	require.NoError(t, err)
	require.Contains(t, string(raw), `"revision":0`)
	body := fmt.Sprintf(`{"expected_revision":0,"scheduled_date":%q,"amount_usd_hr":"0.123456"}`, date.Add(time.Hour).Format(time.RFC3339))
	updated, err := router.Route(ctx, "PATCH", "/api/ladder/tranches/"+id, ladderScopedReq(body))
	require.NoError(t, err)
	require.Equal(t, int64(1), updated.(*config.LadderAmendmentResult).Revision)
	reloaded, err := router.Route(ctx, "GET", "/api/ladder/tranches", read)
	require.NoError(t, err)
	require.Equal(t, "0.123456", reloaded.(*config.LadderTimelinePage).Events[0].AmountUSDHr)
	require.Equal(t, date.Add(time.Hour), reloaded.(*config.LadderTimelinePage).Events[0].ScheduledDate.UTC())
	raw, err = json.Marshal(reloaded.(*config.LadderTimelinePage).Events[0])
	require.NoError(t, err)
	require.Contains(t, string(raw), `"revision":1`)
	_, err = router.Route(ctx, "PATCH", "/api/ladder/tranches/"+id, ladderScopedReq(body))
	clientErr, ok := IsClientError(err)
	require.True(t, ok)
	require.Equal(t, 409, clientErr.code)
	var actor string
	require.NoError(t, container.DB.Pool().QueryRow(ctx, "SELECT actor FROM ladder_tranche_amendments WHERE tranche_id=$1", id).Scan(&actor))
	require.Equal(t, scopedUserID, actor)
	read.QueryStringParameters["account_id"] = uuid.NewString()
	_, err = router.Route(ctx, "GET", "/api/ladder/tranches", read)
	require.Error(t, err)
}
