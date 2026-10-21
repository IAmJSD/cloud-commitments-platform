//go:build integration

package server

import (
	"context"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/stretchr/testify/require"
)

func TestRIExchangeAutoWorkers_ConcreteStoreAndFixtureClient(t *testing.T) {
	// A dedicated container, not an opt-in DSN, so this runs in CI (#724). It
	// skips only when Docker is unavailable; a container or migration failure
	// fails the test (#1597).
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer setupCancel()
	container := testhelpers.RequirePostgresContainer(setupCtx, t)
	t.Cleanup(func() { _ = container.Cleanup(context.Background()) })
	conn := container.DB
	require.NoError(t, migrations.RunMigrations(setupCtx, conn.Pool(), "../database/postgres/migrations", "", ""),
		"migrations failed to apply to a fresh database")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var testRunID string
	require.NoError(t, conn.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&testRunID))
	standaloneRIID, ladderRIID := "ri-standalone-"+testRunID, "ri-ladder-"+testRunID
	store := config.NewPostgresStore(conn)
	baselineText, err := store.GetRIExchangeDailySpend(ctx, time.Now().UTC())
	require.NoError(t, err)
	baseline, err := strconv.ParseFloat(baselineText, 64)
	require.NoError(t, err)

	quoteClient := func(riID string, executions *int) *mockExchangeClient {
		return &mockExchangeClient{
			getQuoteFunc: func(context.Context, exchange.ExchangeQuoteRequest) (*exchange.ExchangeQuoteSummary, error) {
				return &exchange.ExchangeQuoteSummary{IsValidExchange: true, PaymentDueUSD: big.NewRat(900, 1), PaymentDueUSDStr: "900.000000", CurrencyCode: "USD"}, nil
			},
			executeFunc: func(executeCtx context.Context, req exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
				require.Equal(t, riID, req.ReservedIDs[0])
				require.Equal(t, "1000.000000", req.MaxPaymentDueUSD.FloatString(6))
				var reservationID, status, payment string
				require.NoError(t, conn.QueryRow(executeCtx,
					"SELECT id::text, status, payment_due::text FROM ri_exchange_history WHERE $1 = ANY(source_ri_ids)",
					riID).Scan(&reservationID, &status, &payment))
				require.NotEmpty(t, reservationID)
				require.Equal(t, "processing", status)
				require.Equal(t, "1000.000000", payment)
				*executions++
				return "accepted-" + riID, &exchange.ExchangeQuoteSummary{IsValidExchange: true, PaymentDueUSD: big.NewRat(950, 1), PaymentDueUSDStr: "950.000000", CurrencyCode: "USD"}, nil
			},
		}
	}
	ri := func(id string) []ec2svc.ConvertibleRI {
		return []ec2svc.ConvertibleRI{{ReservedInstanceID: id, InstanceType: "c5.xlarge", InstanceCount: 1, NormalizationFactor: 8,
			ProductDescription: "Linux/UNIX", InstanceTenancy: "default", Scope: "Region", Duration: 31536000, FixedPrice: 876}}
	}
	util := func(id string) []recommendations.RIUtilization {
		return []recommendations.RIUtilization{{ReservedInstanceID: id, UtilizationPercent: 30}}
	}

	standaloneExecutions := 0
	app := &Application{Config: store, Email: &mockEmailSender{sendCompletedFunc: func(context.Context, email.RIExchangeNotificationData) error { return nil }}}
	standaloneCfg := &config.GlobalConfig{RIExchangeEnabled: true, RIExchangeMode: "auto", RIExchangeUtilizationThreshold: 95,
		RIExchangeMaxPerExchangeUSD: 1000, RIExchangeMaxDailyUSD: baseline + 1000, RIExchangeLookbackDays: 30}
	standalone, err := app.executeRIExchangeReshape(ctx, standaloneCfg, riExchangeClients{
		listConvertibleRIs: func(context.Context) ([]ec2svc.ConvertibleRI, error) { return ri(standaloneRIID), nil },
		getRIUtilization:   func(context.Context, int) ([]recommendations.RIUtilization, error) { return util(standaloneRIID), nil },
		exchangeClient:     quoteClient(standaloneRIID, &standaloneExecutions),
		lookupOffering: func(context.Context, exchange.LookupOfferingParams) (string, error) {
			return "offering-test", nil
		},
		accountID: "555555555555", region: "us-east-1",
	})
	require.NoError(t, err)
	require.Equal(t, 1, standaloneExecutions)
	require.Len(t, standalone.Completed, 1)
	require.NotEmpty(t, standalone.Completed[0].RecordID)
	row, err := store.GetRIExchangeRecord(ctx, standalone.Completed[0].RecordID)
	require.NoError(t, err)
	require.Equal(t, "completed", row.Status)
	require.Equal(t, "950.000000", row.PaymentDue)

	var ladderRunID string
	require.NoError(t, conn.QueryRow(ctx, "INSERT INTO ladder_runs (status) VALUES ('executing') RETURNING id::text").Scan(&ladderRunID))
	ladderExecutions := 0
	riInfos, utilInfos, metadata := convertForAutoExchange(ri(ladderRIID), util(ladderRIID))
	ladder, err := exchange.RunAutoExchange(ctx, exchange.RunAutoExchangeParams{
		Store: newConfigExchangeStoreAdapter(store), ExchangeClient: quoteClient(ladderRIID, &ladderExecutions),
		LookupOffering: func(context.Context, exchange.LookupOfferingParams) (string, error) {
			return "offering-test", nil
		},
		RIs: riInfos, Utilization: utilInfos, RIMetadata: metadata,
		Config:    exchange.RIExchangeConfig{Mode: "auto", UtilizationThreshold: 95, MaxPaymentPerExchangeUSD: 1000, MaxPaymentDailyUSD: baseline + 2000},
		AccountID: "666666666666", Region: "eu-west-1", LadderRunID: &ladderRunID,
	})
	require.NoError(t, err)
	require.Equal(t, 1, ladderExecutions)
	require.Len(t, ladder.Completed, 1)
	require.NotEmpty(t, ladder.Completed[0].RecordID)
	row, err = store.GetRIExchangeRecord(ctx, ladder.Completed[0].RecordID)
	require.NoError(t, err)
	require.Equal(t, "completed", row.Status)
	require.Equal(t, "950.000000", row.PaymentDue)
	require.NotNil(t, row.LadderRunID)
	require.Equal(t, ladderRunID, *row.LadderRunID)
}
