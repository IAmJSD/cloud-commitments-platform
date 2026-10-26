package purchase

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/savingsplans"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const awsCNRoleARN = "arn:aws-cn:iam::123456789012:role/CUDly"

// TestRejectUnsupportedPartition pins which recs the aws-cn guard refuses
// (issue #1194): every Savings Plans slug for an aws-cn account, and nothing
// else.
func TestRejectUnsupportedPartition(t *testing.T) {
	cases := []struct {
		name      string
		partition string
		service   string
		refused   bool
	}{
		{"aws-cn umbrella SP slug", config.AWSPartitionChina, "savingsplans", true},
		{"aws-cn legacy SP slug", config.AWSPartitionChina, "savings-plans", true},
		{"aws-cn compute SP", config.AWSPartitionChina, "savings-plans-compute", true},
		{"aws-cn EC2 instance SP", config.AWSPartitionChina, "savingsplans-ec2instance", true},
		{"aws-cn SageMaker SP", config.AWSPartitionChina, "savings-plans-sagemaker", true},
		{"aws-cn database SP", config.AWSPartitionChina, "savings-plans-database", true},
		{"aws-cn EC2 RI is left to the provider", config.AWSPartitionChina, "ec2", false},
		{"commercial SP still buys", config.AWSPartitionStandard, "savings-plans-compute", false},
		{"GovCloud SP still buys", config.AWSPartitionGovCloud, "savings-plans-compute", false},
		{"unknown partition leaves the library guard", "", "savings-plans-compute", false},
	}
	m := &Manager{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := config.RecommendationRecord{Provider: "aws", Service: tc.service}
			err := m.rejectUnsupportedPartition(&rec, tc.partition)
			if !tc.refused {
				assert.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, savingsplans.ErrUnsupportedPartition)
			assert.Contains(t, err.Error(), "aws-cn")
		})
	}
}

// newAWSCNManager builds a Manager whose provider factory records any call
// and fails it, so a test can assert that no provider was built (and so no
// AWS API was reached) for a refused rec.
func newAWSCNManager(store *MockConfigStore, factory *MockProviderFactory, emailSender *MockEmailSender) *Manager {
	factory.On("CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("provider must not be built for an aws-cn Savings Plans rec")).Maybe()
	return &Manager{
		config:          store,
		email:           allowFailedNotification(emailSender),
		providerFactory: factory,
		assumeRoleSTS:   new(MockAssumeRoleSTS),
		dashboardURL:    "https://dashboard.example.com",
	}
}

func awsCNSPRec(accountID *string) config.RecommendationRecord {
	return config.RecommendationRecord{
		Provider: "aws", Service: "savings-plans-compute", ResourceType: "compute-sp",
		Count: 1, Term: 1, Payment: "no-upfront", Savings: 40, Selected: true, CloudAccountID: accountID,
	}
}

// TestExecuteForAccount_AWSChinaSavingsPlanRefusedBeforeAnyAWSCall is the
// issue #1194 regression for the plan fan-out path: an SP rec for an account
// whose role ARN is in aws-cn fails with the unsupported-partition error and
// no provider is built for it.
func TestExecuteForAccount_AWSChinaSavingsPlanRefusedBeforeAnyAWSCall(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockFactory := new(MockProviderFactory)
	var saved *config.PurchaseExecution
	mockStore.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
		c := *e
		saved = &c
		return nil
	}
	m := newAWSCNManager(mockStore, mockFactory, new(MockEmailSender))

	account := config.CloudAccount{
		ID: "acct-cn", Name: "China", Provider: "aws", ExternalID: "123456789012",
		AWSAuthMode: "role_arn", AWSRoleARN: awsCNRoleARN, AWSExternalID: "ext-id-placeholder",
	}
	baseExec := &config.PurchaseExecution{
		ExecutionID:     "exec-cn",
		PlanID:          "plan-cn",
		Source:          common.PurchaseSourceWeb,
		Recommendations: []config.RecommendationRecord{awsCNSPRec(nil)},
	}

	committed, err := m.executeForAccount(ctx, baseExec, &config.PurchasePlan{ID: "plan-cn", Name: "CN"}, account)
	require.Error(t, err)
	assert.False(t, committed)
	mockFactory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)

	require.NotNil(t, saved, "the per-account row must be saved")
	assert.Equal(t, "failed", saved.Status)
	assert.Contains(t, saved.Error, savingsplans.ErrUnsupportedPartition.Error())
	require.Len(t, saved.Recommendations, 1)
	assert.False(t, saved.Recommendations[0].Purchased)
	assert.Contains(t, saved.Recommendations[0].Error, savingsplans.ErrUnsupportedPartition.Error())
}

