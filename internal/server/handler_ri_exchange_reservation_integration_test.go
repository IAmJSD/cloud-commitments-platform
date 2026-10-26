//go:build integration

package server

import (
	"context"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/stretchr/testify/require"
)

func TestRIExchangeAutoWorkers_ConcreteStoreAndFixtureClient(t *testing.T) {
	// A fresh migrated database on the package's shared container, not an
	// opt-in DSN, so this runs in CI (#724). It skips only when Docker is
	// unavailable; a container or migration failure fails the test (#1597).
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer setupCancel()
	conn := testhelpers.RequireMigratedDatabase(setupCtx, t, "../database/postgres/migrations").DB
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var testRunID string
	require.NoError(t, conn.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&testRunID))
	standaloneRIID, ladderRIID := "ri-standalone-"+testRunID, "ri-ladder-"+testRunID
	store := config.NewPostgresStore(conn)
	// Each run's daily cap is distinct from the 1000 USD per-exchange cap so
	// only one of them can bind (#666). The standalone run leaves 1500+ USD of
	// daily headroom, so the per-exchange cap must set its 1000 USD ceiling;
	// the ladder run leaves under 1000 USD, so the daily cap must set it.
	// Caps are today's spend rounded up to whole dollars plus a whole-dollar
	// margin: a float64 holding an integer converts to the six-decimal cap
	// string exactly, so a fractional-micro spend can't floor a cap a micro
	// below its intended value.
	todaySpendUSD := func() (exact *big.Rat, wholeDollars float64) {
		spendText, spendErr := store.GetRIExchangeDailySpend(ctx, time.Now().UTC())
		require.NoError(t, spendErr)
		spend, ok := new(big.Rat).SetString(spendText)
		require.True(t, ok, "daily spend %q is not a decimal", spendText)
		approx, _ := spend.Float64()
		return spend, math.Ceil(approx)
	}
	baselineSpend, baselineWholeDollars := todaySpendUSD()
	wantCeiling := map[string]string{standaloneRIID: "1000.000000"}

	quoteClient := func(riID string, executions *int) *mockExchangeClient {
		return &mockExchangeClient{
			getQuoteFunc: func(context.Context, exchange.ExchangeQuoteRequest) (*exchange.ExchangeQuoteSummary, error) {
				return &exchange.ExchangeQuoteSummary{IsValidExchange: true, PaymentDueUSD: big.NewRat(900, 1), PaymentDueUSDStr: "900.000000", CurrencyCode: "USD"}, nil
			},
			executeFunc: func(executeCtx context.Context, req exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
				require.Equal(t, riID, req.ReservedIDs[0])
				require.Equal(t, wantCeiling[riID], req.MaxPaymentDueUSD.FloatString(6))
				var reservationID, status, payment string
				require.NoError(t, conn.QueryRow(executeCtx,
					"SELECT id::text, status, payment_due::text FROM ri_exchange_history WHERE $1 = ANY(source_ri_ids)",
					riID).Scan(&reservationID, &status, &payment))
				require.NotEmpty(t, reservationID)
				require.Equal(t, "processing", status)
				require.Equal(t, wantCeiling[riID], payment)
				*executions++
				return "accepted-" + riID, &exchange.ExchangeQuoteSummary{IsValidExchange: true, PaymentDueUSD: big.NewRat(950, 1), PaymentDueUSDStr: "950.000000", CurrencyCode: "USD"}, nil
			},
		}
	}
	ri := func(id string) []ec2svc.ConvertibleRI {
		return []ec2svc.ConvertibleRI{{ReservedInstanceID: id, InstanceType: "c5.xlarge", InstanceCount: 1, NormalizationFactor: 8,
			ProductDescription: "Linux/UNIX", InstanceTenancy: "default", Scope: "Region", Duration: 31536000}}
	}
	util := func(id string) []recommendations.RIUtilization {
		return []recommendations.RIUtilization{{ReservedInstanceID: id, UtilizationPercent: 30}}
	}

	standaloneExecutions := 0
	app := &Application{Config: store, Email: &mockEmailSender{sendCompletedFunc: func(context.Context, email.RIExchangeNotificationData) error { return nil }}}
	standaloneCfg := &config.GlobalConfig{RIExchangeEnabled: true, RIExchangeMode: "auto", RIExchangeUtilizationThreshold: 95,
		RIExchangeMaxPerExchangeUSD: 1000, RIExchangeMaxDailyUSD: baselineWholeDollars + 1500, RIExchangeLookbackDays: 30}
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

	// Today's spend now includes the standalone exchange's 950 USD, so the
	// ladder's ceiling is its daily cap minus that spend, between 980 and 981
	// USD: below the per-exchange cap and above the 900 USD initial quote.
	ladderSpend, ladderSpendWholeDollars := todaySpendUSD()
	require.Equal(t, "950.000000", new(big.Rat).Sub(ladderSpend, baselineSpend).FloatString(6))
	ladderDailyCapUSD := ladderSpendWholeDollars + 980
	wantCeiling[ladderRIID] = new(big.Rat).Sub(new(big.Rat).SetFloat64(ladderDailyCapUSD), ladderSpend).FloatString(6)
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
		Config:    exchange.RIExchangeConfig{Mode: "auto", UtilizationThreshold: 95, MaxPaymentPerExchangeUSD: 1000, MaxPaymentDailyUSD: ladderDailyCapUSD},
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
