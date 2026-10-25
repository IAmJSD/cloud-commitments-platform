package server

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// fourEyesExchangeFixture is one underutilized convertible RI that the
// reshape analysis turns into a single exchange, wired to spies that record
// whether the exchange was executed and which records were saved.
type fourEyesExchangeFixture struct {
	store    *mockConfigStoreForExchange
	clients  riExchangeClients
	executed int
	listed   int
	saved    []*config.RIExchangeRecord
}

func newFourEyesExchangeFixture(globalCfg *config.GlobalConfig, globalCfgErr error) *fourEyesExchangeFixture {
	f := &fourEyesExchangeFixture{}
	f.store = &mockConfigStoreForExchange{
		globalConfig:    globalCfg,
		globalConfigErr: globalCfgErr,
		saveRIExchangeRecordFunc: func(_ context.Context, record *config.RIExchangeRecord) error {
			f.saved = append(f.saved, record)
			return nil
		},
		reserveRIExchangeFunc: func(_ context.Context, record *config.RIExchangeRecord, _, _ string) (string, error) {
			record.ID = "four-eyes-reservation"
			f.saved = append(f.saved, record)
			return "100.000000", nil
		},
	}
	quote := func() *exchange.ExchangeQuoteSummary {
		return &exchange.ExchangeQuoteSummary{
			IsValidExchange:  true,
			PaymentDueUSD:    new(big.Rat).SetFloat64(3.50),
			PaymentDueUSDStr: "3.50",
			CurrencyCode:     "USD",
		}
	}
	f.clients = riExchangeClients{
		listConvertibleRIs: func(context.Context) ([]ec2svc.ConvertibleRI, error) {
			f.listed++
			return []ec2svc.ConvertibleRI{{
				ReservedInstanceID:  "ri-underutilized",
				InstanceType:        "c5.xlarge",
				InstanceCount:       1,
				NormalizationFactor: 8.0,
				ProductDescription:  "Linux/UNIX",
				InstanceTenancy:     "default",
				Scope:               "Region",
				Duration:            31536000,
			}}, nil
		},
		getRIUtilization: func(context.Context, int) ([]recommendations.RIUtilization, error) {
			return []recommendations.RIUtilization{{ReservedInstanceID: "ri-underutilized", UtilizationPercent: 30.0}}, nil
		},
		exchangeClient: &mockExchangeClient{
			getQuoteFunc: func(context.Context, exchange.ExchangeQuoteRequest) (*exchange.ExchangeQuoteSummary, error) {
				return quote(), nil
			},
			executeFunc: func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
				f.executed++
				return "exch-unattended", quote(), nil
			},
		},
		lookupOffering: func(context.Context, exchange.LookupOfferingParams) (string, error) {
			return "offering-456", nil
		},
		accountID: "123456789012",
		region:    "us-east-1",
	}
	return f
}

func reshapeGlobalConfig(mode string, fourEyes bool) *config.GlobalConfig {
	return &config.GlobalConfig{
		RIExchangeEnabled:              true,
		RIExchangeMode:                 mode,
		RIExchangeUtilizationThreshold: 95.0,
		RIExchangeMaxPerExchangeUSD:    100.0,
		RIExchangeMaxDailyUSD:          500.0,
		RIExchangeLookbackDays:         30,
		RequireDifferentApprover:       fourEyes,
	}
}

// assertDowngradedToPending checks that a run raised one pending manual record
// and executed nothing.
func assertDowngradedToPending(t *testing.T, f *fourEyesExchangeFixture, result *exchange.AutoExchangeResult) {
	t.Helper()
	assert.Equal(t, 0, f.executed, "no exchange may execute without approval while 4-eyes is on")
	require.NotNil(t, result)
	assert.Equal(t, "manual", result.Mode)
	assert.Empty(t, result.Completed)
	require.Len(t, result.Pending, 1)
	require.Len(t, f.saved, 1)
	assert.Equal(t, "pending", f.saved[0].Status)
	assert.Equal(t, "manual", f.saved[0].Mode)
	assert.NotEmpty(t, f.saved[0].ApprovalToken, "the pending record must be approvable through the approval flow")
}

// TestExecuteRIExchangeReshape_FourEyesDowngradesAutoToManual pins issue #679
// (A): with RequireDifferentApprover on, the scheduled reshape must not run
// exchanges unattended, whatever non-manual mode is stored. It raises pending
// records and sends the approval email instead.
func TestExecuteRIExchangeReshape_FourEyesDowngradesAutoToManual(t *testing.T) {
	// "" and "Auto" are included because pkg/exchange treats every
	// non-"manual" value as auto.
	for _, mode := range []string{"auto", "", "Auto"} {
		t.Run("mode="+mode, func(t *testing.T) {
			ctx := testutil.TestContext(t)
			cfg := reshapeGlobalConfig(mode, true)
			f := newFourEyesExchangeFixture(cfg, nil)

			approvalSent, completedSent := false, false
			app := &Application{
				Config: f.store,
				Email: &mockEmailSender{
					sendApprovalFunc: func(context.Context, email.RIExchangeNotificationData) error {
						approvalSent = true
						return nil
					},
					sendCompletedFunc: func(context.Context, email.RIExchangeNotificationData) error {
						completedSent = true
						return nil
					},
				},
			}

			result, err := app.executeRIExchangeReshape(ctx, cfg, f.clients)
			require.NoError(t, err)
			assertDowngradedToPending(t, f, result)
			assert.True(t, approvalSent, "the downgraded run must send the pending-approval email")
			assert.False(t, completedSent)
		})
	}
}

