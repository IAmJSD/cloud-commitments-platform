package scheduler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func zonalRec(account, az string) common.Recommendation {
	return common.Recommendation{
		Provider: common.ProviderAWS, Account: account, Service: common.ServiceEC2, Region: "us-east-1",
		ResourceType: "m5.large", Count: 1, Term: "1yr", PaymentOption: "all-upfront",
		Details: &common.ComputeDetails{InstanceType: "m5.large", Platform: "Linux/UNIX", Tenancy: "default", Scope: "Availability Zone", AvailabilityZone: az},
	}
}

// Two zonal recs for the same type and term that differ only by AZ must get
// distinct IDs and carry the AZ at the top level and inside Details (#792).
func TestConvertRecommendations_ZonalRecsKeepAZAndGetDistinctIDs(t *testing.T) {
	s := &Scheduler{}
	out := s.convertRecommendations([]common.Recommendation{zonalRec("", "us-east-1a"), zonalRec("", "us-east-1b")}, "aws")
	require.Len(t, out, 2)
	assert.Equal(t, "us-east-1a", out[0].AvailabilityZone)
	assert.Equal(t, "us-east-1b", out[1].AvailabilityZone)
	assert.NotEqual(t, out[0].ID, out[1].ID)
	var d common.ComputeDetails
	require.NoError(t, json.Unmarshal(out[1].Details, &d))
	assert.Equal(t, "us-east-1b", d.AvailabilityZone)
}

// A regional EC2 rec has no AZ suffix; it carries only the platform/tenancy pair (issue #806).
func TestConvertRecommendations_RegionalRecIDHasNoAZSuffix(t *testing.T) {
	s := &Scheduler{}
	r := zonalRec("", "")
	r.Details.(*common.ComputeDetails).Scope = "region"
	out := s.convertRecommendations([]common.Recommendation{r}, "aws")
	require.Len(t, out, 1)
	assert.Empty(t, out[0].AvailabilityZone)
	assert.Equal(t, "aws||ec2|us-east-1|m5.large||1|all-upfront|Linux/UNIX|default", out[0].ID)
}

func TestDropForeignZonalEC2(t *testing.T) {
	regional := zonalRec("222222222222", "")
	recs := []common.Recommendation{
		zonalRec("111111111111", "us-east-1a"), // own account: kept
		zonalRec("", "us-east-1b"),             // no account: kept
		zonalRec("222222222222", "us-east-1c"), // member account zonal: dropped
		regional,                               // member account regional: kept
	}
	got := dropForeignZonalEC2(recs, "111111111111")
	require.Len(t, got, 3)
	assert.Equal(t, "us-east-1a", zonalAvailabilityZone(got[0].Details))
	assert.Equal(t, "us-east-1b", zonalAvailabilityZone(got[1].Details))
	assert.Equal(t, "", zonalAvailabilityZone(got[2].Details))

	assert.Len(t, dropForeignZonalEC2(recs, ""), 4, "unknown collecting identity disables the check")
}

func TestPartitionForeignZonalEC2_CountsDropped(t *testing.T) {
	recs := []common.Recommendation{
		zonalRec("111111111111", "us-east-1a"),
		zonalRec("222222222222", "us-east-1b"),
		zonalRec("333333333333", "us-east-1c"),
		zonalRec("222222222222", ""),
	}
	kept, dropped := partitionForeignZonalEC2(recs, "111111111111")
	assert.Equal(t, 2, dropped)
	assert.Len(t, kept, 2)
	kept, dropped = partitionForeignZonalEC2(recs, "")
	assert.Equal(t, 0, dropped)
	assert.Len(t, kept, 4)
}

// mixedAccountRecs is what Cost Explorer returns for a payer: the collecting
// account's own zonal rec, a member account's zonal rec, a member account's
// regional rec.
func mixedAccountRecs(own string) []common.Recommendation {
	memberRegional := zonalRec("222222222222", "")
	memberRegional.Details.(*common.ComputeDetails).Scope = "Region"
	memberRegional.ResourceType = "c5.large"
	return []common.Recommendation{zonalRec(own, "us-east-1a"), zonalRec("222222222222", "us-east-1b"), memberRegional}
}

func assertOnlyMemberZonalDropped(t *testing.T, recs []config.RecommendationRecord, gap *sweepGap) {
	t.Helper()
	assert.Nil(t, gap, "dropping a foreign zonal rec must not mark the collection incomplete")
	require.Len(t, recs, 2)
	azs := []string{recs[0].AvailabilityZone, recs[1].AvailabilityZone}
	assert.ElementsMatch(t, []string{"us-east-1a", ""}, azs, "own zonal and member regional kept; member zonal dropped")
}

func newRecProvider(client *MockRecommendationsClient) *MockProvider {
	prov := new(MockProvider)
	prov.On("GetRecommendationsClient", mock.Anything).Return(client, nil)
	return prov
}

