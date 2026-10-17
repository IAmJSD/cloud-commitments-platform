package server

import (
	"context"
	"math/big"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #921: the scheduled RI exchange forwards the source RI's AZ through
// RunAutoExchange to the provider's offering lookup. These tests run the real
// ec2svc.Client over a fake EC2 API: no AWS call, no exchange accept.

const zoneTestRIID = "ri-zone-test"

// zoneRecordingEC2API serves one convertible RI and records the AZ of every
// offering lookup. The embedded nil API makes any other call (including the
// exchange accept) panic.
type zoneRecordingEC2API struct {
	ec2svc.API
	ri          ec2types.ReservedInstances
	lookupZones []*string
}

func (a *zoneRecordingEC2API) DescribeReservedInstances(context.Context, *awsec2sdk.DescribeReservedInstancesInput, ...func(*awsec2sdk.Options)) (*awsec2sdk.DescribeReservedInstancesOutput, error) {
	return &awsec2sdk.DescribeReservedInstancesOutput{ReservedInstances: []ec2types.ReservedInstances{a.ri}}, nil
}

func (a *zoneRecordingEC2API) DescribeReservedInstancesOfferings(_ context.Context, in *awsec2sdk.DescribeReservedInstancesOfferingsInput, _ ...func(*awsec2sdk.Options)) (*awsec2sdk.DescribeReservedInstancesOfferingsOutput, error) {
	a.lookupZones = append(a.lookupZones, in.AvailabilityZone)
	return &awsec2sdk.DescribeReservedInstancesOfferingsOutput{
		ReservedInstancesOfferings: []ec2types.ReservedInstancesOffering{{
			ReservedInstancesOfferingId: aws.String("offering-in-zone"),
			AvailabilityZone:            in.AvailabilityZone,
			OfferingClass:               ec2types.OfferingClassTypeConvertible,
			InstanceTenancy:             ec2types.TenancyDefault,
			Duration:                    aws.Int64(31536000),
		}},
	}, nil
}

func zoneTestRI(scope ec2types.Scope, az string) ec2types.ReservedInstances {
	ri := ec2types.ReservedInstances{
		ReservedInstancesId: aws.String(zoneTestRIID),
		InstanceType:        ec2types.InstanceType("m5.large"),
		InstanceCount:       aws.Int32(2),
		ProductDescription:  ec2types.RIProductDescription("Linux/UNIX"),
		InstanceTenancy:     ec2types.TenancyDefault,
		Scope:               scope,
		Duration:            aws.Int64(31536000),
		OfferingClass:       ec2types.OfferingClassTypeConvertible,
		CurrencyCode:        ec2types.CurrencyCodeValuesUsd,
	}
	if az != "" {
		ri.AvailabilityZone = aws.String(az)
	}
	return ri
}

// runZoneReshape runs a manual-mode reshape for one underutilized RI and
// returns the result, the fake API and whether a quote was requested.
func runZoneReshape(t *testing.T, ri ec2types.ReservedInstances) (*exchange.AutoExchangeResult, *zoneRecordingEC2API, bool) {
	t.Helper()
	api := &zoneRecordingEC2API{ri: ri}
	c := ec2svc.NewClient(aws.Config{Region: "us-east-1"})
	c.SetEC2API(api)

	quoted := false
	store := &mockConfigStoreForExchange{
		saveRIExchangeRecordFunc: func(context.Context, *config.RIExchangeRecord) error { return nil },
	}
	app := &Application{Config: store, Email: &mockEmailSender{}}
	cfg := &config.GlobalConfig{
		RIExchangeEnabled:              true,
		RIExchangeMode:                 "manual",
		RIExchangeUtilizationThreshold: 95.0,
		RIExchangeMaxPerExchangeUSD:    100.0,
		RIExchangeMaxDailyUSD:          500.0,
		RIExchangeLookbackDays:         30,
	}
	clients := riExchangeClients{
		listConvertibleRIs: c.ListConvertibleReservedInstances,
		getRIUtilization: func(context.Context, int) ([]recommendations.RIUtilization, error) {
			return []recommendations.RIUtilization{{ReservedInstanceID: zoneTestRIID, UtilizationPercent: 40.0}}, nil
		},
		exchangeClient: &mockExchangeClient{
			getQuoteFunc: func(context.Context, exchange.ExchangeQuoteRequest) (*exchange.ExchangeQuoteSummary, error) {
				quoted = true
				return &exchange.ExchangeQuoteSummary{
					IsValidExchange:  true,
					PaymentDueUSD:    new(big.Rat).SetFloat64(5.00),
					PaymentDueUSDStr: "5.00",
					CurrencyCode:     "USD",
				}, nil
			},
		},
		lookupOffering: newConvertibleOfferingLookup(c),
		accountID:      "123456789012",
		region:         "us-east-1",
	}

	result, err := app.executeRIExchangeReshape(context.Background(), cfg, clients)
	require.NoError(t, err)
	return result, api, quoted
}

func TestExecuteRIExchangeReshape_ZonalRIForwardsAvailabilityZone(t *testing.T) {
	result, api, quoted := runZoneReshape(t, zoneTestRI(ec2types.ScopeAvailabilityZone, "us-east-1b"))

	require.NotEmpty(t, api.lookupZones, "offering lookup must reach the provider")
	for _, z := range api.lookupZones {
		assert.Equal(t, "us-east-1b", aws.ToString(z), "every lookup must be pinned to the source RI's AZ")
	}
	assert.True(t, quoted)
	require.Len(t, result.Pending, 1)
	assert.Empty(t, result.Skipped)
}

func TestExecuteRIExchangeReshape_RegionalRIPassesNoAvailabilityZone(t *testing.T) {
	result, api, quoted := runZoneReshape(t, zoneTestRI(ec2types.ScopeRegional, ""))

	require.NotEmpty(t, api.lookupZones, "offering lookup must reach the provider")
	for _, z := range api.lookupZones {
		assert.Nil(t, z, "a regional lookup must not name an AZ")
	}
	assert.True(t, quoted)
	require.Len(t, result.Pending, 1)
}

// A zonal RI whose AZ is missing is refused by the provider before any AWS
// call; the refusal reaches the result as the skip reason, not a silent drop
// and not a quote in an arbitrary zone.
func TestExecuteRIExchangeReshape_ZonalRIWithoutAZRefused(t *testing.T) {
	result, api, quoted := runZoneReshape(t, zoneTestRI(ec2types.ScopeAvailabilityZone, ""))

	assert.Empty(t, api.lookupZones, "no offering lookup without an AZ")
	assert.False(t, quoted, "no quote without an AZ")
	assert.Empty(t, result.Pending)
	require.NotEmpty(t, result.Skipped)
	for _, s := range result.Skipped {
		assert.Equal(t, zoneTestRIID, s.SourceRIID)
		assert.Contains(t, s.Reason, "zonal RI offering needs an availability zone")
	}
}

// fakeOfferingFinder records the params the lookup adapter sends.
type fakeOfferingFinder struct {
	got ec2svc.FindConvertibleOfferingParams
}

func (f *fakeOfferingFinder) FindConvertibleOffering(_ context.Context, p ec2svc.FindConvertibleOfferingParams) (string, error) {
	f.got = p
	return "offering-1", nil
}

func TestNewConvertibleOfferingLookup_MapsEveryField(t *testing.T) {
	f := &fakeOfferingFinder{}
	id, err := newConvertibleOfferingLookup(f)(context.Background(), exchange.LookupOfferingParams{
		InstanceType: "m5.xlarge", ProductDescription: "Linux/UNIX", Tenancy: "default",
		Scope: "Availability Zone", AvailabilityZone: "us-east-1b", Duration: 31536000,
	})
	require.NoError(t, err)
	assert.Equal(t, "offering-1", id)
	assert.Equal(t, ec2svc.FindConvertibleOfferingParams{
		InstanceType: "m5.xlarge", ProductDescription: "Linux/UNIX", Tenancy: "default",
		Scope: "Availability Zone", AvailabilityZone: "us-east-1b", Duration: 31536000,
	}, f.got)
}

func TestConvertForAutoExchange_CarriesAvailabilityZone(t *testing.T) {
	_, _, meta := convertForAutoExchange([]ec2svc.ConvertibleRI{
		{ReservedInstanceID: "ri-zonal", Scope: "Availability Zone", AvailabilityZone: "us-east-1b"},
		{ReservedInstanceID: "ri-regional", Scope: "Region"},
	}, nil)
	assert.Equal(t, "us-east-1b", meta["ri-zonal"].AvailabilityZone)
	assert.Empty(t, meta["ri-regional"].AvailabilityZone)
}
