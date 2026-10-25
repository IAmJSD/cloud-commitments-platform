package server

import (
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleScheduledTask_UnknownTaskRejectedBeforeLock pins issue #111: a
// task name outside the roster (e.g. a typo passed to --task) is rejected
// before any advisory lock keyed on that name is taken, the same way the
// Lambda and HTTP transports reject it at their boundary.
func TestHandleScheduledTask_UnknownTaskRejectedBeforeLock(t *testing.T) {
	locker := &mockTaskLocker{acquired: true}
	app := &Application{
		Scheduler:  &testutil.MockScheduler{},
		Purchase:   &testutil.MockPurchaseManager{},
		TaskLocker: locker,
	}

	_, err := app.HandleScheduledTask(testutil.TestContext(t), ScheduledTaskType("collect_recomendations"), ScheduledTaskParams{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown scheduled task action: "collect_recomendations"`)
	assert.Zero(t, locker.lockCalls, "no advisory lock may be taken for an unknown task")
	assert.Zero(t, locker.unlockCalls)
}

// TestHandleScheduledTask_KnownTaskStillLocks guards the other direction: a
// valid task name still goes through the advisory lock.
func TestHandleScheduledTask_KnownTaskStillLocks(t *testing.T) {
	locker := &mockTaskLocker{acquired: false}
	app := &Application{TaskLocker: locker}

	res, err := app.HandleScheduledTask(testutil.TestContext(t), TaskSendNotifications, ScheduledTaskParams{})

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"status": "skipped", "reason": "already_running"}, res)
	assert.Equal(t, 1, locker.lockCalls)
}
