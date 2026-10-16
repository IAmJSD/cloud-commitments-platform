package api

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
)

// Issue #921: GET /api/ri-exchange/target-offerings sends the source RI's AZ
// to the provider, and the provider's zone refusal is a 422. The real
// ec2svc.Client runs over a fake EC2 API: no AWS call.

// zoneRecordingTargetsAPI serves the source RI and records the AZ of every
// offering lookup. The embedded nil API panics on any other call.
type zoneRecordingTargetsAPI struct {
	ec2svc.API
	ri          ec2types.ReservedInstances
	lookupZones []*string
}

func (a *zoneRecordingTargetsAPI) DescribeReservedInstances(context.Context, *awsec2sdk.DescribeReservedInstancesInput, ...func(*awsec2sdk.Options)) (*awsec2sdk.DescribeReservedInstancesOutput, error) {
	return &awsec2sdk.DescribeReservedInstancesOutput{ReservedInstances: []ec2types.ReservedInstances{a.ri}}, nil
}

func (a *zoneRecordingTargetsAPI) DescribeReservedInstancesOfferings(_ context.Context, in *awsec2sdk.DescribeReservedInstancesOfferingsInput, _ ...func(*awsec2sdk.Options)) (*awsec2sdk.DescribeReservedInstancesOfferingsOutput, error) {
	a.lookupZones = append(a.lookupZones, in.AvailabilityZone)
	return &awsec2sdk.DescribeReservedInstancesOfferingsOutput{
		ReservedInstancesOfferings: []ec2types.ReservedInstancesOffering{{
			ReservedInstancesOfferingId: aws.String("4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91"),
			InstanceType:                ec2types.InstanceType("m5.xlarge"),
			AvailabilityZone:            in.AvailabilityZone,
		}},
	}, nil
}

func runTargetOfferingsForZone(t *testing.T, scope ec2types.Scope, az string) (any, *zoneRecordingTargetsAPI, error) {
	t.Helper()
	ri := ec2types.ReservedInstances{
		ReservedInstancesId: aws.String(targetOfferingsSourceRI),
		InstanceType:        ec2types.InstanceType("m5.large"),
		InstanceCount:       aws.Int32(1),
		ProductDescription:  ec2types.RIProductDescription("Linux/UNIX"),
		InstanceTenancy:     ec2types.TenancyDefault,
		Scope:               scope,
		Duration:            aws.Int64(31536000),
		OfferingType:        ec2types.OfferingTypeValuesNoUpfront,
		OfferingClass:       ec2types.OfferingClassTypeConvertible,
	}
	if az != "" {
		ri.AvailabilityZone = aws.String(az)
	}
	api := &zoneRecordingTargetsAPI{ri: ri}
	h := targetOfferingsHandler(t, scopedOutAccount, false)
	h.targetOfferingsEC2Factory = func(cfg aws.Config) targetOfferingsEC2Client {
		c := ec2svc.NewClient(cfg)
		c.SetEC2API(api)
		return c
	}
	res, err := NewRouter(h).Route(context.Background(), "GET", "/api/ri-exchange/target-offerings", targetOfferingsRequest())
	return res, api, err
}

func TestListTargetOfferings_ZonalSourceForwardsAvailabilityZone(t *testing.T) {
	res, api, err := runTargetOfferingsForZone(t, ec2types.ScopeAvailabilityZone, "us-east-1b")

	require.NoError(t, err)
	require.NotEmpty(t, api.lookupZones)
	for _, z := range api.lookupZones {
		assert.Equal(t, "us-east-1b", aws.ToString(z))
	}
	resp, ok := res.(*TargetOfferingsResponse)
	require.True(t, ok)
	assert.Len(t, resp.Offerings, 1)
}

func TestListTargetOfferings_RegionalSourcePassesNoAvailabilityZone(t *testing.T) {
	_, api, err := runTargetOfferingsForZone(t, ec2types.ScopeRegional, "")

	require.NoError(t, err)
	require.NotEmpty(t, api.lookupZones)
	for _, z := range api.lookupZones {
		assert.Nil(t, z)
	}
}

func TestListTargetOfferings_ZonalSourceWithoutAZIs422(t *testing.T) {
	_, api, err := runTargetOfferingsForZone(t, ec2types.ScopeAvailabilityZone, "")

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "zone refusal must be a client error, got %v", err)
	assert.Equal(t, 422, ce.code)
	assert.Contains(t, ce.message, "zonal RI offering needs an availability zone")
	assert.Contains(t, ce.message, targetOfferingsSourceRI)
	assert.Empty(t, api.lookupZones, "no AWS offering lookup without an AZ")
}

func TestIsOfferingZoneRefusal(t *testing.T) {
	assert.False(t, isOfferingZoneRefusal(assert.AnError))
	for _, msg := range offeringZoneRefusals {
		assert.True(t, isOfferingZoneRefusal(&clientError{message: "wrapped: " + msg}), msg)
	}
}

func TestListTargetOfferings_RegionalSourceWithAZIs422(t *testing.T) {
	_, api, err := runTargetOfferingsForZone(t, ec2types.ScopeRegional, "us-east-1b")

	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "zone refusal must be a client error, got %v", err)
	assert.Equal(t, 422, ce.code)
	assert.Contains(t, ce.message, "regional RI offering must not name an availability zone")
	assert.Empty(t, api.lookupZones)
}
