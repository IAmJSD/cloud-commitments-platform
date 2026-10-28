package purchase

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// --- MON-02: AWS EC2 and Redshift have no duplicate guard --------------
//
// The EC2 and Redshift purchase APIs take no client token or caller ID, so the
// provider sends them once and returns "purchase outcome unknown" when the
// response is lost. The commitment may exist, so nothing may buy again until
// someone has checked the account. These tests pin where that refusal reaches.

// awsNoTokenSlugs are the rec.Service spellings that dispatch to the AWS EC2 or
// Redshift purchase clients (mapServiceSlug: canonical and legacy).
var awsNoTokenSlugs = []string{"ec2", "compute", "redshift", "data-warehouse"}

// TestAWSRedriveGuardReachMatchesDispatchReach states the guard as an iff
// against dispatch, like the Azure savings-plans test: a value that dispatches
// to the EC2/Redshift client but is not refused is a double-buy hole.
func TestAWSRedriveGuardReachMatchesDispatchReach(t *testing.T) {
	m := NewManager(ManagerConfig{})

	candidates := []string{
		"ec2", "compute", "redshift", "data-warehouse",
		"rds", "relational-db", "elasticache", "cache", "opensearch", "search", "memorydb",
		"savings-plans", "savingsplans", "savings-plans-compute", "savingsplans-ec2instance",
		string(common.ServiceEC2), string(common.ServiceCompute), string(common.ServiceRedshift),
		string(common.ServiceDataWarehouse), string(common.ServiceRDS), string(common.ServiceNoSQL),
		"EC2", "Compute", " ec2", "ec2 ", "ec2\n", "Redshift", "data_warehouse", "", "unknown",
	}

	for _, provider := range []string{"aws", ""} {
		for _, service := range candidates {
			provider, service := provider, service
			t.Run("provider="+provider+"/service="+service, func(t *testing.T) {
				svc := m.mapServiceType(service)
				dispatchesToNoTokenClient := svc == common.ServiceEC2 || svc == common.ServiceCompute ||
					svc == common.ServiceRedshift || svc == common.ServiceDataWarehouse
				refused := recRedriveRefusalReason(config.RecommendationRecord{Provider: provider, Service: service}) != ""

				assert.Equal(t, dispatchesToNoTokenClient, refused,
					"guard reach and dispatch reach must match for %q: dispatches=%v refused=%v", service, dispatchesToNoTokenClient, refused)
			})
		}
	}
}

// TestAWSRedriveRefusalSetIsExact is the closed-set form, with negative
// controls so a predicate that refuses every AWS row (or every service named
// compute on any provider) fails here.
func TestAWSRedriveRefusalSetIsExact(t *testing.T) {
	for _, provider := range []string{"aws", ""} {
		for _, service := range awsNoTokenSlugs {
			reason := recRedriveRefusalReason(config.RecommendationRecord{Provider: provider, Service: service})
			assert.Contains(t, reason, "Check EC2 Reserved Instances or Redshift Reserved Nodes",
				"provider %q service %q has no duplicate guard and must be refused with the operator action", provider, service)
		}
		for _, service := range []string{"rds", "relational-db", "elasticache", "cache", "memorydb", "opensearch", "search", "savingsplans", "savings-plans"} {
			assert.Empty(t, recRedriveRefusalReason(config.RecommendationRecord{Provider: provider, Service: service}),
				"provider %q service %q has a provider-side duplicate guard and must stay retryable", provider, service)
		}
	}
	// Same slugs on other providers keep their own rules.
	for _, provider := range []string{"azure", "gcp"} {
		for _, service := range []string{"compute", "data-warehouse"} {
			assert.Empty(t, recRedriveRefusalReason(config.RecommendationRecord{Provider: provider, Service: service}),
				"%s %s is not an AWS no-token path and must stay retryable", provider, service)
		}
	}
	// Any one unsafe rec makes the whole execution unsafe.
	mixed := &config.PurchaseExecution{Recommendations: []config.RecommendationRecord{
		{Provider: "aws", Service: "rds"}, {Provider: "aws", Service: "redshift"},
	}}
	assert.NotEmpty(t, RedriveRefusalReason(mixed))
	assert.False(t, allRecsSafeToRedrive(&config.PurchaseExecution{ExecutionID: "x", Recommendations: mixed.Recommendations}))
}