// TestExecutePurchase_SingleAccount_AWSChinaSavingsPlanRefused is the issue
// #1194 regression for the direct-execute path: the target account comes from
// the recs, and the SP rec is refused before any provider is built, while a
// non-SP rec in the same execution still reaches the provider.
func TestExecutePurchase_SingleAccount_AWSChinaSavingsPlanRefused(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	mockServiceClient := new(MockServiceClient)

	acctID := "acct-cn-direct"
	mockStore.On("GetCloudAccount", ctx, acctID).Return(&config.CloudAccount{
		ID: acctID, Provider: "aws", ExternalID: "123456789012",
		AWSAuthMode: "role_arn", AWSRoleARN: awsCNRoleARN, AWSExternalID: "ext-id-placeholder",
	}, nil)
	mockStore.On("SavePurchaseHistory", ctx, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil)
	// The EC2 rec is the control: it must still reach the provider. Registered
	// before newAWSCNManager's catch-all so it matches first.
	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil).Once()
	mockProviderInst.On("GetServiceClient", mock.Anything, common.ServiceEC2, "cn-north-1").Return(mockServiceClient, nil).Once()
	mockServiceClient.On("PurchaseCommitment", mock.Anything,
		mock.MatchedBy(func(r common.Recommendation) bool { return r.Service == common.ServiceEC2 }),
		mock.AnythingOfType("common.PurchaseOptions"),
	).Return(common.PurchaseResult{Success: true, CommitmentID: "ri-cn"}, nil).Once()
	mockEmail := new(MockEmailSender)
	mockEmail.On("SendPurchaseConfirmation", ctx, mock.AnythingOfType("email.NotificationData")).Return(nil)
	m := newAWSCNManager(mockStore, mockFactory, mockEmail)

	exec := &config.PurchaseExecution{
		ExecutionID: "exec-cn-direct",
		Source:      common.PurchaseSourceWeb,
		Recommendations: []config.RecommendationRecord{
			awsCNSPRec(&acctID),
			{Provider: "aws", Service: "ec2", ResourceType: "m5.large", Region: "cn-north-1", Count: 1, UpfrontCost: 100, Selected: true, CloudAccountID: &acctID},
		},
	}

	err := m.executePurchase(ctx, exec)
	var partial *partialPurchaseError
	require.ErrorAs(t, err, &partial, "the EC2 rec committed, so the run is a partial success")
	assert.Contains(t, err.Error(), savingsplans.ErrUnsupportedPartition.Error())

	mockFactory.AssertNumberOfCalls(t, "CreateAndValidateProvider", 1)
	mockProviderInst.AssertNotCalled(t, "GetServiceClient", mock.Anything, common.ServiceSavingsPlansCompute, mock.Anything)
	assert.False(t, exec.Recommendations[0].Purchased)
	assert.Contains(t, exec.Recommendations[0].Error, savingsplans.ErrUnsupportedPartition.Error())
	assert.True(t, exec.Recommendations[1].Purchased)
	mockServiceClient.AssertExpectations(t)
}

// countingHTTPClient fails every request and counts it, so a test can prove a
// real SDK client made no AWS call without touching the network.
type countingHTTPClient struct{ calls atomic.Int32 }

func (c *countingHTTPClient) Do(*http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return nil, errors.New("network disabled in tests")
}

// TestPinnedSavingsPlansClientRefusesAWSCN pins the providers/aws bump
// (issue #1194): the pinned library must refuse an aws-cn Savings Plans
// purchase with ErrUnsupportedPartition before sending any request. This is
// the backstop for accounts whose partition the platform cannot read from a
// role ARN (access_keys, or a self-account on ambient credentials).
func TestPinnedSavingsPlansClientRefusesAWSCN(t *testing.T) {
	httpClient := &countingHTTPClient{}
	client := savingsplans.NewClient(aws.Config{
		Region:      "cn-north-1",
		Credentials: aws.AnonymousCredentials{},
		HTTPClient:  httpClient,
	}, "Compute")
	rec := common.Recommendation{
		Provider:      common.ProviderAWS,
		Service:       common.ServiceSavingsPlansCompute,
		ResourceType:  "Compute",
		Region:        "cn-north-1",
		Count:         1,
		Term:          "1yr",
		PaymentOption: "no-upfront",
		Details:       &common.SavingsPlanDetails{PlanType: "Compute", HourlyCommitment: 1},
	}

	_, err := client.PurchaseCommitment(context.Background(), rec, common.PurchaseOptions{})
	require.ErrorIs(t, err, savingsplans.ErrUnsupportedPartition)
	assert.Zero(t, httpClient.calls.Load(), "no AWS request may be sent for an aws-cn Savings Plans purchase")
}
