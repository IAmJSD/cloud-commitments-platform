package purchase

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// runFanOutCapturingOpts runs a two-account fan-out of recs built by mk and
// returns the PurchaseOptions every PurchaseCommitment call received.
func runFanOutCapturingOpts(t *testing.T, service common.ServiceType, mk func(string) config.RecommendationRecord) []common.PurchaseOptions {
	t.Helper()
	ctx := context.Background()
	accounts := []config.CloudAccount{
		{ID: "acct-A", Name: "A", Provider: "aws", ExternalID: "111111111111", AWSAuthMode: "access_keys"},
		{ID: "acct-B", Name: "B", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "access_keys"},
	}
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	mockServiceClient := new(MockServiceClient)
	assertMocksOnCleanup(t, mockStore, mockEmail, mockFactory, mockProviderInst, mockServiceClient)

	exec := &config.PurchaseExecution{
		ExecutionID: "root-exec", IdempotencyKey: "root-lineage", PlanID: "plan-x",
		Source:          common.PurchaseSourceWeb,
		Recommendations: []config.RecommendationRecord{mk("acct-A"), mk("acct-B")},
	}
	mockStore.SavePurchaseExecutionFn = func(context.Context, *config.PurchaseExecution) error { return nil }
	mockStore.On("SavePurchaseHistory", ctx, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil)
	mockEmail.On("SendPurchaseConfirmation", ctx, mock.AnythingOfType("email.NotificationData")).Return(nil)
	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil)
	mockProviderInst.On("GetServiceClient", mock.Anything, service, mock.Anything).Return(mockServiceClient, nil)

	var mu sync.Mutex
	var got []common.PurchaseOptions
	mockServiceClient.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			got = append(got, args.Get(2).(common.PurchaseOptions))
			mu.Unlock()
		}).Return(common.PurchaseResult{Success: true, CommitmentID: "ri-ok"}, nil)

	manager := &Manager{
		config: mockStore, email: mockEmail, providerFactory: mockFactory,
		credStore: awsAccessKeyCredStore(), dashboardURL: "https://dashboard.example.com",
	}
	require.NoError(t, manager.executeMultiAccount(ctx, exec, &config.PurchasePlan{ID: "plan-x", Name: "Plan X"}, accounts))
	return got
}

// optsIgnoringRequestedAt matches PurchaseOptions equal to want apart from the
// dispatch-time RequestedAt, which is a clock reading.
func optsIgnoringRequestedAt(want common.PurchaseOptions) interface{} {
	return mock.MatchedBy(func(got common.PurchaseOptions) bool {
		got.RequestedAt = want.RequestedAt
		return got == want
	})
}

func assertRequestedAtStamped(t *testing.T, got []common.PurchaseOptions, before, after time.Time) {
	t.Helper()
	require.Len(t, got, 2)
	for _, o := range got {
		require.False(t, o.RequestedAt.IsZero(), "RequestedAt must be set at dispatch")
		assert.Equal(t, time.UTC, o.RequestedAt.Location())
		assert.False(t, o.RequestedAt.Before(before), "RequestedAt %s before dispatch start %s", o.RequestedAt, before)
		assert.False(t, o.RequestedAt.After(after), "RequestedAt %s after dispatch end %s", o.RequestedAt, after)
	}
}

func TestExecuteMultiAccount_StampsRequestedAtOnEveryDispatch(t *testing.T) {
	before := time.Now()
	got := runFanOutCapturingOpts(t, common.ServiceEC2, scopedTestRec)
	assertRequestedAtStamped(t, got, before, time.Now())
	for _, o := range got {
		assert.NotEmpty(t, o.IdempotencyToken)
		assert.Equal(t, common.PurchaseSourceWeb, o.Source)
		assert.NotEmpty(t, o.ExecutionID)
	}
}

func TestExecuteMultiAccount_StampsRequestedAtOnRedshift(t *testing.T) {
	mk := func(acct string) config.RecommendationRecord {
		r := scopedTestRec(acct)
		r.Service, r.ResourceType = "redshift", "ra3.xlplus"
		return r
	}
	before := time.Now()
	got := runFanOutCapturingOpts(t, common.ServiceRedshift, mk)
	assertRequestedAtStamped(t, got, before, time.Now())
}

// With no idempotency token the options pass through unchanged apart from
// RequestedAt.
func TestExecuteSinglePurchase_EmptyTokenOptsOtherwiseUntouched(t *testing.T) {
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	mockServiceClient := new(MockServiceClient)
	assertMocksOnCleanup(t, mockFactory, mockProviderInst, mockServiceClient)
	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil)
	mockProviderInst.On("GetServiceClient", mock.Anything, common.ServiceEC2, mock.Anything).Return(mockServiceClient, nil)
	var got common.PurchaseOptions
	mockServiceClient.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { got = args.Get(2).(common.PurchaseOptions) }).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-ok"}, nil)
	m := &Manager{providerFactory: mockFactory}
	in := common.PurchaseOptions{Source: common.PurchaseSourceWeb, ExecutionID: "exec-1"}

	before := time.Now()
	_, err := m.executeSinglePurchase(context.Background(), scopedTestRec("acct-A"), nil, in)
	after := time.Now()

	require.NoError(t, err)
	assertRequestedAtStamped(t, []common.PurchaseOptions{got, got}, before, after)
	got.RequestedAt = time.Time{}
	assert.Equal(t, in, got)
}
