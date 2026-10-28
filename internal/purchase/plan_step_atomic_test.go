package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func stepTestPlan() *config.PurchasePlan {
	next := time.Now().Add(24 * time.Hour)
	return &config.PurchasePlan{ID: "plan-s", Enabled: true, AutoPurchase: true, RampSchedule: config.RampSchedule{CurrentStep: 1}, NextExecutionDate: &next}
}

func stepRow(status string, mutate func(*config.PurchaseExecution)) config.PurchaseExecution {
	e := config.PurchaseExecution{
		ExecutionID: "row-1", PlanID: "plan-s", StepNumber: 2, Status: status,
		Recommendations: []config.RecommendationRecord{scopedTestRec("acct-A")},
	}
	if mutate != nil {
		mutate(&e)
	}
	return e
}

// E1/E2: any existing row that is not a pending/notified ROOT blocks creation
// quietly, and nothing is written. Mutating adoptableRootRow to a "live status"
// filter makes the failed and partially_completed cases mint a second root.
func TestGetOrCreateExecution_ExistingNonAdoptableRowBlocks(t *testing.T) {
	acct := "acct-A"
	cases := map[string]config.PurchaseExecution{
		"failed root":              stepRow("failed", nil),
		"partially_completed root": stepRow("partially_completed", nil),
		"expired root":             stepRow("expired", nil),
		"canceled root":            stepRow("canceled", nil),
		"completed root":           stepRow("completed", nil),
		"approved root":            stepRow("approved", nil),
		"pending child":            stepRow("pending", func(e *config.PurchaseExecution) { e.CloudAccountID = &acct }),
		"pending retry":            stepRow("pending", func(e *config.PurchaseExecution) { e.RetryAttemptN = 1 }),
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			store := new(MockConfigStore)
			assertMocksOnCleanup(t, store)
			plan := stepTestPlan()
			expectStepRows(store, plan, row)
			m := &Manager{config: store}

			exec, _, _, err := m.getOrCreateExecution(context.Background(), plan)

			require.ErrorIs(t, err, errExecutionNotNotifiable)
			assert.Nil(t, exec)
			store.AssertNotCalled(t, "SavePurchaseExecution")
			store.AssertNotCalled(t, "SavePurchaseExecutionTx")
			store.AssertNotCalled(t, "TransitionExecutionStatus")
		})
	}
}

// A pending root with recommendations is adopted (not duplicated) and gets a
// fresh token to email.
func TestGetOrCreateExecution_AdoptsPendingRootOnly(t *testing.T) {
	acct := "acct-A"
	store := new(MockConfigStore)
	assertMocksOnCleanup(t, store)
	plan := stepTestPlan()
	child := stepRow("pending", func(e *config.PurchaseExecution) { e.ExecutionID = "child"; e.CloudAccountID = &acct })
	root := stepRow("notified", func(e *config.PurchaseExecution) { e.ExecutionID = "root" })
	expectStepRows(store, plan, child, root)
	m := &Manager{config: store}

	exec, tok, rotate, err := m.getOrCreateExecution(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, "root", exec.ExecutionID)
	assert.NotEmpty(t, tok)
	assert.True(t, rotate)
}

// The step is keyed on the locked plan's CurrentStep, not on the date.
func TestGetOrCreateExecution_KeysOnStepNotDate(t *testing.T) {
	store := new(MockConfigStore)
	assertMocksOnCleanup(t, store)
	plan := stepTestPlan()
	row := stepRow("pending", nil)
	row.ScheduledDate = time.Now().AddDate(0, 0, -30) // differs from NextExecutionDate
	expectStepRows(store, plan, row)
	m := &Manager{config: store}

	exec, _, _, err := m.getOrCreateExecution(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, "row-1", exec.ExecutionID, "a pre-created row with another date must be adopted, not duplicated")
}

