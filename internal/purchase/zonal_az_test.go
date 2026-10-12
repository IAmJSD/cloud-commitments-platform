package purchase

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

func zonalRecRecord(t *testing.T, az string) config.RecommendationRecord {
	t.Helper()
	blob, err := common.MarshalServiceDetails(&common.ComputeDetails{
		InstanceType: "m5.large", Platform: "Linux/UNIX", Tenancy: "default",
		Scope: "availability-zone", AvailabilityZone: az,
	})
	require.NoError(t, err)
	return config.RecommendationRecord{
		Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Count: 1, Term: 1, Payment: "no-upfront", Details: blob,
	}
}

func managerServing(sc provider.ServiceClient) *Manager {
	prov := new(MockProvider)
	prov.On("GetServiceClient", mock.Anything, mock.Anything, "us-east-1").Return(sc, nil)
	factory := new(MockProviderFactory)
	factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(prov, nil)
	return &Manager{providerFactory: factory}
}

// A zonal rec stored WITH its AZ reaches the service client with the AZ
// (persisted blob -> decode -> PurchaseCommitment).
func TestExecuteSinglePurchase_ZonalRecKeepsAvailabilityZone(t *testing.T) {
	sc := new(MockServiceClient)
	var got common.Recommendation
	sc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { got = args.Get(1).(common.Recommendation) }).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-1"}, nil)

	_, err := managerServing(sc).executeSinglePurchase(context.Background(), zonalRecRecord(t, "us-east-1b"), nil, common.PurchaseOptions{ExecutionID: "exec-az"})
	require.NoError(t, err)

	d, ok := got.Details.(*common.ComputeDetails)
	require.True(t, ok)
	assert.Equal(t, "us-east-1b", d.AvailabilityZone)
}

// EC2 API stub that fails the test on any call that could spend money. The
// embedded nil interface makes any other call panic.
type noPurchaseEC2API struct {
	ec2svc.API
	purchases, lookups int
}

func (a *noPurchaseEC2API) PurchaseReservedInstancesOffering(context.Context, *awsec2sdk.PurchaseReservedInstancesOfferingInput, ...func(*awsec2sdk.Options)) (*awsec2sdk.PurchaseReservedInstancesOfferingOutput, error) {
	a.purchases++
	return &awsec2sdk.PurchaseReservedInstancesOfferingOutput{}, nil
}

func (a *noPurchaseEC2API) DescribeReservedInstancesOfferings(context.Context, *awsec2sdk.DescribeReservedInstancesOfferingsInput, ...func(*awsec2sdk.Options)) (*awsec2sdk.DescribeReservedInstancesOfferingsOutput, error) {
	a.lookups++
	return &awsec2sdk.DescribeReservedInstancesOfferingsOutput{}, nil
}

// A zonal row stored under an older pin has no AZ. The real EC2 client must
// refuse it before any offering lookup or purchase call.
func TestExecuteSinglePurchase_StaleZonalRowWithoutAZFailsClosed(t *testing.T) {
	api := &noPurchaseEC2API{}
	c := ec2svc.NewClient(awsConfigForTest())
	c.SetEC2API(api)

	_, err := managerServing(c).executeSinglePurchase(context.Background(), zonalRecRecord(t, ""), nil, common.PurchaseOptions{ExecutionID: "exec-stale"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "re-fetch")
	assert.Zero(t, api.purchases, "no purchase call")
	assert.Zero(t, api.lookups, "no offering lookup")
}

func awsConfigForTest() aws.Config { return aws.Config{Region: "us-east-1"} }