// TestAWSNoTokenRetrySuccessorIsRefusedAndFirstBuyStillExecutes drives the real
// executor funnel: a retry successor (RetryAttemptN > 0) of an EC2/Redshift
// purchase buys nothing and is persisted as failed with the reason, while a
// first purchase of the same service still buys exactly once.
func TestAWSNoTokenRetrySuccessorIsRefusedAndFirstBuyStillExecutes(t *testing.T) {
	m := NewManager(ManagerConfig{})
	for _, service := range awsNoTokenSlugs {
		service := service
		t.Run(service, func(t *testing.T) {
			svcType := m.mapServiceType(service)

			retry := armedExecution("aws", service, 2, "approved")
			mgr, store, rec := armedHarness(t, svcType)
			var saved []config.PurchaseExecution
			store.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
				saved = append(saved, *e)
				return nil
			}
			store.On("GetExecutionByID", mock.Anything, retry.ExecutionID).Return(retry, nil).Maybe()
			expectClaim(store, retry, []string{"approved", "pending", "notified"})
			body, err := json.Marshal(AsyncMessage{Type: MessageTypeExecutePurchase, ExecutionID: retry.ExecutionID})
			require.NoError(t, err)
			_ = mgr.ProcessMessage(context.Background(), string(body))

			assert.Equal(t, 0, rec.count(), "a retry after a possibly-landed %s purchase must not buy again", service)
			require.NotEmpty(t, saved)
			final := saved[len(saved)-1]
			assert.Equal(t, "failed", final.Status)
			assert.Contains(t, final.Error, "Check EC2 Reserved Instances or Redshift Reserved Nodes")

			first := armedExecution("aws", service, 0, "approved")
			mgr, store, rec = armedHarness(t, svcType)
			store.On("GetExecutionByID", mock.Anything, first.ExecutionID).Return(first, nil).Maybe()
			expectClaim(store, first, []string{"approved", "pending", "notified"})
			require.NoError(t, mgr.ProcessMessage(context.Background(), string(body)))
			assert.Equal(t, 1, rec.count(), "a first %s purchase must still go through; the guard is for retries only", service)
		})
	}
}

// TestRecoverStrandedApprovals_EC2RowIsFailedNotRedriven pins the sweep: a
// stranded approved EC2 row may have bought before the run died, so the sweep
// must make zero provider calls and tell the operator to check the account.
func TestRecoverStrandedApprovals_EC2RowIsFailedNotRedriven(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	assertMocksOnCleanup(t, mockStore)
	mockFactory := new(MockProviderFactory) // any CreateAndValidateProvider call would panic: no expectation set

	stranded := config.PurchaseExecution{
		ExecutionID: "exec-ec2-stranded",
		PlanID:      "plan-ec2",
		Status:      "approved",
		Recommendations: []config.RecommendationRecord{
			{Provider: "aws", Service: "ec2", ResourceType: "m5.large", Region: "us-east-1", Count: 1, UpfrontCost: 200, Selected: true},
		},
	}
	failedRow := stranded
	failedRow.Status = "failed"
	mockStore.On("GetStaleApprovedExecutions", ctx, staleApprovedThreshold).Return([]config.PurchaseExecution{stranded}, nil)
	mockStore.On("TransitionExecutionStatus", ctx, "exec-ec2-stranded", []string{"approved"}, "failed", (*string)(nil)).Return(&failedRow, nil)
	var saved *config.PurchaseExecution
	mockStore.On("SavePurchaseExecution", ctx, mock.AnythingOfType("*config.PurchaseExecution")).
		Run(func(args mock.Arguments) { saved = args.Get(1).(*config.PurchaseExecution) }).Return(nil)

	mgr := &Manager{config: mockStore, providerFactory: mockFactory, dashboardURL: "https://dashboard.example.com"}
	recovered, err := mgr.RecoverStrandedApprovals(ctx)

	require.NoError(t, err)
	assert.Equal(t, 1, recovered)
	require.NotNil(t, saved)
	assert.Equal(t, "failed", saved.Status)
	assert.Contains(t, saved.Error, "start a new purchase")
	assert.NotContains(t, saved.Error, "then Retry", "Retry is refused for this row, so the message must not point at it")
	mockFactory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
}

