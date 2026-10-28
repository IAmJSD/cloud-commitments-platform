package purchase

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Issue #415: the handler checks permission Constraints against a row read
// before the claim CAS, but the funnel buys the row the CAS returns. These
// tests reprice the row between the two (the CAS mock returns the repriced
// copy) and assert, per entry point, that the ClaimedRowCheck sees the
// claimed row, refuses it before any provider call, and leaves it failed with
// no_submission set. Mock store and recording provider only; nothing here
// reaches a cloud API.

const (
	claimCheckCap      = 50000.0
	claimCheckPrice    = 30000.0 // under the cap: what the handler checked
	claimCheckRepriced = 90000.0 // over the cap: what the CAS returned
	claimCheckToken    = "claimed-row-check-token"
)

var errClaimCheckOverCap = errors.New("permission denied: over MaxPurchaseAmount")

// capCheck is a ClaimedRowCheck modeling a MaxPurchaseAmount cap. It records
// every row it was handed so a test can prove it saw the claimed row.
type capCheck struct {
	max  float64
	err  error // returned instead of the cap decision when set
	seen []config.PurchaseExecution
}

func (c *capCheck) check(_ context.Context, claimed *config.PurchaseExecution) error {
	c.seen = append(c.seen, *claimed)
	if c.err != nil {
		return c.err
	}
	var total float64
	for i := range claimed.Recommendations {
		total += claimed.Recommendations[i].TotalCommitment()
	}
	if total > c.max {
		return fmt.Errorf("%w: %.2f > %.2f", errClaimCheckOverCap, total, c.max)
	}
	return nil
}

// claimEntryPoint drives one funnel entry point; row is nil where the entry
// point returns none (the token approve).
type claimEntryPoint struct {
	name string
	from []string
	run  func(m *Manager, id string, check ClaimedRowCheck) (*config.PurchaseExecution, error)
	// pinnedToken is the approval token the entry point pins inside its claim
	// (the token approve, issue #1190); "" for the unpinned entry points.
	pinnedToken string
}

func claimEntryPoints() []claimEntryPoint {
	ctx := context.Background()
	actor := "operator@example.com"
	return []claimEntryPoint{
		{"run-now", runNowFrom, func(m *Manager, id string, c ClaimedRowCheck) (*config.PurchaseExecution, error) {
			row, _, err := m.RunPlannedPurchaseNow(ctx, id, actor, nil, c)
			return row, err
		}, ""},
		{"session approve", []string{"pending", "notified"}, func(m *Manager, id string, c ClaimedRowCheck) (*config.PurchaseExecution, error) {
			row, _, err := m.ApproveAndExecute(ctx, id, actor, nil, c)
			return row, err
		}, ""},
		{"direct execute", []string{"pending", "notified"}, func(m *Manager, id string, c ClaimedRowCheck) (*config.PurchaseExecution, error) {
			row, _, err := m.DirectExecute(ctx, id, actor, nil, c)
			return row, err
		}, ""},
		{"token approve", []string{"pending", "notified"}, func(m *Manager, id string, c ClaimedRowCheck) (*config.PurchaseExecution, error) {
			_, _, err := m.ApproveExecution(ctx, id, claimCheckToken, actor, c)
			return nil, err
		}, claimCheckToken},
	}
}

// claimCheckHarness wires a pending AWS RDS row (no plan) that GetExecutionByID
// returns at claimCheckPrice and the claim CAS returns at claimedPrice. saved
// collects every persisted copy of the row.
func claimCheckHarness(t *testing.T, ep claimEntryPoint, claimedPrice float64) (*Manager, *MockConfigStore, *armedPurchaseRecorder, *config.PurchaseExecution, *[]config.PurchaseExecution) {
	t.Helper()
	snapshot := armedExecution("aws", "rds", 0, "pending")
	snapshot.PlanID = ""
	snapshot.Recommendations[0].UpfrontCost = claimCheckPrice
	snapshot.ApprovalToken = config.HashApprovalToken(claimCheckToken)
	mgr, store, rec := armedHarness(t, common.ServiceRDS)
	saved := &[]config.PurchaseExecution{}
	store.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
		*saved = append(*saved, *e)
		return nil
	}
	store.On("GetExecutionByID", mock.Anything, snapshot.ExecutionID).Return(snapshot, nil).Maybe()
	claimed := *snapshot
	claimed.Recommendations = append([]config.RecommendationRecord(nil), snapshot.Recommendations...)
	claimed.Recommendations[0].UpfrontCost = claimedPrice
	claimed.Status = "running"
	if ep.pinnedToken != "" {
		store.On("TransitionExecutionStatusTokenPinned", mock.Anything, snapshot.ExecutionID, ep.from, "running", (*string)(nil), ep.pinnedToken).
			Return(&claimed, nil).Once()
	} else {
		store.On("TransitionExecutionStatus", mock.Anything, snapshot.ExecutionID, ep.from, "running", (*string)(nil)).
			Return(&claimed, nil).Once()
	}
	return mgr, store, rec, snapshot, saved
}

func lastSaved(t *testing.T, saved []config.PurchaseExecution) config.PurchaseExecution {
	t.Helper()
	require.NotEmpty(t, saved, "the refused row must be persisted, not left running for the reaper")
	return saved[len(saved)-1]
}

