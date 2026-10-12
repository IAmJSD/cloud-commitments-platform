package purchase

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// approveFromStatuses mirrors the value Manager.ApproveAndExecute passes to
// TransitionExecutionStatus. Centralized so a future change to the
// allowed-from set updates every test in one place.
var approveFromStatuses = []string{"pending", "notified"}

// newApproveManager wires a Manager backed by a fresh MockConfigStore +
// MockEmailSender. Returns both mocks so tests can register expectations
// per scenario without re-stating the boilerplate.
func newApproveManager(t *testing.T) (*Manager, *MockConfigStore, *MockEmailSender) {
	t.Helper()
	store := new(MockConfigStore)
	sender := new(MockEmailSender)

	// A plan step with no recommendations is refused (#609), so the
	// approved fixtures carry one recommendation; this factory lets it
	// purchase. Maybe(): tests that never reach execution register nothing.
	prov := new(MockProvider)
	svc := new(MockServiceClient)
	factory := new(MockProviderFactory)
	factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(prov, nil).Maybe()
	prov.On("GetServiceClient", mock.Anything, common.ServiceEC2, "us-east-1").Return(svc, nil).Maybe()
	svc.On("PurchaseCommitment", mock.Anything, mock.Anything, mock.Anything).
		Return(common.PurchaseResult{Success: true, CommitmentID: "ri-approval-test"}, nil).Maybe()

	return &Manager{
		config:          store,
		email:           sender,
		providerFactory: factory,
		dashboardURL:    "https://dashboard.example.com",
	}, store, sender
}

// approvalTestRecs is the one recommendation an approved plan step carries in
// these tests.
func approvalTestRecs() []config.RecommendationRecord {
	return []config.RecommendationRecord{{
		Provider:     "aws",
		Service:      "ec2",
		ResourceType: "m5.large",
		Region:       "us-east-1",
		Count:        1,
		Savings:      10,
		UpfrontCost:  50,
		Selected:     true,
	}}
}

// stubExecuteChain wires the mocks that Manager.executeAndFinalize touches
// for an approved execution carrying approvalTestRecs (no plan accounts).
// GetPlanAccounts uses the Fn-override pattern in this mock, not testify's
// .On; left at its nil default it returns (nil, nil) which drops us into the
// single-account branch.
func stubExecuteChain(t *testing.T, store *MockConfigStore, sender *MockEmailSender, planID string) {
	t.Helper()
	plan := &config.PurchasePlan{ID: planID, Name: "test-plan"}
	store.On("GetPurchasePlan", mock.Anything, planID).Return(plan, nil)
	store.On("SavePurchaseHistory", mock.Anything, mock.AnythingOfType("*config.PurchaseHistoryRecord")).Return(nil)
	sender.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil)
	// finalizeExecution writes status=completed via SavePurchaseExecution.
	store.On("SavePurchaseExecution", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution")).Return(nil)
	// updatePlanProgress calls CompletePlanStep (atomic, issue #1071); every
	// caller of this helper stamps StepNumber: 1 on its "updated" fixture.
	store.On("CompletePlanStep", mock.Anything, planID, 1).Return(nil)
}

func TestManager_ApproveExecution_Success(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-123",
		PlanID:        "plan-456",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-123",
		PlanID:          "plan-456",
		Status:          "approved",
		ApprovalToken:   config.HashApprovalToken("valid-token"),
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}

	store.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-123", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-456")

	_, err := manager.ApproveExecution(ctx, "exec-123", "valid-token", "")
	require.NoError(t, err)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

func TestManager_ApproveExecution_StampsApprovedBy(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-123",
		PlanID:        "plan-456",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-123",
		PlanID:          "plan-456",
		Status:          "approved",
		ApprovalToken:   config.HashApprovalToken("valid-token"),
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}

	store.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-123", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-456")

	_, err := manager.ApproveExecution(ctx, "exec-123", "valid-token", "operator@example.com")
	require.NoError(t, err)

	// Two SavePurchaseExecution calls land on the same pointer (the
	// transitioned row): once for the ApprovedBy stamp, once at the end
	// of executeAndFinalize. Both should reflect the actor.
	require.NotNil(t, updated.ApprovedBy)
	assert.Equal(t, "operator@example.com", *updated.ApprovedBy)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

func TestManager_ApproveExecution_NotifiedStatus(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-123",
		PlanID:        "plan-456",
		Status:        "notified",
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-123",
		PlanID:          "plan-456",
		Status:          "approved",
		ApprovalToken:   config.HashApprovalToken("valid-token"),
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}

	store.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-123", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-456")

	_, err := manager.ApproveExecution(ctx, "exec-123", "valid-token", "")
	require.NoError(t, err)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

func TestManager_ApproveExecution_InvalidToken(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-123",
		PlanID:        "plan-456",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}

	store.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)

	_, err := manager.ApproveExecution(ctx, "exec-123", "invalid-token", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid approval token")
	// Critically: TransitionExecutionStatus and SavePurchaseExecution must
	// NOT have been called. A token-validation bypass is exactly what the
	// constant-time comparison guards against.
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}

