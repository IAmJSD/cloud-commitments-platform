package purchase

import (
	"context"
	"errors"
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

	// Redshift dispatch needs the lineage stamp executeAndFinalize mints; direct callers set it.
	stampedAt := time.Now().UTC()
	exec := &config.PurchaseExecution{
		ExecutionID: "root-exec", IdempotencyKey: "root-lineage", PlanID: "plan-x",
		FirstDispatchAt: &stampedAt,
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

type redshiftFinalizeRun struct {
	events     []string
	stampedIDs []string
	opts       []common.PurchaseOptions
	saves      []config.PurchaseExecution
	err        error
}

// runRedshiftFinalize drives executeAndFinalize for exec with a capturing
// provider. A nil stamp registers no StampFirstDispatchTx expectation, so any
// stamp call panics the mock.
func runRedshiftFinalize(t *testing.T, exec *config.PurchaseExecution, accounts []config.CloudAccount, service common.ServiceType,
	stamp func() (config.StampResult, error)) *redshiftFinalizeRun {
	t.Helper()
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	mockServiceClient := new(MockServiceClient)
	run := &redshiftFinalizeRun{}
	var mu sync.Mutex

	mockStore.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
		mu.Lock()
		run.saves = append(run.saves, *e)
		mu.Unlock()
		return nil
	}
	mockStore.GetPurchasePlanFn = func(_ context.Context, id string) (*config.PurchasePlan, error) {
		return &config.PurchasePlan{ID: id, Name: "Plan X"}, nil
	}
	mockStore.GetPlanAccountsFn = func(context.Context, string) ([]config.CloudAccount, error) { return accounts, nil }
	if stamp != nil {
		res, stampErr := stamp()
		mockStore.On("StampFirstDispatchTx", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) {
				mu.Lock()
				run.events = append(run.events, "stamp")
				run.stampedIDs = append(run.stampedIDs, args.String(2))
				mu.Unlock()
			}).Return(res, stampErr)
	}
	mockStore.On("CompletePlanStep", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	mockStore.On("SavePurchaseHistory", mock.Anything, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil).Maybe()
	mockEmail.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil).Maybe()
	mockEmail.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).Return(nil).Maybe()
	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil).Maybe()
	mockProviderInst.On("GetServiceClient", mock.Anything, service, mock.Anything).Return(mockServiceClient, nil).Maybe()
	mockServiceClient.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			run.events = append(run.events, "provider")
			run.opts = append(run.opts, args.Get(2).(common.PurchaseOptions))
			mu.Unlock()
		}).Return(common.PurchaseResult{Success: true, CommitmentID: "ri-ok"}, nil).Maybe()

	manager := &Manager{
		config: mockStore, email: mockEmail, providerFactory: mockFactory,
		credStore: awsAccessKeyCredStore(), dashboardURL: "https://dashboard.example.com",
	}
	run.err = manager.executeAndFinalize(ctx, exec)
	return run
}

func redshiftScopedRec(accountID string) config.RecommendationRecord {
	r := scopedTestRec(accountID)
	r.Service, r.ResourceType = "redshift", "ra3.xlplus"
	return r
}

func stampAt(at time.Time) func() (config.StampResult, error) {
	return func() (config.StampResult, error) { return config.StampResult{At: &at, WithinHorizon: true}, nil }
}

func TestExecuteAndFinalize_RedshiftStampsBeforeProvider(t *testing.T) {
	at := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Microsecond)
	exec := &config.PurchaseExecution{
		ExecutionID: "rs-exec", IdempotencyKey: "rs-lineage", Status: "running",
		Recommendations: []config.RecommendationRecord{redshiftScopedRec("acct-A")},
	}
	run := runRedshiftFinalize(t, exec, nil, common.ServiceRedshift, stampAt(at))

	require.NoError(t, run.err)
	assert.Equal(t, []string{"stamp", "provider"}, run.events)
	assert.Equal(t, []string{"rs-exec"}, run.stampedIDs)
	require.Len(t, run.opts, 1)
	assert.True(t, run.opts[0].RequestedAt.Equal(at), "RequestedAt %s must be the stamp %s, not a per-dispatch clock", run.opts[0].RequestedAt, at)
}

func TestExecuteAndFinalize_RedshiftFanOutAccountsShareRootStamp(t *testing.T) {
	at := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
	accounts := []config.CloudAccount{
		{ID: "acct-A", Name: "A", Provider: "aws", ExternalID: "111111111111", AWSAuthMode: "access_keys"},
		{ID: "acct-B", Name: "B", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "access_keys"},
	}
	exec := &config.PurchaseExecution{
		ExecutionID: "root-exec", IdempotencyKey: "root-lineage", PlanID: "plan-x", Status: "running",
		Recommendations: []config.RecommendationRecord{redshiftScopedRec("acct-A"), redshiftScopedRec("acct-B")},
	}
	run := runRedshiftFinalize(t, exec, accounts, common.ServiceRedshift, stampAt(at))

	require.NoError(t, run.err)
	assert.Equal(t, []string{"root-exec"}, run.stampedIDs, "only the root is stamped; per-account rows inherit the stamp")
	require.Len(t, run.opts, 2)
	for _, o := range run.opts {
		assert.True(t, o.RequestedAt.Equal(at), "every account dispatch must see the root stamp, got %s", o.RequestedAt)
	}
	perAccount := 0
	for _, s := range run.saves {
		if s.CloudAccountID == nil {
			continue
		}
		perAccount++
		require.NotNil(t, s.FirstDispatchAt, "per-account row %s must carry the stamp into its INSERT", s.ExecutionID)
		assert.True(t, s.FirstDispatchAt.Equal(at))
	}
	assert.GreaterOrEqual(t, perAccount, 2)
}

