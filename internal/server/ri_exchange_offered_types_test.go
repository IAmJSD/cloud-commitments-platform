package server

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// noOfferedCatalog is an offeredInstanceTypes source with no catalog, so the
// reshape keeps the legacy size ladder (after a warning).
func noOfferedCatalog(context.Context) ([]string, error) { return nil, nil }

const (
	catalogTestRegion = "eu-west-3"
	catalogTestRIID   = "ri-catalog"
)

// catalogEC2 serves one convertible 1x m5.4xlarge RI (32 normalized units)
// and a configurable instance type catalog, and records offering lookups.
type catalogEC2 struct {
	offered      []string
	offeredErr   error
	offeredCalls int
	lookups      []string
}

func (f *catalogEC2) ListConvertibleReservedInstances(context.Context) ([]ec2svc.ConvertibleRI, error) {
	return []ec2svc.ConvertibleRI{{
		ReservedInstanceID: catalogTestRIID,
		InstanceType:       "m5.4xlarge",
		InstanceCount:      1,
		ProductDescription: "Linux/UNIX",
		InstanceTenancy:    "default",
		Scope:              "Region",
		Duration:           31536000,
		CurrencyCode:       "USD",
	}}, nil
}

func (f *catalogEC2) GetValidResourceTypes(context.Context) ([]string, error) {
	f.offeredCalls++
	return f.offered, f.offeredErr
}

func (f *catalogEC2) FindConvertibleOffering(_ context.Context, p ec2svc.FindConvertibleOfferingParams) (string, error) {
	f.lookups = append(f.lookups, p.InstanceType)
	return "offering-1", nil
}

// catalogRecs reports the RI at 50% utilization (16 of 32 units used): the
// legacy ladder reshapes it to 1x m5.2xlarge.
type catalogRecs struct{ regions []string }

func (r *catalogRecs) GetRIUtilization(_ context.Context, _ int, region string) ([]recommendations.RIUtilization, error) {
	r.regions = append(r.regions, region)
	return []recommendations.RIUtilization{{ReservedInstanceID: catalogTestRIID, UtilizationPercent: 50}}, nil
}

func validQuoteClient() *mockExchangeClient {
	return &mockExchangeClient{
		getQuoteFunc: func(context.Context, exchange.ExchangeQuoteRequest) (*exchange.ExchangeQuoteSummary, error) {
			return &exchange.ExchangeQuoteSummary{
				IsValidExchange:  true,
				PaymentDueUSD:    new(big.Rat).SetFloat64(5),
				PaymentDueUSDStr: "5.00",
				CurrencyCode:     "USD",
			}, nil
		},
	}
}

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureWarnLogs routes pkg/logging into a buffer at warn level for the rest
// of the test. Tests using it must not call t.Parallel.
func captureWarnLogs(t *testing.T) *lockedLog {
	t.Helper()
	out := &lockedLog{}
	prevOut := logging.SetOutput(out)
	prevLevel := logging.GetLevel()
	logging.SetLevelValue(logging.LevelWarn)
	t.Cleanup(func() {
		logging.SetOutput(prevOut)
		logging.SetLevelValue(prevLevel)
	})
	return out
}

func TestNewRIExchangeClients_RoutesToClients(t *testing.T) {
	ctx := context.Background()
	ec2 := &catalogEC2{offered: []string{"m5.large"}}
	recs := &catalogRecs{}
	exch := validQuoteClient()
	c := newRIExchangeClients(ec2, recs, exch, catalogTestRegion, "acct-1")

	offered, err := c.offeredInstanceTypes(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"m5.large"}, offered)
	assert.Equal(t, 1, ec2.offeredCalls)

	ris, err := c.listConvertibleRIs(ctx)
	require.NoError(t, err)
	require.Len(t, ris, 1)

	_, err = c.getRIUtilization(ctx, 30)
	require.NoError(t, err)
	assert.Equal(t, []string{catalogTestRegion}, recs.regions)

	_, err = c.lookupOffering(ctx, exchange.LookupOfferingParams{InstanceType: "m5.xlarge"})
	require.NoError(t, err)
	assert.Equal(t, []string{"m5.xlarge"}, ec2.lookups)

	assert.Same(t, exch, c.exchangeClient)
	assert.Equal(t, catalogTestRegion, c.region)
	assert.Equal(t, "acct-1", c.accountID)
}