func TestManager_ApproveExecution_EmptyToken(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-123",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}
	store.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)

	_, err := manager.ApproveExecution(ctx, "exec-123", "", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid approval token")
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestManager_ApproveExecution_NotFound(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	store.On("GetExecutionByID", ctx, "exec-123").Return(nil, fmt.Errorf("%w: execution exec-123", config.ErrNotFound))

	_, err := manager.ApproveExecution(ctx, "exec-123", "token", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "execution not found")
	store.AssertExpectations(t)
}

func TestManager_ApproveExecution_GetError(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	store.On("GetExecutionByID", ctx, "exec-123").Return(nil, errors.New("database error"))

	_, err := manager.ApproveExecution(ctx, "exec-123", "token", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get execution")
	store.AssertExpectations(t)
}

func TestManager_ApproveExecution_TransitionFails(t *testing.T) {
	// Closes the race-with-cancel/expire path: token is valid but the row
	// drifted out of pending/notified between fetch and atomic UPDATE.
	// TransitionExecutionStatus 0-rows; the caller surfaces a clean error
	// and never enters the execute chain.
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-123",
		PlanID:        "plan-456",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}
	store.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-123", approveFromStatuses, "approved", (*string)(nil)).
		Return(nil, errors.New(`execution exec-123 cannot transition from "canceled" to "approved"`))

	_, err := manager.ApproveExecution(ctx, "exec-123", "valid-token", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot transition")
	// Execute chain must not run after a failed transition.
	store.AssertNotCalled(t, "GetPurchasePlan", mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}

// TestManager_ApproveAndExecute_EmptyPlanID is the regression guard for
// issue #395. Direct-execute purchases (Opportunities "Purchase" button)
// arrive with PlanID = "". Before the fix, executePurchase called
// GetPurchasePlan(ctx, "") which crashed with SQLSTATE 22P02 because the
// Postgres purchase_plans.id column is UUID. This test asserts the
// approve+execute path now short-circuits the plan/accounts fetch and the
// plan-progress update, lands the execution in a terminal state, and
// never calls GetPurchasePlan / GetPlanAccounts / UpdatePurchasePlan.
func TestManager_ApproveAndExecute_EmptyPlanID(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	updated := &config.PurchaseExecution{
		ExecutionID:   "exec-direct-1",
		PlanID:        "", // direct-execute shape — no associated plan
		Status:        "approved",
		ApprovalToken: "tok",
	}
	store.On("TransitionExecutionStatus", ctx, "exec-direct-1", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	// No GetPurchasePlan / GetPlanAccounts / UpdatePurchasePlan calls — see AssertNotCalled below.
	sender.On("SendPurchaseConfirmation", mock.Anything, mock.Anything).Return(nil)
	store.On("SavePurchaseExecution", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution")).Return(nil)

	_, _, err := manager.ApproveAndExecute(ctx, "exec-direct-1", "operator@example.com", nil)
	require.NoError(t, err)

	// Crucially, the empty PlanID must never reach the UUID-typed store columns.
	store.AssertNotCalled(t, "GetPurchasePlan", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "UpdatePurchasePlan", mock.Anything, mock.Anything)
	// GetPlanAccounts uses the Fn-override pattern in MockConfigStore; leaving
	// GetPlanAccountsFn nil means the production code path is the one under
	// test. The early-return on empty PlanID skips the call entirely.
	assert.Nil(t, store.GetPlanAccountsFn, "test sanity: empty-PlanID branch must not depend on GetPlanAccounts being stubbed")

	// Status reaches a terminal state — completed because no recs failed.
	assert.Equal(t, "completed", updated.Status)
	require.NotNil(t, updated.ApprovedBy)
	assert.Equal(t, "operator@example.com", *updated.ApprovedBy)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

func TestManager_ApproveAndExecute_SkipsTokenCheck(t *testing.T) {
	// Session-authed path: ApproveAndExecute is called directly without a
	// token, after the caller has run RBAC. Verifies the entry point works
	// independently of the token branch.
	//
	// Also the manager-level regression guard for issue #1009: a non-nil
	// transitionedBy (the session user's UUID) must be threaded into
	// TransitionExecutionStatus so transitioned_by is stamped for human
	// session approvals. Pre-fix ApproveAndExecute always passed nil here.
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	actorUUID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-456",
		PlanID:          "plan-789",
		Status:          "approved",
		ApprovalToken:   "tok",
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}
	store.On("TransitionExecutionStatus", ctx, "exec-456", approveFromStatuses, "approved",
		mock.MatchedBy(func(actor *string) bool { return actor != nil && *actor == actorUUID })).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-789")

	_, _, err := manager.ApproveAndExecute(ctx, "exec-456", "session-user@example.com", &actorUUID)
	require.NoError(t, err)
	require.NotNil(t, updated.ApprovedBy)
	assert.Equal(t, "session-user@example.com", *updated.ApprovedBy)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

// TestManager_RunPlannedPurchaseNow_ExecutesFromPaused pins issue #218:
// "Run now" on a scheduled purchase must actually execute it, not just flip
// its status. Pre-fix, the handler CASed the row straight to "running" with
// no executor consuming that state -- the scheduler only picks up
// pending/notified, so the purchase was never bought and the reaper
// eventually failed the row out from under the operator who was told it
// ran. This drives RunPlannedPurchaseNow from "paused" (a source
// ApproveAndExecute itself does not accept) and asserts the full
// executeAndFinalize chain runs: the plan lookup, the confirmation email,
// SavePurchaseExecution, and CompletePlanStep all fire, proving the
// purchase was actually executed rather than left stuck in "running".
func TestManager_RunPlannedPurchaseNow_ExecutesFromPaused(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-run-now",
		PlanID:          "plan-run-now",
		Status:          "approved",
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}
	store.On("TransitionExecutionStatus", ctx, "exec-run-now", []string{"pending", "notified", "paused"}, "approved", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-run-now")

	final, revocationToken, err := manager.RunPlannedPurchaseNow(ctx, "exec-run-now", "operator@example.com", nil)
	require.NoError(t, err)
	// Run-now shares the execute funnel's revocation-token rotation (#103).
	require.NotEmpty(t, revocationToken)
	assert.Equal(t, config.HashApprovalToken(revocationToken), updated.ApprovalToken)
	assert.NotEqual(t, revocationToken, updated.ApprovalToken)
	require.NotNil(t, updated.ApprovedBy)
	assert.Equal(t, "operator@example.com", *updated.ApprovedBy)
	assert.Same(t, updated, final)
	assert.Equal(t, "completed", final.Status)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

// TestManager_RunPlannedPurchaseNow_LostCASReturnsError proves a lost CAS
// (row already claimed -- by a concurrent "Run now" click, or the scheduler
// racing the same pending row) surfaces as an error rather than silently
// reporting success, and that no execution work runs on that path (no
// unexpected mock calls -- store has no execute-chain stubs registered).
// This is the idempotency half of issue #218's "no double purchase" ask.
func TestManager_RunPlannedPurchaseNow_LostCASReturnsError(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	store.On("TransitionExecutionStatus", ctx, "exec-race", []string{"pending", "notified", "paused"}, "approved", (*string)(nil)).
		Return(nil, config.ErrExecutionNotInExpectedStatus)

	final, _, err := manager.RunPlannedPurchaseNow(ctx, "exec-race", "operator@example.com", nil)
	require.Error(t, err)
	assert.Nil(t, final)
	assert.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
	store.AssertExpectations(t)
}

// TestManager_DirectExecute_StampsAuditFieldsAfterClaim pins #532: the
// executed_* audit fields land on the row the claim returned, together with
// the skip reason, and the row comes back to the caller.
func TestManager_DirectExecute_StampsAuditFieldsAfterClaim(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	actorUUID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-direct-stamp",
		PlanID:          "plan-direct-stamp",
		Status:          "approved",
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}
	store.On("TransitionExecutionStatus", ctx, "exec-direct-stamp", approveFromStatuses, "approved",
		mock.MatchedBy(func(actor *string) bool { return actor != nil && *actor == actorUUID })).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-direct-stamp")

	final, _, err := manager.DirectExecute(ctx, "exec-direct-stamp", "session-user@example.com", &actorUUID)
	require.NoError(t, err)
	assert.Same(t, updated, final)
	require.NotNil(t, final.ExecutedAt)
	require.NotNil(t, final.ExecutedByUserID)
	assert.Equal(t, actorUUID, *final.ExecutedByUserID)
	require.NotNil(t, final.PreApprovalSkipReason)
	assert.Equal(t, directExecuteSkipReason, *final.PreApprovalSkipReason)
}

// TestManager_DirectExecute_RefusedLeavesNoExecutedAt pins #532: a lost claim
// returns no row and saves nothing, so no executed_at can land on a row that
// never ran.
func TestManager_DirectExecute_RefusedLeavesNoExecutedAt(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	store.On("TransitionExecutionStatus", ctx, "exec-direct-lost", approveFromStatuses, "approved", (*string)(nil)).
		Return(nil, config.ErrExecutionNotInExpectedStatus)

	final, _, err := manager.DirectExecute(ctx, "exec-direct-lost", "operator@example.com", nil)
	require.ErrorIs(t, err, config.ErrExecutionNotInExpectedStatus)
	assert.Nil(t, final)
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
}

// TestManager_ApproveAndExecute_ReturnsRowOnExecutionFailure pins #532: after
// the claim, an execution failure still returns the row (with its final
// status) so the HTTP layer can tell it from a lost claim.
func TestManager_ApproveAndExecute_ReturnsRowOnExecutionFailure(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	factory := new(MockProviderFactory)
	factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(nil, errors.New("credentials rejected"))
	manager := &Manager{config: store, email: new(MockEmailSender), providerFactory: factory}

	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-fail-after-claim",
		Status:          "approved",
		Recommendations: approvalTestRecs(),
	}
	store.On("TransitionExecutionStatus", ctx, "exec-fail-after-claim", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	store.On("SavePurchaseExecution", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution")).Return(nil)

	final, token, err := manager.ApproveAndExecute(ctx, "exec-fail-after-claim", "operator@example.com", nil)
	require.Error(t, err)
	assert.Empty(t, token)
	require.NotNil(t, final)
	assert.Equal(t, "failed", final.Status)
}

// ─── enforceFourEyesPolicy at the ApproveAndExecute choke point ───────────────
// (issue #1005 / PR #1500 adversarial review)
//
// ApproveAndExecute is the ONE place every approve/execute entry point
// funnels through: approvePurchaseViaSession, directExecutePurchase
// (execute_mode="direct", issue #289), and ApproveExecution (both the
// email-token deep-link caller and the SQS async approve worker). The
// tests below exercise the manager-layer gate directly with the exact
// (actor, transitionedBy) shapes each real caller passes, proving the two
// bypasses adversarial review found on PR #1500 -- execute_mode="direct"
// (HIGH) and the SQS approve worker (MEDIUM) -- are now closed at the
// shared choke point rather than only at the 2 HTTP handler call sites the
// original PR wired requireDifferentApprover into.

func fourEyesCfgOnForManager() *config.GlobalConfig {
	return &config.GlobalConfig{RequireDifferentApprover: true}
}

func fourEyesManagerExec(executionID string, creatorID *string) *config.PurchaseExecution {
	return &config.PurchaseExecution{
		ExecutionID:     executionID,
		PlanID:          "plan-fourEyes",
		Status:          "pending",
		ApprovalToken:   "tok",
		CreatedByUserID: creatorID,
	}
}

// TestManager_ApproveAndExecute_FourEyesOn_DeniesSelfApprove models the
// execute_mode="direct" call shape (issue #289): directExecutePurchase always
// passes the session's own email as actor and its own UUID as transitionedBy,
// and by construction CreatedByUserID is stamped from that same session --
// so self-approval is unconditional on this path. Pre-fix, ApproveAndExecute
// had no 4-eyes awareness at all and this call would have proceeded straight
// to TransitionExecutionStatus and executed the purchase (the HIGH finding).
// The actor's UUID (transitionedBy) is compared directly against
// CreatedByUserID -- no GetUserEmailByID call is needed or expected on this
// tier-1 (UUID) path; see checkDifferentApprover's doc comment.
func TestManager_ApproveAndExecute_FourEyesOn_DeniesSelfApprove(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	creatorID := "user-creator"
	creatorEmail := "creator@example.com"
	execution := fourEyesManagerExec("exec-direct-self", &creatorID)

	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOnForManager(), nil)
	store.On("GetExecutionByID", ctx, "exec-direct-self").Return(execution, nil)

	_, _, err := manager.ApproveAndExecute(ctx, "exec-direct-self", creatorEmail, &creatorID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4-eyes mode requires a different approver")
	store.AssertNotCalled(t, "TransitionExecutionStatus",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "GetUserEmailByID", mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}

// TestManager_ApproveAndExecute_FourEyesOn_AllowsDifferentApprover is the
// positive control for the same call shape: a different actor than the
// creator must still succeed end to end (the fix must not over-broaden into
// blocking legitimate dual-control approvals). Compared by UUID (tier 1); no
// GetUserEmailByID call expected.
func TestManager_ApproveAndExecute_FourEyesOn_AllowsDifferentApprover(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	creatorID := "user-creator"
	approverUUID := "user-approver"
	approverEmail := "approver@example.com"
	execution := fourEyesManagerExec("exec-direct-diff", &creatorID)
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-direct-diff",
		PlanID:          "plan-fourEyes",
		Status:          "approved",
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}

	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOnForManager(), nil)
	store.On("GetExecutionByID", ctx, "exec-direct-diff").Return(execution, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-direct-diff", approveFromStatuses, "approved", &approverUUID).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-fourEyes")

	_, _, err := manager.ApproveAndExecute(ctx, "exec-direct-diff", approverEmail, &approverUUID)
	require.NoError(t, err)
	require.NotNil(t, updated.ApprovedBy)
	assert.Equal(t, approverEmail, *updated.ApprovedBy)
	store.AssertNotCalled(t, "GetUserEmailByID", mock.Anything, mock.Anything)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

// TestManager_ApproveAndExecute_FourEyesOn_PerUserAPIKey_DeniesSelfExecute
// closes the exact HIGH gap an independent adversarial review found in an
// earlier version of this fix: a per-user API key session
// (Session.UserAPIKeyID != "") carries a real user UUID as Session.UserID
// but an EMPTY Session.Email -- unlike a normal bearer-token session.
// internal/api's fourEyesActorIdentity falls back to session.UserID (a UUID
// string, not an email) whenever Email is empty; if checkDifferentApprover
// only ever compared emails, that UUID string could never equal the
// creator's real resolved email and self-direct-execute would silently
// succeed. Comparing UUIDs first (tier 1, via transitionedBy) closes this
// regardless of what the email-shaped actor string looks like.
func TestManager_ApproveAndExecute_FourEyesOn_PerUserAPIKey_DeniesSelfExecute(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	creatorID := "11111111-1111-1111-1111-111111111111"
	execution := fourEyesManagerExec("exec-apikey-self", &creatorID)

	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOnForManager(), nil)
	store.On("GetExecutionByID", ctx, "exec-apikey-self").Return(execution, nil)

	// actor is the UUID string itself (empty Session.Email fallback), exactly
	// what internal/api's fourEyesActorIdentity would pass for a per-user API
	// key session; transitionedBy is the same UUID, populated regardless of
	// Email by validUUIDPtrOrNil(&session.UserID).
	_, _, err := manager.ApproveAndExecute(ctx, "exec-apikey-self", creatorID, &creatorID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4-eyes mode requires a different approver")
	store.AssertNotCalled(t, "TransitionExecutionStatus",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "GetUserEmailByID", mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}

// TestManager_ApproveAndExecute_FourEyesOn_NullCreatorDenied: a legacy row
// with no recorded creator cannot satisfy "different from creator" and must
// fail closed, mirroring the handler-level requireDifferentApprover behavior
// for the same case.
func TestManager_ApproveAndExecute_FourEyesOn_NullCreatorDenied(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	execution := fourEyesManagerExec("exec-legacy", nil)
	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOnForManager(), nil)
	store.On("GetExecutionByID", ctx, "exec-legacy").Return(execution, nil)

	_, _, err := manager.ApproveAndExecute(ctx, "exec-legacy", "someone@example.com", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predates the dual-control feature")
	store.AssertExpectations(t)
}

// TestManager_ApproveAndExecute_FourEyesOn_EmptyActorDenied covers the
// "actor identity is unknown" fail-closed branch explicitly: mode is on, the
// execution has a recorded creator, but no actor identity was supplied.
func TestManager_ApproveAndExecute_FourEyesOn_EmptyActorDenied(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	creatorID := "user-creator"
	execution := fourEyesManagerExec("exec-no-actor", &creatorID)
	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOnForManager(), nil)
	store.On("GetExecutionByID", ctx, "exec-no-actor").Return(execution, nil)

	_, _, err := manager.ApproveAndExecute(ctx, "exec-no-actor", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no approver identity could be determined")
	store.AssertExpectations(t)
}

// TestManager_ApproveAndExecute_FourEyesOff_AllowsSelfApprove is the
// regression-continuity guard: mode off (the default) must behave exactly
// as before this fix -- the creator may approve/direct-execute their own
// row, and the FOUR-EYES GATE ITSELF performs no extra DB calls (it never
// calls GetUserEmailByID, and never calls GetExecutionByID for identity
// purposes since loadExecutionForFourEyes is skipped when mode is off).
// rotateApprovalToken (called unconditionally after a successful execute,
// issue #103) mutates the `updated` struct already in hand rather than
// re-fetching by ID, so it adds no extra GetExecutionByID call either.
func TestManager_ApproveAndExecute_FourEyesOff_AllowsSelfApprove(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	creatorID := "user-creator"
	updated := &config.PurchaseExecution{ExecutionID: "exec-mode-off", PlanID: "plan-fourEyes", Status: "approved", StepNumber: 1, Recommendations: approvalTestRecs()}
	store.On("TransitionExecutionStatus", ctx, "exec-mode-off", approveFromStatuses, "approved", &creatorID).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-fourEyes")

	_, _, err := manager.ApproveAndExecute(ctx, "exec-mode-off", "creator@example.com", &creatorID)
	require.NoError(t, err)
	store.AssertNotCalled(t, "GetExecutionByID", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "GetUserEmailByID", mock.Anything, mock.Anything)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

// TestManager_ApproveExecution_FourEyesOn_SQSActorEqualsCreator_Denied models
// the SQS async approve worker call shape (MEDIUM finding): ApproveExecution
// always passes transitionedBy=nil, and the only identity signal is the
// verified actor email. Pre-fix, ApproveExecution/ApproveAndExecute had no
// awareness of 4-eyes mode at all, so a replayed or forwarded token+actor
// matching the creator's own email would approve the row despite mode being
// on.
func TestManager_ApproveExecution_FourEyesOn_SQSActorEqualsCreator_Denied(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	creatorID := "user-creator"
	creatorEmail := "creator@example.com"
	execution := &config.PurchaseExecution{
		ExecutionID:     "exec-sqs-self",
		PlanID:          "plan-fourEyes",
		Status:          "pending",
		ApprovalToken:   config.HashApprovalToken("valid-token"),
		CreatedByUserID: &creatorID,
	}

	store.On("GetExecutionByID", ctx, "exec-sqs-self").Return(execution, nil)
	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOnForManager(), nil)
	store.On("GetUserEmailByID", ctx, creatorID).Return(creatorEmail, nil)

	_, err := manager.ApproveExecution(ctx, "exec-sqs-self", "valid-token", creatorEmail)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4-eyes mode requires a different approver")
	store.AssertNotCalled(t, "TransitionExecutionStatus",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}

// TestManager_ApproveExecution_FourEyesOn_DifferentActor_Allowed is the
// positive control for the SQS/token call shape: an actor that resolves to
// a different email than the creator must still succeed.
func TestManager_ApproveExecution_FourEyesOn_DifferentActor_Allowed(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	creatorID := "user-creator"
	execution := &config.PurchaseExecution{
		ExecutionID:     "exec-sqs-diff",
		PlanID:          "plan-fourEyes",
		Status:          "pending",
		ApprovalToken:   config.HashApprovalToken("valid-token"),
		CreatedByUserID: &creatorID,
	}
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-sqs-diff",
		PlanID:          "plan-fourEyes",
		Status:          "approved",
		ApprovalToken:   config.HashApprovalToken("valid-token"),
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}

	store.On("GetExecutionByID", ctx, "exec-sqs-diff").Return(execution, nil)
	store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOnForManager(), nil)
	store.On("GetUserEmailByID", ctx, creatorID).Return("creator@example.com", nil)
	store.On("TransitionExecutionStatus", ctx, "exec-sqs-diff", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-fourEyes")

	_, err := manager.ApproveExecution(ctx, "exec-sqs-diff", "valid-token", "approver@example.com")
	require.NoError(t, err)
	require.NotNil(t, updated.ApprovedBy)
	assert.Equal(t, "approver@example.com", *updated.ApprovedBy)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

// TestManager_ApproveExecution_FourEyesOn_UnresolvableCreator_Denied is the
// A16-001 regression guard (#256). checkDifferentApprover's tier-2 (email)
// path resolves the creator's email via GetUserEmailByID and must fail
// closed when that lookup cannot identify the creator -- either because the
// creator's row was deleted (Postgres's GetUserEmailByID returns ("", nil)
// on pgx.ErrNoRows, per internal/config/store_postgres.go) or because the
// lookup itself errors. Neither branch had a test: every stub in this
// package returned a real address, so a refactor that dropped or inverted
// the `creatorEmail == ""` check would silently let a forwarded token from
// the original requester clear dual control for any execution whose creator
// was deleted, without failing a single existing test.
func TestManager_ApproveExecution_FourEyesOn_UnresolvableCreator_Denied(t *testing.T) {
	creatorID := "user-creator-deleted"
	baseExecution := func() *config.PurchaseExecution {
		return &config.PurchaseExecution{
			ExecutionID:     "exec-sqs-unresolvable",
			PlanID:          "plan-fourEyes",
			Status:          "pending",
			ApprovalToken:   config.HashApprovalToken("valid-token"),
			CreatedByUserID: &creatorID,
		}
	}

	t.Run("GetUserEmailByID returns empty address (deleted creator row)", func(t *testing.T) {
		ctx := context.Background()
		manager, store, _ := newApproveManager(t)

		store.On("GetExecutionByID", ctx, "exec-sqs-unresolvable").Return(baseExecution(), nil)
		store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOnForManager(), nil)
		// Mirrors the real Postgres store's ("", nil) result for
		// pgx.ErrNoRows -- a deleted/nonexistent creator user row.
		store.On("GetUserEmailByID", ctx, creatorID).Return("", nil)

		_, err := manager.ApproveExecution(ctx, "exec-sqs-unresolvable", "valid-token", "approver@example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "creator's account could not be resolved")
		store.AssertNotCalled(t, "TransitionExecutionStatus",
			mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		store.AssertExpectations(t)
	})

	t.Run("GetUserEmailByID errors", func(t *testing.T) {
		ctx := context.Background()
		manager, store, _ := newApproveManager(t)
		lookupErr := errors.New("connection reset by peer")

		store.On("GetExecutionByID", ctx, "exec-sqs-unresolvable").Return(baseExecution(), nil)
		store.On("GetGlobalConfig", ctx).Return(fourEyesCfgOnForManager(), nil)
		store.On("GetUserEmailByID", ctx, creatorID).Return("", lookupErr)

		_, err := manager.ApproveExecution(ctx, "exec-sqs-unresolvable", "valid-token", "approver@example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to resolve creator identity")
		store.AssertNotCalled(t, "TransitionExecutionStatus",
			mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		store.AssertExpectations(t)
	})
}

func TestManager_CancelExecution(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-123",
		PlanID:        "plan-456",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}

	mockStore.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)
	// WithTx passes nil as the tx sentinel in tests; empty actor -> nil cancelledBy.
	mockStore.On("CancelExecutionAtomic", ctx, mock.Anything, "exec-123", (*string)(nil)).
		Return(true, "canceled", nil)
	mockStore.On("DeleteSuppressionsByExecutionTx", ctx, mock.Anything, "exec-123").
		Return(nil)
	// Token rotation: SavePurchaseExecution is called after a successful cancel.
	mockStore.On("SavePurchaseExecution", ctx, mock.MatchedBy(func(e *config.PurchaseExecution) bool {
		return e.ExecutionID == "exec-123" && e.ApprovalToken == ""
	})).Return(nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	err := manager.CancelExecution(ctx, "exec-123", "valid-token", "")
	require.NoError(t, err)

	mockStore.AssertExpectations(t)
}

func TestManager_CancelExecution_InvalidToken(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-123",
		PlanID:        "plan-456",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}

	mockStore.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	err := manager.CancelExecution(ctx, "exec-123", "invalid-token", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid approval token")

	mockStore.AssertExpectations(t)
}

func TestManager_CancelExecution_AlreadyCompleted(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-123",
		PlanID:        "plan-456",
		Status:        "completed",
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}

	mockStore.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	err := manager.CancelExecution(ctx, "exec-123", "valid-token", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "execution cannot be canceled")

	mockStore.AssertExpectations(t)
}

// TestManager_CancelExecution_RejectsNonCancelableStatus is the regression
// guard for issue #645: the token/email cancel path previously rejected only
// completed/canceled, so an email-link holder could cancel an
// approved/running/paused/failed/expired execution that the dashboard
// (session) path refuses. Each non-cancelable status must now be rejected with
// no write to the store — approved/running rows in particular are mid-execution
// and canceling them would desync the DB from the cloud. Pending, notified, and
// scheduled are cancelable; all other states are not.
func TestManager_CancelExecution_RejectsNonCancelableStatus(t *testing.T) {
	rejected := []string{"approved", "running", "paused", "failed", "expired", "completed", "canceled"}
	for _, status := range rejected {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			mockStore := new(MockConfigStore)
			mockEmail := new(MockEmailSender)

			execution := &config.PurchaseExecution{
				ExecutionID:   "exec-123",
				PlanID:        "plan-456",
				Status:        status,
				ApprovalToken: config.HashApprovalToken("valid-token"),
			}
			mockStore.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)

			manager := &Manager{
				config:       mockStore,
				email:        mockEmail,
				dashboardURL: "https://dashboard.example.com",
			}

			err := manager.CancelExecution(ctx, "exec-123", "valid-token", status)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "execution cannot be canceled")
			assert.Contains(t, err.Error(), status)
			// Status guard fires before the atomic UPDATE — a rejected
			// cancel must never reach CancelExecutionAtomic.
			mockStore.AssertNotCalled(t, "CancelExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			mockStore.AssertExpectations(t)
		})
	}
}

// TestManager_CancelExecution_AllowsCancelableStatus confirms the inverse of
// the #645 guard: pending, notified, and scheduled rows are all cancelable on
// the token path, and the cancel commits (status flip + suppression cleanup)
// in a single tx. Without this the alignment fix could silently over-restrict
// and break the legitimate email-link cancel of a row awaiting approval.
// "scheduled" is included because the cloud SDK has not been called yet (issue
// #291 wave-2) and IsCancelable already permits it.
func TestManager_CancelExecution_AllowsCancelableStatus(t *testing.T) {
	allowed := []string{"pending", "notified", "scheduled"}
	for _, status := range allowed {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			mockStore := new(MockConfigStore)
			mockEmail := new(MockEmailSender)

			execution := &config.PurchaseExecution{
				ExecutionID:   "exec-123",
				PlanID:        "plan-456",
				Status:        status,
				ApprovalToken: config.HashApprovalToken("valid-token"),
			}
			mockStore.On("GetExecutionByID", ctx, "exec-123").Return(execution, nil)
			// CancelExecutionAtomic is called inside WithTx (nil tx sentinel in tests).
			mockStore.On("CancelExecutionAtomic", ctx, mock.Anything, "exec-123", (*string)(nil)).
				Return(true, "canceled", nil)
			// Suppression cleanup must follow a successful atomic cancel.
			mockStore.On("DeleteSuppressionsByExecutionTx", ctx, mock.Anything, "exec-123").
				Return(nil)
			// Token rotation after successful cancel.
			mockStore.On("SavePurchaseExecution", ctx, mock.AnythingOfType("*config.PurchaseExecution")).Return(nil)

			manager := &Manager{
				config:       mockStore,
				email:        mockEmail,
				dashboardURL: "https://dashboard.example.com",
			}

			err := manager.CancelExecution(ctx, "exec-123", "valid-token", "")
			require.NoError(t, err)
			mockStore.AssertCalled(t, "CancelExecutionAtomic", ctx, mock.Anything, "exec-123", (*string)(nil))
			mockStore.AssertCalled(t, "DeleteSuppressionsByExecutionTx", ctx, mock.Anything, "exec-123")
			mockStore.AssertExpectations(t)
		})
	}
}

func TestManager_CancelExecution_NotFound(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	mockStore.On("GetExecutionByID", ctx, "exec-123").Return(nil, fmt.Errorf("%w: execution exec-123", config.ErrNotFound))

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	err := manager.CancelExecution(ctx, "exec-123", "token", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "execution not found")

	mockStore.AssertExpectations(t)
}

func TestManager_CancelExecution_GetError(t *testing.T) {
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	mockEmail := new(MockEmailSender)

	mockStore.On("GetExecutionByID", ctx, "exec-123").Return(nil, errors.New("database error"))

	manager := &Manager{
		config:       mockStore,
		email:        mockEmail,
		dashboardURL: "https://dashboard.example.com",
	}

	err := manager.CancelExecution(ctx, "exec-123", "token", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get execution")

	mockStore.AssertExpectations(t)
}

// --- Regression tests for issue #397 (token TTL enforcement) ---

// TestManager_ApproveExecution_ExpiredToken is the regression guard for
// issue #397: an approval token whose ApprovalTokenExpiresAt deadline has
// passed must be rejected with "expired", not silently accepted. This
// prevents a phished or log-leaked token from authorizing a purchase weeks
// after the approval window closed.
func TestManager_ApproveExecution_ExpiredToken(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	past := time.Now().Add(-1 * time.Hour) // expired 1 hour ago
	execution := &config.PurchaseExecution{
		ExecutionID:            "exec-expired",
		Status:                 "pending",
		ApprovalToken:          config.HashApprovalToken("valid-token"),
		ApprovalTokenExpiresAt: &past,
	}
	store.On("GetExecutionByID", ctx, "exec-expired").Return(execution, nil)

	_, err := manager.ApproveExecution(ctx, "exec-expired", "valid-token", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
	// Token validation passes but the expiry check fires — TransitionExecutionStatus
	// must never be called on an expired token.
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}

// TestManager_ApproveExecution_ValidTokenWithinTTL confirms that a
// non-expired token still works (regression guard for the inverse case).
func TestManager_ApproveExecution_ValidTokenWithinTTL(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	future := time.Now().Add(7 * 24 * time.Hour)
	execution := &config.PurchaseExecution{
		ExecutionID:            "exec-live",
		PlanID:                 "plan-live",
		Status:                 "pending",
		ApprovalToken:          config.HashApprovalToken("valid-token"),
		ApprovalTokenExpiresAt: &future,
	}
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-live",
		PlanID:          "plan-live",
		Status:          "approved",
		ApprovalToken:   config.HashApprovalToken("valid-token"),
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}
	store.On("GetExecutionByID", ctx, "exec-live").Return(execution, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-live", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-live")

	_, err := manager.ApproveExecution(ctx, "exec-live", "valid-token", "")
	require.NoError(t, err)
	store.AssertExpectations(t)
}

// TestManager_ApproveExecution_NilExpiresAt_LegacyRow verifies backward
// compatibility: rows created before migration 000051 have a nil
// ApprovalTokenExpiresAt and must not be rejected (issue #397 explicitly
// carves out legacy rows for compatibility).
func TestManager_ApproveExecution_NilExpiresAt_LegacyRow(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:            "exec-legacy",
		PlanID:                 "plan-legacy",
		Status:                 "pending",
		ApprovalToken:          config.HashApprovalToken("valid-token"),
		ApprovalTokenExpiresAt: nil, // pre-migration row
	}
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-legacy",
		PlanID:          "plan-legacy",
		Status:          "approved",
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}
	store.On("GetExecutionByID", ctx, "exec-legacy").Return(execution, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-legacy", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-legacy")

	_, err := manager.ApproveExecution(ctx, "exec-legacy", "valid-token", "")
	require.NoError(t, err)
	store.AssertExpectations(t)
}

// --- Regression tests for issue #609 (orphan-account guard, ApproveExecution path) ---

// TestManager_ApproveExecution_NonAWSOrphanReturnsError is the regression
// guard for issue #609 on the token-authenticated ApproveExecution path (used
// by the SQS approve worker and the legacy email-link flow). An execution
// whose CloudAccountID is nil and whose provider is non-AWS must return a
// descriptive error before the execute chain is entered, instead of surfacing
// an opaque cloud-SDK credential error at runtime.
func TestManager_ApproveExecution_NonAWSOrphanReturnsError(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-orphan",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("valid-token"),
		// CloudAccountID nil — account was deleted.
		Recommendations: []config.RecommendationRecord{{ID: "r1", Provider: "azure"}},
	}
	store.On("GetExecutionByID", ctx, "exec-orphan").Return(execution, nil)

	_, err := manager.ApproveExecution(ctx, "exec-orphan", "valid-token", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer exists")
	assert.Contains(t, err.Error(), "azure")
	// Execute chain must not run after the orphan guard fires.
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}

// TestManager_ApproveExecution_AWSOrphanFallsThrough confirms the AWS
// ambient-fallback carve-out: nil CloudAccountID + AWS provider must NOT be
// blocked by the issue-#609 guard. The execution proceeds normally.
func TestManager_ApproveExecution_AWSOrphanFallsThrough(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-aws-ambient",
		PlanID:        "plan-aws",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("valid-token"),
		// CloudAccountID nil but provider is AWS — ambient fallback applies.
		Recommendations: []config.RecommendationRecord{{ID: "r1", Provider: "aws"}},
	}
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-aws-ambient",
		PlanID:          "plan-aws",
		Status:          "approved",
		ApprovalToken:   config.HashApprovalToken("valid-token"),
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}
	store.On("GetExecutionByID", ctx, "exec-aws-ambient").Return(execution, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-aws-ambient", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-aws")

	_, err := manager.ApproveExecution(ctx, "exec-aws-ambient", "valid-token", "")
	require.NoError(t, err)
	store.AssertExpectations(t)
	sender.AssertExpectations(t)
}

// TestOrphanExecutionError_RecLevelAccountIDPreventsOrphan is the regression
// guard for CR pass-1 actionable 2: an execution with a nil execution-level
// CloudAccountID and a non-AWS provider must NOT be treated as an orphan when
// at least one recommendation carries its own non-nil CloudAccountID. The
// multi-rec fan-out path stores account affinity at the rec level and leaves
// the execution-level field nil.
func TestOrphanExecutionError_RecLevelAccountIDPreventsOrphan(t *testing.T) {
	accountID := "acct-azure-multi-001"
	execution := &config.PurchaseExecution{
		ExecutionID: "exec-multi-rec",
		// Execution-level CloudAccountID is nil — fan-out shape.
		Recommendations: []config.RecommendationRecord{
			{ID: "r1", Provider: "azure", CloudAccountID: &accountID},
			{ID: "r2", Provider: "azure", CloudAccountID: &accountID},
		},
	}
	// Despite execution-level nil + non-AWS provider, the rec-level account
	// ID means the execution is NOT orphaned. OrphanExecutionError must return nil.
	err := OrphanExecutionError(execution)
	assert.NoError(t, err, "rec-level CloudAccountID must prevent orphan classification")
}

// TestManager_CancelExecution_RaceWithApprove is the regression guard for
// issue #671: when a concurrent approve wins the race and transitions the
// execution to 'approved' before the cancel's conditional UPDATE runs,
// CancelExecutionAtomic returns (false, "approved", nil). CancelExecution
// must surface a clean error containing the racing status rather than
// silently overwriting the approved row.
func TestManager_CancelExecution_RaceWithApprove(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-raced",
		Status:        "pending", // status at load time
		ApprovalToken: config.HashApprovalToken("valid-token"),
	}
	store.On("GetExecutionByID", ctx, "exec-raced").Return(execution, nil)
	// Simulate: concurrent approve won between our IsCancelable check and
	// the atomic UPDATE. The DB row is now 'approved' so zero rows affected.
	store.On("CancelExecutionAtomic", ctx, mock.Anything, "exec-raced", (*string)(nil)).
		Return(false, "approved", nil)

	err := manager.CancelExecution(ctx, "exec-raced", "valid-token", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "approved", "error must surface the racing status")
	assert.Contains(t, err.Error(), "concurrent", "error must mention the concurrent operation")
	// The approve is already in flight — the DB row must not have been
	// overwritten by the cancel. AssertExpectations verifies that
	// CancelExecutionAtomic was called (confirming the guard reached the DB)
	// and that no further writes landed.
	store.AssertExpectations(t)
	// When the CAS misses, suppression cleanup must never fire.
	store.AssertNotCalled(t, "DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, mock.Anything)
}

// TestManager_CancelExecution_ExpiredToken is the cancel-path regression
// guard for issue #397.
func TestManager_CancelExecution_ExpiredToken(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	past := time.Now().Add(-2 * 24 * time.Hour)
	execution := &config.PurchaseExecution{
		ExecutionID:            "exec-cancel-expired",
		Status:                 "pending",
		ApprovalToken:          config.HashApprovalToken("valid-token"),
		ApprovalTokenExpiresAt: &past,
	}
	store.On("GetExecutionByID", ctx, "exec-cancel-expired").Return(execution, nil)

	err := manager.CancelExecution(ctx, "exec-cancel-expired", "valid-token", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
	store.AssertNotCalled(t, "CancelExecutionAtomic", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	store.AssertExpectations(t)
}

// ---------------------------------------------------------------------------
// Finding #1: Token rotation on approve/cancel
// ---------------------------------------------------------------------------

// TestApproveExecution_MintsRevocationToken verifies that after a successful
// approve a FRESH revocation token is minted (not cleared), so the consumed
// approval token can no longer trigger a revoke while the executed-notification
// email carries a valid revoke-capable token.
func TestApproveExecution_MintsRevocationToken(t *testing.T) {
	ctx := context.Background()
	manager, store, sender := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-rotate",
		PlanID:        "plan-rotate",
		Status:        "pending",
		ApprovalToken: config.HashApprovalToken("pre-rotate-token"),
	}
	updated := &config.PurchaseExecution{
		ExecutionID:     "exec-rotate",
		PlanID:          "plan-rotate",
		Status:          "approved",
		ApprovalToken:   config.HashApprovalToken("pre-rotate-token"),
		StepNumber:      1,
		Recommendations: approvalTestRecs(),
	}

	// GetExecutionByID is called once, in ApproveExecution itself, to
	// validate the supplied token against `execution`. rotateApprovalToken
	// (the rotation step) mutates `updated` -- the row TransitionExecutionStatus
	// returned and executeAndFinalize continued operating on -- in place; it
	// does no fetch of its own (see rotateApprovalToken's doc comment for why).
	store.On("GetExecutionByID", ctx, "exec-rotate").Return(execution, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-rotate", approveFromStatuses, "approved", (*string)(nil)).Return(updated, nil)
	stubExecuteChain(t, store, sender, "plan-rotate")

	_, err := manager.ApproveExecution(ctx, "exec-rotate", "pre-rotate-token", "")
	require.NoError(t, err)

	// After approve, rotateApprovalToken must have stamped a fresh token onto
	// `updated` (the post-transition row, NOT the pre-approve `execution`
	// struct, which is never touched again once its token has validated).
	// The old approval token's hash must be gone (proving the rotation
	// happened), and the expiry must be set to the 24-hour revocation window.
	// stubExecuteChain registers SavePurchaseExecution with AnythingOfType
	// so it absorbs both the finalize write and the rotateApprovalToken write.
	assert.NotEmpty(t, updated.ApprovalToken,
		"a fresh revocation token must be minted after successful approve (not cleared)")
	assert.NotEqual(t, config.HashApprovalToken("pre-rotate-token"), updated.ApprovalToken,
		"fresh revocation token must differ from the consumed approval token")
	assert.NotNil(t, updated.ApprovalTokenExpiresAt,
		"revocation token expiry must be set to the 24-hour window")
	// The pre-approve struct is untouched: it is a separate object from
	// `updated` and rotateApprovalToken never receives it.
	assert.Equal(t, config.HashApprovalToken("pre-rotate-token"), execution.ApprovalToken)
}

// TestClearApprovalToken_PersistsEmpty is a unit test for clearApprovalToken
// (the cancel path) that verifies the helper clears both ApprovalToken and
// ApprovalTokenExpiresAt on the persisted row. A canceled execution has no
// valid follow-up action, so emptying the token is correct.
func TestClearApprovalToken_PersistsEmpty(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	future := time.Now().Add(24 * time.Hour)
	execution := &config.PurchaseExecution{
		ExecutionID:            "exec-tok-clear",
		Status:                 "canceled",
		ApprovalToken:          "some-token",
		ApprovalTokenExpiresAt: &future,
	}
	store.On("GetExecutionByID", ctx, "exec-tok-clear").Return(execution, nil)
	store.On("SavePurchaseExecution", ctx, mock.MatchedBy(func(e *config.PurchaseExecution) bool {
		return e.ExecutionID == "exec-tok-clear" && e.ApprovalToken == "" && e.ApprovalTokenExpiresAt == nil
	})).Return(nil)

	err := manager.clearApprovalToken(ctx, "exec-tok-clear")
	require.NoError(t, err)
	store.AssertExpectations(t)
}

// TestMintRevocationToken_PersistsFreshToken is a unit test for
// rotateApprovalToken that verifies the helper writes a non-empty, changed
// token with a future expiry on the persisted row, mutating the caller's own
// struct in place (no internal fetch) and persisting it in a single save.
func TestMintRevocationToken_PersistsFreshToken(t *testing.T) {
	ctx := context.Background()
	manager, store, _ := newApproveManager(t)

	execution := &config.PurchaseExecution{
		ExecutionID:   "exec-mint",
		Status:        "completed",
		ApprovalToken: "old-approval-token",
	}
	store.On("SavePurchaseExecution", ctx, mock.MatchedBy(func(e *config.PurchaseExecution) bool {
		// Fresh token must be non-empty, different from old, and expiry set.
		return e.ExecutionID == "exec-mint" &&
			e.ApprovalToken != "" &&
			e.ApprovalToken != "old-approval-token" &&
			e.ApprovalTokenExpiresAt != nil
	})).Return(nil)

	rawToken, err := manager.rotateApprovalToken(ctx, execution, config.RevocationWindow)
	require.NoError(t, err)
	store.AssertExpectations(t)
	// The returned value is the RAW token (issue #103); the in-memory
	// struct's field was mutated to the HASH of a different, freshly
	// generated token, so the two must never be equal.
	assert.NotEmpty(t, rawToken)
	assert.NotEqual(t, rawToken, execution.ApprovalToken)
	assert.Equal(t, config.HashApprovalToken(rawToken), execution.ApprovalToken)
	assert.NotEmpty(t, execution.ApprovalToken)
	assert.NotEqual(t, "old-approval-token", execution.ApprovalToken)
	assert.NotNil(t, execution.ApprovalTokenExpiresAt)
}