func TestAttachResolvedRecommendations(t *testing.T) {
	recs := []config.RecommendationRecord{scopedTestRec("acct-A"), scopedTestRec("acct-B")}
	t.Run("attaches recs and writes one suppression per tuple in the same tx", func(t *testing.T) {
		store := new(MockConfigStore)
		assertMocksOnCleanup(t, store)
		store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)
		store.On("SetExecutionRecommendationsIfEmptyTx", mock.Anything, mock.Anything, "row-1", recs, 600.0, 150.0).Return(true, nil)
		store.On("CreateSuppressionTx", mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		exec := &config.PurchaseExecution{ExecutionID: "row-1"}

		ok, err := (&Manager{config: store}).AttachResolvedRecommendations(context.Background(), exec, recs)

		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, 600.0, exec.TotalUpfrontCost)
		assert.Len(t, exec.Recommendations, 2)
		store.AssertNumberOfCalls(t, "CreateSuppressionTx", 2)
	})
	t.Run("lost race writes no suppressions", func(t *testing.T) {
		store := new(MockConfigStore)
		store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)
		store.On("SetExecutionRecommendationsIfEmptyTx", mock.Anything, mock.Anything, "row-1", recs, 600.0, 150.0).Return(false, nil)
		exec := &config.PurchaseExecution{ExecutionID: "row-1"}

		ok, err := (&Manager{config: store}).AttachResolvedRecommendations(context.Background(), exec, recs)

		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, exec.Recommendations)
		store.AssertNotCalled(t, "CreateSuppressionTx")
	})
	t.Run("suppression failure surfaces", func(t *testing.T) {
		store := new(MockConfigStore)
		store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil)
		store.On("SetExecutionRecommendationsIfEmptyTx", mock.Anything, mock.Anything, "row-1", recs, 600.0, 150.0).Return(true, nil)
		store.On("CreateSuppressionTx", mock.Anything, mock.Anything, mock.Anything).Return(errors.New("boom"))
		exec := &config.PurchaseExecution{ExecutionID: "row-1"}

		ok, err := (&Manager{config: store}).AttachResolvedRecommendations(context.Background(), exec, recs)

		require.Error(t, err)
		assert.False(t, ok)
		assert.Empty(t, exec.Recommendations, "exec must stay untouched when the tx fails")
	})
	t.Run("empty set refused", func(t *testing.T) {
		_, err := (&Manager{config: new(MockConfigStore)}).AttachResolvedRecommendations(context.Background(), &config.PurchaseExecution{}, nil)
		require.Error(t, err)
	})
}

