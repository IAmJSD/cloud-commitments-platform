package purchase

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// defaultProcessSchedule is the process_scheduled_purchases sweep cadence the
// terraform modules default to ("rate(1 hour)"). RecoverStrandedApprovals runs
// on that sweep, so a stranded approval may wait this long past
// staleApprovedThreshold before recovery sees it.
const defaultProcessSchedule = time.Hour

// listedThresholds runs one reaper sweep against a store that returns no rows
// and records the age threshold each ListStuckExecutions call used per status.
func listedThresholds(t *testing.T, reapAfter time.Duration) map[string]time.Duration {
	t.Helper()
	ctx := context.Background()
	store := new(MockConfigStore)
	got := map[string]time.Duration{}
	store.On("ListStuckExecutions", ctx, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			for _, s := range args.Get(1).([]string) {
				got[s] = args.Get(2).(time.Duration)
			}
		}).
		Return([]config.PurchaseExecution{}, nil)

	_, err := newReaperManager(store).ReapStuckExecutions(ctx, reapAfter)
	require.NoError(t, err)
	return got
}

// Issue #894: with the default thresholds the reaper must not list approved
// rows until RecoverStrandedApprovals has had a full process sweep past
// staleApprovedThreshold to re-drive them. Before the fix approved rows were
// listed at reapAfter (10m) and failed before recovery could see them (15m).
func TestReapStuckExecutions_ApprovedRowsWaitForStrandedRecovery(t *testing.T) {
	got := listedThresholds(t, DefaultReapAfter)

	require.Contains(t, got, "approved", "the reaper must still back-stop approved rows")
	assert.Greater(t, got["approved"], staleApprovedThreshold+defaultProcessSchedule,
		"approved rows must outlive the stranded-approval recovery window")
	require.Contains(t, got, "running")
	assert.Equal(t, DefaultReapAfter, got["running"],
		"running rows are not re-driven by recovery and keep the configured threshold")
}

// A reapAfter configured below the recovery window (PURCHASE_APPROVED_REAP_AFTER)
// must not reopen the race for approved rows.
func TestReapStuckExecutions_ShortReapAfterDoesNotBeatRecovery(t *testing.T) {
	got := listedThresholds(t, 2*time.Minute)

	assert.Greater(t, got["approved"], staleApprovedThreshold+defaultProcessSchedule)
	assert.Equal(t, 2*time.Minute, got["running"])
}

// A reapAfter configured above the recovery window is honored as is for both
// statuses: the floor only ever delays the approved pass, never hastens it.
func TestReapStuckExecutions_LongReapAfterIsHonoredForApproved(t *testing.T) {
	long := 3 * time.Hour
	got := listedThresholds(t, long)

	assert.Equal(t, long, got["approved"])
	assert.Equal(t, long, got["running"])
}

// The approved and running passes together must cover exactly stuckStatuses,
// so splitting the sweep neither drops a status nor widens it.
func TestReapStuckExecutions_PassesCoverStuckStatuses(t *testing.T) {
	got := listedThresholds(t, DefaultReapAfter)

	listed := slices.Sorted(maps.Keys(got))
	want := slices.Clone(stuckStatuses)
	slices.Sort(want)
	assert.Equal(t, want, listed)
}

// The per-row reap annotation reports the threshold of the pass that listed
// the row, so an approved row is not described as stuck for only 10 minutes.
func TestReapStuckExecutions_ApprovedAnnotationUsesApprovedThreshold(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	row := stuckExec("exec-old-approved", "approved")
	flipped := row
	flipped.Status = failedStatus
	expectStuckLists(ctx, store, DefaultReapAfter, row)
	store.On("TransitionExecutionStatus", ctx, row.ExecutionID, []string{"approved"}, failedStatus, (*string)(nil)).
		Return(&flipped, nil)
	var saved *config.PurchaseExecution
	store.On("SavePurchaseExecution", ctx, mock.AnythingOfType("*config.PurchaseExecution")).
		Run(func(args mock.Arguments) { saved = args.Get(1).(*config.PurchaseExecution) }).Return(nil)

	result, err := newReaperManager(store).ReapStuckExecutions(ctx, DefaultReapAfter)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Reaped)
	require.NotNil(t, saved)
	wantMinutes := int(approvedReapAfter(DefaultReapAfter) / time.Minute)
	assert.Contains(t, saved.Error, fmt.Sprintf("reaped after %dm in approved state", wantMinutes))
	store.AssertExpectations(t)
}

// expectStuckLists stubs both reaper passes: running rows listed at reapAfter
// and approved rows at approvedReapAfter(reapAfter). Rows are routed to the
// pass matching their status.
func expectStuckLists(ctx context.Context, store *MockConfigStore, reapAfter time.Duration, rows ...config.PurchaseExecution) {
	byStatus := map[string][]config.PurchaseExecution{"running": {}, "approved": {}}
	for _, r := range rows {
		byStatus[r.Status] = append(byStatus[r.Status], r)
	}
	store.On("ListStuckExecutions", ctx, []string{"running"}, reapAfter).Return(byStatus["running"], nil)
	store.On("ListStuckExecutions", ctx, []string{"approved"}, approvedReapAfter(reapAfter)).Return(byStatus["approved"], nil)
}

// RecoverStrandedApprovals can claim a listed approved row (approved -> running)
// between the reaper's SELECT and its CAS. The CAS must be pinned to the status
// the row was listed under, or it matches "running" and fails a purchase that is
// in flight. The fake store models the row as already running.
func TestReapStuckExecutions_ApprovedRowClaimedBeforeCASIsNotFailed(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	row := stuckExec("exec-claimed", "approved")
	flipped := row
	flipped.Status = failedStatus
	expectStuckLists(ctx, store, DefaultReapAfter, row)
	store.On("TransitionExecutionStatus", ctx, row.ExecutionID,
		mock.MatchedBy(func(from []string) bool { return slices.Contains(from, "running") }),
		failedStatus, (*string)(nil)).Return(&flipped, nil)
	store.On("TransitionExecutionStatus", ctx, row.ExecutionID,
		mock.MatchedBy(func(from []string) bool { return !slices.Contains(from, "running") }),
		failedStatus, (*string)(nil)).
		Return(nil, fmt.Errorf("%w: from %q", config.ErrExecutionNotInExpectedStatus, "running"))

	result, err := newReaperManager(store).ReapStuckExecutions(ctx, DefaultReapAfter)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Found)
	assert.Equal(t, 0, result.Reaped, "an in-flight row must not be failed")
	assert.Equal(t, 1, result.RaceLost)
	assert.Zero(t, result.Errored)
	store.AssertNotCalled(t, "SaveExecutionIfStatus", mock.Anything, mock.Anything, mock.Anything)
}
