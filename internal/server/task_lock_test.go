package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/mocks"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
	"github.com/stretchr/testify/mock"
)

// TestHandleScheduledTaskStuckLock pins issue #394 item 4 (issue #105's third
// ask): an "already_running" skip whose lock has been held longer than any run
// can last must surface as an error, not as a 200 skip, so a wedged lock is
// visible to the scheduler and to alarms. A lock held for a plausible run
// duration is still a normal skip.
func TestHandleScheduledTaskStuckLock(t *testing.T) {
	skipCases := []struct {
		locker *mockTaskLocker
		name   string
	}{
		{name: "held under the threshold", locker: &mockTaskLocker{heldFound: true, heldFor: 5 * time.Minute}},
		{name: "held exactly the threshold", locker: &mockTaskLocker{heldFound: true, heldFor: stuckTaskLockThreshold}},
		{name: "released before the age check", locker: &mockTaskLocker{heldFor: 2 * stuckTaskLockThreshold}},
		{name: "age check fails", locker: &mockTaskLocker{heldFound: true, heldFor: 2 * stuckTaskLockThreshold, heldErr: errors.New("pg_stat_activity unavailable")}},
	}
	for _, tc := range skipCases {
		t.Run(tc.name+" - normal skip", func(t *testing.T) {
			app := &Application{Scheduler: &testutil.MockScheduler{}, TaskLocker: tc.locker}

			result, err := app.HandleScheduledTask(testutil.TestContext(t), TaskCleanupExpiredRecords, ScheduledTaskParams{})
			testutil.AssertNoError(t, err)
			testutil.AssertEqual(t, 1, tc.locker.heldCalls)
			m, ok := result.(map[string]string)
			if !ok {
				t.Fatalf("expected map[string]string, got %T", result)
			}
			testutil.AssertEqual(t, "already_running", m["reason"])
		})
	}

	t.Run("held past the threshold - returns ErrTaskLockStuck", func(t *testing.T) {
		locker := &mockTaskLocker{heldFound: true, heldFor: stuckTaskLockThreshold + time.Second}
		collected := false
		app := &Application{
			Scheduler: &testutil.MockScheduler{
				CollectRecommendationsFunc: func(context.Context, string) (*scheduler.CollectResult, error) {
					collected = true
					return &scheduler.CollectResult{}, nil
				},
			},
			TaskLocker: locker,
		}

		result, err := app.HandleScheduledTask(testutil.TestContext(t), TaskCollectRecommendations, ScheduledTaskParams{})
		if !errors.Is(err, ErrTaskLockStuck) {
			t.Fatalf("expected ErrTaskLockStuck, got %v", err)
		}
		testutil.AssertContains(t, err.Error(), `"collect_recommendations"`)
		testutil.AssertContains(t, err.Error(), "30m1s")
		if result != nil {
			t.Fatalf("expected nil result, got %v", result)
		}
		if collected {
			t.Fatal("a stuck-lock skip must not run the task")
		}
		testutil.AssertEqual(t, 0, locker.unlockCalls)
	})

	// The stuck path still skips the run, so a marker-owning collect must
	// release its marker exactly as a normal skip does.
	t.Run("held past the threshold - still releases the owner's marker", func(t *testing.T) {
		store := new(mocks.MockConfigStore)
		t.Cleanup(func() { store.AssertExpectations(t) })
		store.On("ClearCollectionStarted", mock.Anything, "tok-owner").Return(nil)

		app := &Application{
			Config:     store,
			Scheduler:  &testutil.MockScheduler{},
			TaskLocker: &mockTaskLocker{heldFound: true, heldFor: time.Hour},
		}

		_, err := app.HandleScheduledTask(testutil.TestContext(t), TaskCollectRecommendations,
			ScheduledTaskParams{OwnerToken: "tok-owner"})
		if !errors.Is(err, ErrTaskLockStuck) {
			t.Fatalf("expected ErrTaskLockStuck, got %v", err)
		}
	})

	t.Run("lock acquired - holder age never checked", func(t *testing.T) {
		locker := &mockTaskLocker{acquired: true, heldFound: true, heldFor: time.Hour}
		app := &Application{Scheduler: &testutil.MockScheduler{}, TaskLocker: locker}

		_, err := app.HandleScheduledTask(testutil.TestContext(t), TaskCleanupExpiredRecords, ScheduledTaskParams{})
		testutil.AssertNoError(t, err)
		testutil.AssertEqual(t, 0, locker.heldCalls)
	})
}

// TestHandleScheduledHTTP_StuckLockIsNon200 checks the /api/scheduled/ path
// end to end: a normal skip answers 200, a stuck lock answers 500 so Cloud
// Scheduler / Logic Apps record a failed attempt.
func TestHandleScheduledHTTP_StuckLockIsNon200(t *testing.T) {
	cases := []struct {
		name    string
		heldFor time.Duration
		want    int
	}{
		{name: "plausible run", heldFor: time.Minute, want: 200},
		{name: "stuck lock", heldFor: time.Hour, want: 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &Application{
				Scheduler:  &testutil.MockScheduler{},
				TaskLocker: &mockTaskLocker{heldFound: true, heldFor: tc.heldFor},
			}
			req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/scheduled/cleanup", nil)
			w := httptest.NewRecorder()

			app.handleScheduledHTTP(w, req)

			testutil.AssertEqual(t, tc.want, w.Code)
		})
	}
}