func TestExecuteAndFinalize_RedshiftStampErrorFailsBeforeSubmission(t *testing.T) {
	exec := &config.PurchaseExecution{
		ExecutionID: "rs-exec", IdempotencyKey: "rs-lineage", Status: "running",
		Recommendations: []config.RecommendationRecord{redshiftScopedRec("acct-A")},
	}
	run := runRedshiftFinalize(t, exec, nil, common.ServiceRedshift, func() (config.StampResult, error) {
		return config.StampResult{}, errors.New("db down")
	})

	require.Error(t, run.err)
	assert.Empty(t, run.opts, "no provider call after a failed stamp")
	assert.Equal(t, "failed", exec.Status)
	assert.True(t, exec.NoSubmission)
}

func TestIsRedshiftRec_MatcherTable(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		service  string
		want     bool
	}{
		{"redshift, empty provider", "", "redshift", true},
		{"redshift, aws", "aws", "redshift", true},
		{"data-warehouse, empty provider", "", "data-warehouse", true},
		{"data-warehouse, aws", "aws", "data-warehouse", true},
		{"data-warehouse, azure", "azure", "data-warehouse", false},
		{"redshift slug, gcp", "gcp", "redshift", false},
		{"ec2, aws", "aws", "ec2", false},
		{"unknown slug, aws", "aws", "no-such-service", false},
		{"empty service, aws", "aws", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := config.RecommendationRecord{Provider: tc.provider, Service: tc.service, Selected: true}
			assert.Equal(t, tc.want, isRedshiftRec(rec))
			exec := &config.PurchaseExecution{Recommendations: []config.RecommendationRecord{rec}}
			assert.Equal(t, tc.want, hasRedshiftRec(exec))
		})
	}

	t.Run("unselected redshift does not stamp", func(t *testing.T) {
		rec := config.RecommendationRecord{Provider: "aws", Service: "redshift", Selected: false}
		assert.False(t, hasRedshiftRec(&config.PurchaseExecution{Recommendations: []config.RecommendationRecord{rec}}))
	})
	t.Run("one selected redshift among others stamps", func(t *testing.T) {
		recs := []config.RecommendationRecord{
			{Provider: "aws", Service: "ec2", Selected: true},
			{Provider: "aws", Service: "redshift", Selected: false},
			{Provider: "aws", Service: "data-warehouse", Selected: true},
		}
		assert.True(t, hasRedshiftRec(&config.PurchaseExecution{Recommendations: recs}))
	})
}

func TestStampRedshiftDispatch_NilAtRefusalByAttempt(t *testing.T) {
	for _, tc := range []struct {
		attempt          int
		wantBeforeSubmit bool
	}{{0, true}, {1, false}} {
		store := new(MockConfigStore)
		store.On("StampFirstDispatchTx", mock.Anything, mock.Anything, "rs-x", mock.Anything).
			Return(config.StampResult{At: nil}, nil).Once()
		m := &Manager{config: store}
		exec := &config.PurchaseExecution{
			ExecutionID: "rs-x", RetryAttemptN: tc.attempt,
			Recommendations: []config.RecommendationRecord{redshiftScopedRec("acct-A")},
		}

		var err error
		require.NotPanics(t, func() { err = m.stampRedshiftDispatch(context.Background(), exec) })

		require.Error(t, err)
		var bs *beforeSubmissionError
		assert.Equal(t, tc.wantBeforeSubmit, errors.As(err, &bs), "attempt %d", tc.attempt)
		assert.Nil(t, exec.FirstDispatchAt)
		store.AssertNumberOfCalls(t, "StampFirstDispatchTx", 1)
	}
}

func TestExecuteAndFinalize_NonRedshiftNeverStamps(t *testing.T) {
	exec := &config.PurchaseExecution{
		ExecutionID: "ec2-exec", IdempotencyKey: "ec2-lineage", Status: "running",
		Recommendations: []config.RecommendationRecord{scopedTestRec("acct-A")},
	}
	before := time.Now()
	run := runRedshiftFinalize(t, exec, nil, common.ServiceEC2, nil)

	require.NoError(t, run.err)
	assert.Empty(t, run.stampedIDs)
	require.Len(t, run.opts, 1)
	assertRequestedAtStamped(t, []common.PurchaseOptions{run.opts[0], run.opts[0]}, before, time.Now())
}

func TestExecuteSinglePurchase_RedshiftWithoutRequestedAtRefuses(t *testing.T) {
	mockFactory := new(MockProviderFactory)
	mockProviderInst := new(MockProvider)
	mockServiceClient := new(MockServiceClient)
	mockFactory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(mockProviderInst, nil).Maybe()
	mockProviderInst.On("GetServiceClient", mock.Anything, common.ServiceRedshift, mock.Anything).Return(mockServiceClient, nil).Maybe()
	m := &Manager{providerFactory: mockFactory}

	_, err := m.executeSinglePurchase(context.Background(), redshiftScopedRec("acct-A"), nil, common.PurchaseOptions{ExecutionID: "e"})

	require.Error(t, err)
	mockServiceClient.AssertNotCalled(t, "PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything)
}
