package purchase

// Issue #1037: the stranded-approval re-drive must buy the row its claim
// returned, not the snapshot RecoverStrandedApprovals listed before the claim.
// Mock store and mock provider (labeled fixtures; nothing here reaches a cloud
// API). The Postgres version of the race is in
// redrive_claimed_row_integration_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const redriveExecID = "exec-redrive-1037"

// redriveSnapshot is the row as the sweep's SELECT saw it: a stranded approved
// AWS RDS purchase of one instance, safe to re-drive.
func redriveSnapshot() config.PurchaseExecution {
	return config.PurchaseExecution{
		ExecutionID: redriveExecID,
		PlanID:      "plan-redrive-1037",
		Status:      "approved",
		StepNumber:  1,
		Recommendations: []config.RecommendationRecord{
			{Provider: "aws", Service: "rds", ResourceType: "db.r5.large", Region: "us-east-1", Count: 1, UpfrontCost: 200, Selected: true},
		},
		TotalUpfrontCost: 200,
	}
}

// refuseProviderCalls makes any provider construction fail AND be recorded, so
// AssertNotCalled can see it. Without an expectation the mock panics inside the
// fan-out's recover, and the attempt would never reach the call log.
func refuseProviderCalls(factory *MockProviderFactory) {
	factory.On("CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("the provider must not be reached")).Maybe()
}

// expectRedriveClaim lists snapshot and makes the approved -> running claim
// return claimed, modeling a write that landed between the SELECT and the CAS.
func expectRedriveClaim(ctx context.Context, store *MockConfigStore, snapshot, claimed config.PurchaseExecution) {
	claimed.Status = "running"
	store.On("GetStaleApprovedExecutions", ctx, staleApprovedThreshold).Return([]config.PurchaseExecution{snapshot}, nil)
	store.On("TransitionExecutionStatus", ctx, redriveExecID, []string{"approved"}, "running", (*string)(nil)).Return(&claimed, nil).Once()
}

// The claimed row was repriced (1 -> 7 instances, $200 -> $1400) after the
// sweep listed it. The provider must be asked for the claimed 7, and the
// terminal row must carry the claimed recommendations. Before the fix the
// re-drive bought the listed 1 and then saved the stale snapshot over the row.
func TestRecoverStrandedApprovals_RedrivesClaimedRowNotSnapshot(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	emailSender := new(MockEmailSender)
	stsClient := new(MockSTSClient)
	factory := new(MockProviderFactory)
	prov := new(MockProvider)
	assertMocksOnCleanup(t, emailSender, stsClient, factory, prov)
	client := new(MockServiceClient)
	t.Cleanup(func() { store.AssertExpectations(t) })
	t.Cleanup(func() { client.AssertExpectations(t) })

	snapshot := redriveSnapshot()
	claimed := redriveSnapshot()
	claimed.Recommendations[0].Count = 7
	claimed.Recommendations[0].UpfrontCost = 1400
	claimed.TotalUpfrontCost = 1400
	expectRedriveClaim(ctx, store, snapshot, claimed)

	store.On("GetPurchasePlan", mock.Anything, snapshot.PlanID).Return(&config.PurchasePlan{
		ID: snapshot.PlanID, RampSchedule: config.RampSchedule{TotalSteps: 4},
	}, nil).Once()
	store.On("SavePurchaseHistory", mock.Anything, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil)
	store.On("CompletePlanStep", mock.Anything, snapshot.PlanID, 1).Return(nil)
	var saved *config.PurchaseExecution
	store.On("SavePurchaseExecution", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution")).
		Run(func(args mock.Arguments) { saved = args.Get(1).(*config.PurchaseExecution) }).Return(nil)
	emailSender.On("SendPurchaseConfirmation", mock.Anything, mock.AnythingOfType("email.NotificationData")).Return(nil)
	stsClient.On("GetCallerIdentity", mock.Anything, mock.AnythingOfType("*sts.GetCallerIdentityInput")).
		Return(&sts.GetCallerIdentityOutput{Account: aws.String("123456789012")}, nil)
	factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(prov, nil)
	prov.On("GetServiceClient", mock.Anything, common.ServiceRDS, "us-east-1").Return(client, nil)
	var boughtCounts []int
	client.On("PurchaseCommitment", mock.Anything, mock.AnythingOfType("common.Recommendation"), mock.AnythingOfType("common.PurchaseOptions")).
		Run(func(args mock.Arguments) {
			boughtCounts = append(boughtCounts, args.Get(1).(common.Recommendation).Count)
		}).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-redrive-1037"}, nil)

	mgr := &Manager{config: store, email: emailSender, stsClient: stsClient, providerFactory: factory, dashboardURL: "https://dashboard.example.com"}
	recovered, err := mgr.RecoverStrandedApprovals(ctx)

	require.NoError(t, err)
	assert.Equal(t, 1, recovered)
	assert.Equal(t, []int{7}, boughtCounts, "the re-drive must buy the claimed row's 7 instances, not the listed snapshot's 1")
	require.NotNil(t, saved)
	assert.Equal(t, "completed", saved.Status)
	require.Len(t, saved.Recommendations, 1)
	assert.Equal(t, 7, saved.Recommendations[0].Count, "the terminal save must not write the stale snapshot back over the claimed row")
	assert.InDelta(t, 1400, saved.TotalUpfrontCost, 0.001)
}

// The listed snapshot was safe to re-drive, but by the time the claim landed
// the row no longer qualifies: its rec became EC2 (no provider duplicate
// guard) or it lost its recommendations. Nothing may be bought, and the row is
// failed the way safeFail fails a listed row in that state: no no_submission
// stamp, because the interrupted original run may already have bought.
func TestRecoverStrandedApprovals_ClaimedRowNoLongerRedrivableIsFailed(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*config.PurchaseExecution)
		wantError string
	}{
		{
			name: "recommendation changed to EC2",
			mutate: func(e *config.PurchaseExecution) {
				e.Recommendations[0].Service = "ec2"
				e.Recommendations[0].ResourceType = "m5.large"
			},
			wantError: "start a new purchase",
		},
		{
			name:      "recommendations emptied",
			mutate:    func(e *config.PurchaseExecution) { e.Recommendations = nil },
			wantError: "then Retry",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := new(MockConfigStore)
			factory := new(MockProviderFactory)
			refuseProviderCalls(factory)
			t.Cleanup(func() { store.AssertExpectations(t) })

			snapshot := redriveSnapshot()
			claimed := redriveSnapshot()
			tc.mutate(&claimed)
			expectRedriveClaim(ctx, store, snapshot, claimed)
			var saved *config.PurchaseExecution
			var savedFrom []string
			store.On("SaveExecutionIfStatus", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution"), mock.Anything).
				Run(func(args mock.Arguments) {
					saved = args.Get(1).(*config.PurchaseExecution)
					savedFrom = args.Get(2).([]string)
				}).Return(nil).Once()

			mgr := &Manager{config: store, providerFactory: factory, dashboardURL: "https://dashboard.example.com"}
			recovered, err := mgr.RecoverStrandedApprovals(ctx)

			require.NoError(t, err)
			assert.Equal(t, 1, recovered, "a failed stranded row counts as recovered, as on the safeFail path")
			factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
			require.NotNil(t, saved)
			assert.Equal(t, []string{"running"}, savedFrom, "the terminal save must be guarded on the claim this run holds")
			assert.Equal(t, "failed", saved.Status)
			assert.False(t, saved.NoSubmission, "the original run may have bought: no_submission must not be stamped")
			assert.Contains(t, saved.Error, "interrupted before completing")
			assert.Contains(t, saved.Error, tc.wantError)
			assert.Equal(t, claimed.Recommendations, saved.Recommendations, "the failed row keeps the claimed recommendations")
		})
	}
}

// A terminal save failure after failing the claimed row leaves it in
// "running"; the sweep must surface that as ErrAuditLoss, not count it.
func TestRecoverStrandedApprovals_ClaimedRowFailSaveErrorIsAuditLoss(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	factory := new(MockProviderFactory)
	refuseProviderCalls(factory)
	t.Cleanup(func() { store.AssertExpectations(t) })

	claimed := redriveSnapshot()
	claimed.Recommendations = nil
	expectRedriveClaim(ctx, store, redriveSnapshot(), claimed)
	saveErr := errors.New("db connection lost")
	store.On("SaveExecutionIfStatus", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution"), []string{"running"}).
		Return(saveErr).Once()

	mgr := &Manager{config: store, providerFactory: factory}
	recovered, err := mgr.RecoverStrandedApprovals(ctx)

	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrAuditLoss)
	assert.ErrorIs(t, err, saveErr)
	assert.Equal(t, 0, recovered)
	factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
}