func TestFetchAndConvert_DropsForeignZonalEC2_Primary(t *testing.T) {
	client := new(MockRecommendationsClient)
	client.On("GetAllRecommendations", mock.Anything).Return(mixedAccountRecs("111111111111"), nil)
	s := &Scheduler{}
	recs, gap, err := s.fetchAndConvert(context.Background(), newRecProvider(client), "aws", nil, "111111111111", nil)
	require.NoError(t, err)
	assertOnlyMemberZonalDropped(t, recs, gap)
}

func TestFetchAndConvert_DropsForeignZonalEC2_LookbackFallback(t *testing.T) {
	client := new(MockRecommendationsClient)
	client.On("GetAllRecommendations", mock.Anything).Return([]common.Recommendation{}, nil)
	client.On("GetRecommendations", mock.Anything, mock.MatchedBy(func(p *common.RecommendationParams) bool {
		return p.Service == common.ServiceEC2
	})).Return(mixedAccountRecs("111111111111"), nil)
	client.On("GetRecommendations", mock.Anything, mock.MatchedBy(func(p *common.RecommendationParams) bool {
		return p.Service != common.ServiceEC2
	})).Return([]common.Recommendation{}, nil)
	s := &Scheduler{}
	recs, gap, err := s.fetchAndConvert(context.Background(), newRecProvider(client), "aws", nil, "111111111111",
		&config.GlobalConfig{DefaultTerm: 1, DefaultPayment: "no-upfront", RecommendationsLookbackDays: 30})
	require.NoError(t, err)
	assertOnlyMemberZonalDropped(t, recs, gap)
}

func TestFetchAndConvert_NilExpectedAccountKeepsForeignZonal(t *testing.T) {
	client := new(MockRecommendationsClient)
	client.On("GetAllRecommendations", mock.Anything).Return(mixedAccountRecs("111111111111"), nil)
	s := &Scheduler{}
	recs, _, err := s.fetchAndConvert(context.Background(), newRecProvider(client), "aws", nil, "", nil)
	require.NoError(t, err)
	assert.Len(t, recs, 3, "an unknown collecting identity disables the drop")
}

// The ambient path must compare against the STS host account.
func TestCollectAWSAmbient_PassesHostAccount(t *testing.T) {
	ctx := context.Background()
	factory := new(MockProviderFactory)
	client := new(MockRecommendationsClient)
	client.On("GetAllRecommendations", mock.Anything).Return(mixedAccountRecs("111111111111"), nil)
	factory.On("CreateAndValidateProvider", ctx, "aws", (*provider.ProviderConfig)(nil)).Return(newRecProvider(client), nil)
	s := &Scheduler{providerFactory: factory, stsClient: &fakeSTSClient{accountID: "111111111111"}}
	recs, gap, err := s.collectAWSAmbient(ctx, nil)
	require.NoError(t, err)
	assertOnlyMemberZonalDropped(t, recs, gap)
}

// A registered self account must compare against its ExternalID.
func TestCollectAWSForAccount_SelfAccountPassesExternalID(t *testing.T) {
	ctx := context.Background()
	factory := new(MockProviderFactory)
	client := new(MockRecommendationsClient)
	client.On("GetAllRecommendations", mock.Anything).Return(mixedAccountRecs("111111111111"), nil)
	factory.On("CreateAndValidateProvider", ctx, "aws", (*provider.ProviderConfig)(nil)).Return(newRecProvider(client), nil)
	s := &Scheduler{config: new(MockConfigStore), providerFactory: factory, stsClient: &fakeSTSClient{accountID: "111111111111"}}
	recs, gap, err := s.collectAWSForAccount(ctx, nil, config.CloudAccount{
		ID: "self", Provider: "aws", AWSAuthMode: "role_arn", ExternalID: "111111111111", Enabled: true,
	})
	require.NoError(t, err)
	assertOnlyMemberZonalDropped(t, recs, gap)
}

// A member account reached through an assumed role must compare against its
// ExternalID, not the host account.
func TestCollectAWSForAccount_AssumedRolePassesExternalID(t *testing.T) {
	ctx := context.Background()
	factory := new(MockProviderFactory)
	client := new(MockRecommendationsClient)
	client.On("GetAllRecommendations", mock.Anything).Return(mixedAccountRecs("333333333333"), nil)
	factory.On("CreateAndValidateProvider", ctx, "aws", mock.AnythingOfType("*provider.ProviderConfig")).Return(newRecProvider(client), nil)
	s := &Scheduler{config: new(MockConfigStore), providerFactory: factory, stsClient: &fakeSTSClient{accountID: "111111111111"}}
	recs, gap, err := s.collectAWSForAccount(ctx, nil, config.CloudAccount{
		ID: "member", Provider: "aws", AWSAuthMode: "role_arn", AWSRoleARN: "arn:aws:iam::333333333333:role/cudly",
		ExternalID: "333333333333", Enabled: true,
	})
	require.NoError(t, err)
	assert.Nil(t, gap)
	require.Len(t, recs, 2)
	// 333333333333 owns the first zonal rec; the 222222222222 zonal rec is dropped.
	assert.ElementsMatch(t, []string{"us-east-1a", ""}, []string{recs[0].AvailabilityZone, recs[1].AvailabilityZone})
}