// TestExecuteRIExchangeReshape_FourEyesOffAutoStillExecutes is the control:
// with 4-eyes off, auto mode keeps executing exchanges.
func TestExecuteRIExchangeReshape_FourEyesOffAutoStillExecutes(t *testing.T) {
	ctx := testutil.TestContext(t)
	cfg := reshapeGlobalConfig("auto", false)
	f := newFourEyesExchangeFixture(cfg, nil)
	app := &Application{Config: f.store}

	result, err := app.executeRIExchangeReshape(ctx, cfg, f.clients)
	require.NoError(t, err)
	assert.Equal(t, 1, f.executed)
	assert.Equal(t, "auto", result.Mode)
	assert.Len(t, result.Completed, 1)
	assert.Empty(t, result.Pending)
}

// TestExchangeRunnerAdapter_FourEyesDowngradesLadderReshape: the ladder
// always asks for auto mode, and under 4-eyes it must run as manual too.
func TestExchangeRunnerAdapter_FourEyesDowngradesLadderReshape(t *testing.T) {
	ctx := testutil.TestContext(t)
	f := newFourEyesExchangeFixture(&config.GlobalConfig{RequireDifferentApprover: true}, nil)
	adapter := &exchangeRunnerAdapter{
		app:       &Application{Config: f.store},
		region:    "us-east-1",
		accountID: "123456789012",
	}
	ladderRunID := "ladder-run-1"

	result, err := adapter.run(ctx, ladderExchangeConfig(), f.clients, &ladderRunID, false)
	require.NoError(t, err)
	assertDowngradedToPending(t, f, result)
	require.NotNil(t, f.saved[0].LadderRunID)
	assert.Equal(t, ladderRunID, *f.saved[0].LadderRunID)
}

// TestExchangeRunnerAdapter_FourEyesOffLadderStillExecutes is the control for
// the ladder path.
func TestExchangeRunnerAdapter_FourEyesOffLadderStillExecutes(t *testing.T) {
	ctx := testutil.TestContext(t)
	f := newFourEyesExchangeFixture(&config.GlobalConfig{}, nil)
	adapter := &exchangeRunnerAdapter{
		app:       &Application{Config: f.store},
		region:    "us-east-1",
		accountID: "123456789012",
	}
	ladderRunID := "ladder-run-1"

	result, err := adapter.run(ctx, ladderExchangeConfig(), f.clients, &ladderRunID, false)
	require.NoError(t, err)
	assert.Equal(t, 1, f.executed)
	assert.Equal(t, "auto", result.Mode)
	assert.Len(t, result.Completed, 1)
}

// TestExchangeRunnerAdapter_PolicyLookupErrorFailsClosed: an unreadable
// global config must stop the ladder reshape before any AWS call, never be
// treated as "4-eyes off".
func TestExchangeRunnerAdapter_PolicyLookupErrorFailsClosed(t *testing.T) {
	ctx := testutil.TestContext(t)
	f := newFourEyesExchangeFixture(nil, errors.New("db down"))
	adapter := &exchangeRunnerAdapter{
		app:       &Application{Config: f.store},
		region:    "us-east-1",
		accountID: "123456789012",
	}

	result, err := adapter.run(ctx, ladderExchangeConfig(), f.clients, nil, false)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "4-eyes policy check")
	assert.Equal(t, 0, f.listed, "no AWS call may happen when the policy cannot be read")
	assert.Equal(t, 0, f.executed)
	assert.Empty(t, f.saved)
}

func ladderExchangeConfig() exchange.RIExchangeConfig {
	return exchange.RIExchangeConfig{
		Mode:                     string(exchange.ExchangeModeAuto),
		UtilizationThreshold:     95.0,
		MaxPaymentPerExchangeUSD: 100.0,
		MaxPaymentDailyUSD:       500.0,
		LookbackDays:             30,
	}
}

func TestFourEyesExchangeMode(t *testing.T) {
	on := &config.GlobalConfig{RequireDifferentApprover: true}
	off := &config.GlobalConfig{}
	tests := []struct {
		name string
		cfg  *config.GlobalConfig
		mode string
		want string
	}{
		{"4-eyes on downgrades auto", on, "auto", "manual"},
		{"4-eyes on downgrades an empty mode", on, "", "manual"},
		{"4-eyes on downgrades an unknown mode", on, "Auto", "manual"},
		{"4-eyes on keeps manual", on, "manual", "manual"},
		{"4-eyes off keeps auto", off, "auto", "auto"},
		{"4-eyes off keeps manual", off, "manual", "manual"},
		{"nil config fails closed", nil, "auto", "manual"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, fourEyesExchangeMode(tt.cfg, tt.mode))
		})
	}
}