// E5: a plan row bound to an account refuses recs of another account, or with
// none, before any credential or provider work.
func TestExecuteSingleAccount_PlanRowRecsMustMatchRowAccount(t *testing.T) {
	rowAcct := "acct-A"
	for name, rec := range map[string]config.RecommendationRecord{
		"other account": scopedTestRec("acct-B"),
		"no account":    {Provider: "aws", Service: "ec2", ResourceType: "m5.large", Selected: true},
	} {
		t.Run(name, func(t *testing.T) {
			exec := &config.PurchaseExecution{ExecutionID: "x", PlanID: "plan-s", CloudAccountID: &rowAcct,
				Recommendations: []config.RecommendationRecord{rec}}
			err := (&Manager{config: new(MockConfigStore)}).executeSingleAccount(context.Background(), exec, &config.PurchasePlan{ID: "plan-s"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "is not attributed to it")
		})
	}
	t.Run("direct execute unchanged", func(t *testing.T) {
		exec := &config.PurchaseExecution{ExecutionID: "x", CloudAccountID: &rowAcct,
			Recommendations: []config.RecommendationRecord{scopedTestRec("acct-B")}}
		require.NoError(t, requireRecsMatchRowAccount(exec))
	})
}

// E3: a plan row releases its suppressions in the same tx as the terminal
// save ONLY when it failed before any purchase call was submitted. Outcome
// unknown, a refused armed re-drive, partial runs and direct executes keep them.
func TestSaveTerminalExecution_ReleasesSuppressionsOnlyBeforeSubmission(t *testing.T) {
	failedRow := func() config.PurchaseExecution {
		return config.PurchaseExecution{ExecutionID: "e", PlanID: "p", Status: "failed", Recommendations: []config.RecommendationRecord{scopedTestRec("acct-A")}}
	}
	bought := scopedTestRec("acct-A")
	bought.Purchased = true
	boughtRow := failedRow()
	boughtRow.Recommendations = []config.RecommendationRecord{bought}
	direct := failedRow()
	direct.PlanID = ""
	completed := failedRow()
	completed.Status = "completed"
	cases := []struct {
		name    string
		exec    config.PurchaseExecution
		err     error
		release bool
	}{
		{"failed before any provider call", failedRow(), beforeSubmission(errors.New("credential resolution failed")), true},
		{"outcome unknown (lost response) stays failed", failedRow(), errors.New("purchase outcome unknown: response lost"), false},
		{"refused armed re-drive", failedRow(), errors.New("refusing to execute retry attempt 1: unsafe to re-drive"), false},
		{"pre-submission error but a rec bought", boughtRow, beforeSubmission(errors.New("x")), false},
		{"direct execute", direct, beforeSubmission(errors.New("x")), false},
		{"completed", completed, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := new(MockConfigStore)
			assertMocksOnCleanup(t, store)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
			store.On("DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "e").Return(nil).Maybe()
			exec := c.exec

			require.NoError(t, (&Manager{config: store}).saveTerminalExecution(context.Background(), &exec, c.err))

			// The persisted no_submission proof follows the same condition as
			// the release, except it is also written for a direct execute.
			wantProof := c.release || c.name == "direct execute"
			assert.Equal(t, wantProof, exec.NoSubmission, "no_submission proof")

			if c.release {
				store.AssertCalled(t, "DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "e")
			} else {
				store.AssertNotCalled(t, "DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "e")
			}
		})
	}
}

// End to end through executeAndFinalize: a rec not attributed to the row's
// account is refused before the provider (released); a provider failure keeps
// the suppressions.
func TestExecuteAndFinalize_SuppressionReleaseFollowsSubmission(t *testing.T) {
	acct := "acct-A"
	run := func(rec config.RecommendationRecord) (*MockConfigStore, error) {
		store := new(MockConfigStore)
		assertMocksOnCleanup(t, store)
		store.On("GetPurchasePlan", mock.Anything, "p").Return(&config.PurchasePlan{ID: "p"}, nil)
		store.On("GetPlanAccounts", mock.Anything, "p").Return([]config.CloudAccount{}, nil).Maybe()
		store.On("GetCloudAccount", mock.Anything, acct).Return(nil, errors.New("account lookup failed")).Maybe()
		store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
		store.On("DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "e").Return(nil).Maybe()
		exec := &config.PurchaseExecution{ExecutionID: "e", PlanID: "p", CloudAccountID: &acct, Status: "running",
			Recommendations: []config.RecommendationRecord{rec}}
		return store, (&Manager{config: store}).executeAndFinalize(context.Background(), exec)
	}
	t.Run("rec for another account is refused before any call", func(t *testing.T) {
		store, err := run(scopedTestRec("acct-B"))
		require.Error(t, err)
		store.AssertCalled(t, "DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "e")
	})
	t.Run("credential failure before any call", func(t *testing.T) {
		store, err := run(scopedTestRec(acct))
		require.Error(t, err)
		store.AssertCalled(t, "DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "e")
	})
}

// Post-submission failures driven through the real executeAndFinalize path
// must KEEP the suppressions: the provider may have created a commitment.
// Marking any post-call error as before-submission makes these fail.
func TestExecuteAndFinalize_PostSubmissionFailureKeepsSuppressions(t *testing.T) {
	mk := func(types ...string) []config.RecommendationRecord {
		recs := make([]config.RecommendationRecord, 0, len(types))
		for _, rt := range types {
			recs = append(recs, config.RecommendationRecord{Provider: "aws", Service: "ec2", ResourceType: rt,
				Region: "us-east-1", Count: 1, UpfrontCost: 100, Selected: true})
		}
		return recs
	}
	cases := []struct {
		name    string
		recs    []config.RecommendationRecord
		results []error // PurchaseCommitment outcome per call, nil = success
	}{
		{"provider failure", mk("m5.large"), []error{errors.New("provider rejected the purchase")}},
		{"outcome unknown (lost response)", mk("m5.large"), []error{errors.New("purchase outcome unknown: response lost after submission")}},
		{"partial failure", mk("m5.large", "c5.large"), []error{nil, errors.New("provider rejected the purchase")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := new(MockConfigStore)
			factory := new(MockProviderFactory)
			prov := new(MockProvider)
			assertMocksOnCleanup(t, store, factory, prov)
			svc := new(MockServiceClient)
			stsMock := new(MockSTSClient)
			store.On("GetPurchasePlan", mock.Anything, "p").Return(&config.PurchasePlan{ID: "p"}, nil)
			store.On("GetPlanAccounts", mock.Anything, "p").Return([]config.CloudAccount{}, nil)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
			store.On("SavePurchaseHistory", mock.Anything, mock.Anything).Return(nil).Maybe()
			store.On("DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "e").Return(nil).Maybe()
			email := allowFailedNotification(new(MockEmailSender))
			email.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil).Maybe()
			stsMock.On("GetCallerIdentity", mock.Anything, mock.Anything).Return(&sts.GetCallerIdentityOutput{Account: aws.String("123456789012")}, nil).Maybe()
			factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(prov, nil)
			prov.On("GetServiceClient", mock.Anything, mock.Anything, "us-east-1").Return(svc, nil)
			for _, e := range c.results {
				res := common.PurchaseResult{Success: e == nil, CommitmentID: "ri-1"}
				svc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).Return(res, e).Once()
			}
			exec := &config.PurchaseExecution{ExecutionID: "e", PlanID: "p", Status: "running", Recommendations: c.recs}
			m := &Manager{config: store, email: email, stsClient: stsMock, providerFactory: factory}

			_ = m.executeAndFinalize(context.Background(), exec)

			store.AssertNotCalled(t, "DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "e")
			svc.AssertExpectations(t)
		})
	}
}