// The regression: the row is repriced over the cap between the handler's
// check and the CAS. Before the fix the funnel bought the repriced row.
func TestClaimedRowCheck_RepricedBetweenCheckAndClaimIsRefused(t *testing.T) {
	for _, ep := range claimEntryPoints() {
		t.Run(ep.name, func(t *testing.T) {
			mgr, _, rec, snapshot, saved := claimCheckHarness(t, ep, claimCheckRepriced)
			cc := &capCheck{max: claimCheckCap}

			row, err := ep.run(mgr, snapshot.ExecutionID, cc.check)

			assert.Zero(t, rec.count(), "the repriced row must not reach the provider")
			require.ErrorIs(t, err, ErrClaimedRowRefused)
			require.ErrorIs(t, err, errClaimCheckOverCap, "the check's own error must stay reachable for the HTTP mapping")
			require.Len(t, cc.seen, 1)
			assert.Equal(t, claimCheckRepriced, cc.seen[0].Recommendations[0].UpfrontCost,
				"the check must run on the row the CAS returned, not on the pre-CAS snapshot")
			got := lastSaved(t, *saved)
			assert.Equal(t, "failed", got.Status)
			assert.True(t, got.NoSubmission, "a refusal before any provider call is proven not submitted")
			assert.Contains(t, got.Error, ErrClaimedRowRefused.Error())
			assert.Nil(t, got.ApprovedBy, "a refused run is not an approval")
			assert.Nil(t, got.ExecutedAt, "a refused run never executed")
			assert.Nil(t, got.PreApprovalSkipReason)
			if row != nil {
				assert.Equal(t, "failed", row.Status)
			}
		})
	}
}

// Negative control: an unmodified row passes the same check and is bought
// once. A check that refused everything would fail here.
func TestClaimedRowCheck_UnmodifiedRowStillPurchases(t *testing.T) {
	for _, ep := range claimEntryPoints() {
		t.Run(ep.name, func(t *testing.T) {
			mgr, _, rec, snapshot, saved := claimCheckHarness(t, ep, claimCheckPrice)
			cc := &capCheck{max: claimCheckCap}

			_, err := ep.run(mgr, snapshot.ExecutionID, cc.check)

			require.NoError(t, err)
			assert.Equal(t, 1, rec.count())
			require.Len(t, cc.seen, 1, "the check runs exactly once, after the claim")
			assert.Equal(t, "running", cc.seen[0].Status, "the check runs on the claimed row")
			assert.Equal(t, "completed", lastSaved(t, *saved).Status)
		})
	}
}

// A check that cannot be evaluated (an auth backend error) fails closed.
func TestClaimedRowCheck_CheckErrorFailsClosed(t *testing.T) {
	ep := claimEntryPoints()[0]
	mgr, _, rec, snapshot, saved := claimCheckHarness(t, ep, claimCheckPrice)
	backendErr := errors.New("permission check failed: auth store unavailable")
	cc := &capCheck{max: claimCheckCap, err: backendErr}

	_, err := ep.run(mgr, snapshot.ExecutionID, cc.check)

	assert.Zero(t, rec.count())
	require.ErrorIs(t, err, ErrClaimedRowRefused)
	require.ErrorIs(t, err, backendErr)
	got := lastSaved(t, *saved)
	assert.Equal(t, "failed", got.Status)
	assert.True(t, got.NoSubmission)
}

// When the failed row cannot be saved, the refusal still holds and the caller
// learns the row is stranded "running" for the reaper.
func TestClaimedRowCheck_TerminalSaveFailureIsAuditLoss(t *testing.T) {
	ep := claimEntryPoints()[0]
	mgr, store, rec, snapshot, _ := claimCheckHarness(t, ep, claimCheckRepriced)
	assertMocksOnCleanup(t, store)
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, executorOwnedStatuses).Return(errors.New("db down"))
	cc := &capCheck{max: claimCheckCap}

	_, err := ep.run(mgr, snapshot.ExecutionID, cc.check)

	assert.Zero(t, rec.count())
	require.ErrorIs(t, err, config.ErrAuditLoss)
	require.ErrorIs(t, err, ErrClaimedRowRefused)
}

// A plan row refused after the claim releases its suppressions with the
// failed save, like every other before-submission refusal.
func TestClaimedRowCheck_PlanRowReleasesSuppressions(t *testing.T) {
	ep := claimEntryPoints()[0]
	mgr, store, rec, snapshot, saved := claimCheckHarness(t, ep, claimCheckRepriced)
	assertMocksOnCleanup(t, store)
	// Re-register the claim for a plan row: same row, PlanID set.
	planRow := *snapshot
	planRow.PlanID = armedPlanID
	store.ExpectedCalls = nil
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	store.On("GetExecutionByID", mock.Anything, planRow.ExecutionID).Return(&planRow, nil).Maybe()
	claimed := planRow
	claimed.Recommendations = append([]config.RecommendationRecord(nil), planRow.Recommendations...)
	claimed.Recommendations[0].UpfrontCost = claimCheckRepriced
	claimed.Status = "running"
	store.On("TransitionExecutionStatus", mock.Anything, planRow.ExecutionID, ep.from, "running", (*string)(nil)).
		Return(&claimed, nil).Once()
	cc := &capCheck{max: claimCheckCap}

	_, err := ep.run(mgr, planRow.ExecutionID, cc.check)

	require.ErrorIs(t, err, ErrClaimedRowRefused)
	assert.Zero(t, rec.count())
	assert.Equal(t, "failed", lastSaved(t, *saved).Status)
	store.AssertCalled(t, "DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, planRow.ExecutionID)
}

// A nil check (the SQS approve worker, which has no session Constraints)
// keeps the previous behavior.
func TestClaimedRowCheck_NilCheckSkipsRecheck(t *testing.T) {
	ep := claimEntryPoints()[3]
	mgr, _, rec, snapshot, _ := claimCheckHarness(t, ep, claimCheckRepriced)

	_, err := ep.run(mgr, snapshot.ExecutionID, nil)

	require.NoError(t, err)
	assert.Equal(t, 1, rec.count())
}