// reshapeEntryPoints are the two scheduled paths that run RunAutoExchange:
// the standalone RI-exchange task and the ladder's exchange runner.
var reshapeEntryPoints = []struct {
	name string
	run  func(t *testing.T, clients riExchangeClients) *exchange.AutoExchangeResult
}{
	{
		name: "standalone",
		run: func(t *testing.T, clients riExchangeClients) *exchange.AutoExchangeResult {
			app := &Application{Config: &mockConfigStoreForExchange{}, Email: &mockEmailSender{}}
			cfg := &config.GlobalConfig{
				RIExchangeEnabled:              true,
				RIExchangeMode:                 "manual",
				RIExchangeUtilizationThreshold: 95,
				RIExchangeMaxPerExchangeUSD:    100,
				RIExchangeMaxDailyUSD:          500,
				RIExchangeLookbackDays:         30,
			}
			result, err := app.executeRIExchangeReshape(context.Background(), cfg, clients)
			require.NoError(t, err)
			return result
		},
	},
	{
		name: "ladder",
		run: func(t *testing.T, clients riExchangeClients) *exchange.AutoExchangeResult {
			a := &exchangeRunnerAdapter{app: &Application{Config: &mockConfigStoreForExchange{}}}
			cfg := exchange.RIExchangeConfig{
				Mode:                     "manual",
				UtilizationThreshold:     95,
				MaxPaymentPerExchangeUSD: 100,
				MaxPaymentDailyUSD:       500,
				LookbackDays:             30,
			}
			ladderRunID := "ladder-run-1"
			result, err := a.runWithClients(context.Background(), cfg, &ladderRunID, true, clients)
			require.NoError(t, err)
			return result
		},
	},
}

func TestRIExchangeReshape_OfferedCatalog(t *testing.T) {
	cases := []struct {
		name        string
		ec2         func() *catalogEC2
		wantLookups []string
		wantTarget  string
		wantCount   int32
		wantSkip    string
		wantWarning string
	}{
		{
			name:        "catalog limits the target to offered sizes",
			ec2:         func() *catalogEC2 { return &catalogEC2{offered: []string{"m5.4xlarge", "m5.xlarge", "c5.large"}} },
			wantLookups: []string{"m5.xlarge"},
			wantTarget:  "m5.xlarge",
			wantCount:   2,
		},
		{
			name:     "catalog without the family skips with a reason",
			ec2:      func() *catalogEC2 { return &catalogEC2{offered: []string{"c5.large", "c5.xlarge"}} },
			wantSkip: "instance family not offered in region (m5)",
		},
		{
			name:        "catalog error falls back to the legacy ladder",
			ec2:         func() *catalogEC2 { return &catalogEC2{offeredErr: errors.New("UnauthorizedOperation")} },
			wantLookups: []string{"m5.2xlarge"},
			wantTarget:  "m5.2xlarge",
			wantCount:   1,
			wantWarning: "failed, proceeding without the catalog: UnauthorizedOperation",
		},
		{
			name:        "empty catalog falls back to the legacy ladder",
			ec2:         func() *catalogEC2 { return &catalogEC2{offered: []string{}} },
			wantLookups: []string{"m5.2xlarge"},
			wantTarget:  "m5.2xlarge",
			wantCount:   1,
			wantWarning: "came back empty, proceeding without the catalog",
		},
	}
	for _, ep := range reshapeEntryPoints {
		for _, tc := range cases {
			t.Run(ep.name+"/"+tc.name, func(t *testing.T) {
				logs := captureWarnLogs(t)
				ec2 := tc.ec2()
				clients := newRIExchangeClients(ec2, &catalogRecs{}, validQuoteClient(), catalogTestRegion, "acct-1")

				result := ep.run(t, clients)

				assert.Equal(t, 1, ec2.offeredCalls, "the run must query the region's instance type catalog once")
				assert.Equal(t, tc.wantLookups, ec2.lookups, "offering lookups")
				outcomes := append(append([]exchange.ExchangeOutcome{}, result.Pending...), result.Completed...)
				if tc.wantSkip != "" {
					assert.Empty(t, outcomes)
					require.Len(t, result.Skipped, 1)
					assert.Contains(t, result.Skipped[0].Reason, tc.wantSkip)
				} else {
					assert.Empty(t, result.Skipped)
					require.Len(t, outcomes, 1)
					assert.Equal(t, tc.wantTarget, outcomes[0].TargetInstanceType)
					assert.Equal(t, tc.wantCount, outcomes[0].TargetCount)
				}
				if tc.wantWarning != "" {
					assert.Contains(t, logs.String(), `region "`+catalogTestRegion+`"`)
					assert.Contains(t, logs.String(), tc.wantWarning)
				} else {
					assert.NotContains(t, logs.String(), "proceeding without the catalog")
				}
			})
		}
	}
}