// TestApproveAndExecuteRefusesAWSNoTokenRetrySuccessor pins the refusal on the
// approval entry points (issue #780). The ProcessMessage test above covers the
// SQS/cron executor; this drives the two ways an operator approves a pending
// row: the email-link token path (ApproveExecution, which calls
// ApproveAndExecute with no session UUID) and the session path
// (ApproveAndExecute with the session user's UUID as transitionedBy).
//
// The aws/rds case is the positive control: the same harness and entry point
// buy exactly once for a retry successor that has a provider-side duplicate
// guard, so a zero count on the refused rows means the guard fired, not that
// the harness never reached the provider.
func TestApproveAndExecuteRefusesAWSNoTokenRetrySuccessor(t *testing.T) {
	const rawToken = "approve-780"
	sessionUser := "55555555-6666-7777-8888-999999999780"

	paths := []struct {
		name           string
		transitionedBy *string
		pinnedToken    string // the token path pins its token inside the claim (issue #1190)
		approve        func(m *Manager, id string) error
	}{
		{"token", nil, rawToken, func(m *Manager, id string) error {
			_, _, err := m.ApproveExecution(context.Background(), id, rawToken, "approver@example.com", nil)
			return err
		}},
		{"session", &sessionUser, "", func(m *Manager, id string) error {
			_, _, err := m.ApproveAndExecute(context.Background(), id, "approver@example.com", &sessionUser, nil)
			return err
		}},
	}

	m := NewManager(ManagerConfig{})
	for _, path := range paths {
		for _, service := range append(append([]string{}, awsNoTokenSlugs...), "rds") {
			path, service := path, service
			refused := service != "rds"
			t.Run(path.name+"/"+service, func(t *testing.T) {
				exec := armedExecution("aws", service, 1, "pending")
				exec.ApprovalToken = config.HashApprovalToken(rawToken)
				mgr, store, rec := armedHarness(t, m.mapServiceType(service))
				var saved []config.PurchaseExecution
				store.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
					saved = append(saved, *e)
					return nil
				}
				store.On("GetExecutionByID", mock.Anything, exec.ExecutionID).Return(exec, nil).Maybe()
				claimed := *exec
				claimed.Status = "running"
				if path.pinnedToken != "" {
					store.On("TransitionExecutionStatusTokenPinned", mock.Anything, exec.ExecutionID,
						[]string{"pending", "notified"}, "running", path.transitionedBy, path.pinnedToken).Return(&claimed, nil).Once()
				} else {
					store.On("TransitionExecutionStatus", mock.Anything, exec.ExecutionID,
						[]string{"pending", "notified"}, "running", path.transitionedBy).Return(&claimed, nil).Once()
				}

				err := path.approve(mgr, exec.ExecutionID)

				if !refused {
					require.NoError(t, err)
					assert.Equal(t, 1, rec.count(), "positive control: an aws/rds retry successor must still buy once on the %s approve path", path.name)
					return
				}
				assert.Equal(t, 0, rec.count(),
					"approving a %s retry successor via the %s path must buy NOTHING; one call here may be a second commitment", service, path.name)
				require.Error(t, err, "the refusal must surface to the approver")
				assert.Contains(t, err.Error(), "refusing to execute retry attempt 1")
				require.NotEmpty(t, saved, "the refused row must be persisted, not left running")
				final := saved[len(saved)-1]
				assert.Equal(t, "failed", final.Status)
				assert.Contains(t, final.Error, "Check EC2 Reserved Instances or Redshift Reserved Nodes")
				store.AssertExpectations(t)
			})
		}
	}
}
