package purchase

// Issue #712: the fire and process sweeps refuse a row that was due more than
// the due-row age cap ago. Mock store, mock email and a provider factory that
// fails if reached (labeled fixtures; nothing here reaches a cloud API). The
// Postgres replay, including concurrent sweeps, is in
// due_age_cap_integration_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDueRowAgeRefusal(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	cases := []struct {
		name    string
		exec    config.PurchaseExecution
		refused bool
	}{
		{"pending row inside the cap", config.PurchaseExecution{ScheduledDate: *ago(23 * time.Hour)}, false},
		{"pending row exactly at the cap", config.PurchaseExecution{ScheduledDate: *ago(24 * time.Hour)}, false},
		{"pending row past the cap", config.PurchaseExecution{ScheduledDate: *ago(24*time.Hour + time.Second)}, true},
		{"not yet due", config.PurchaseExecution{ScheduledDate: now.Add(time.Hour)}, false},
		// A delayed purchase is due at scheduled_execution_at, whatever its
		// step date says.
		{"scheduled row fresh, old step date", config.PurchaseExecution{ScheduledDate: *ago(30 * 24 * time.Hour), ScheduledExecutionAt: ago(time.Hour)}, false},
		{"scheduled row past the cap, recent step date", config.PurchaseExecution{ScheduledDate: *ago(time.Hour), ScheduledExecutionAt: ago(25 * time.Hour)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := dueRowAgeRefusal(&tc.exec, DefaultMaxDueRowAge, now)
			if !tc.refused {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrDueRowTooOld)
			assert.Contains(t, err.Error(), "nothing was submitted")
			assert.Contains(t, err.Error(), "SCHEDULED_PURCHASE_MAX_OVERDUE")
		})
	}
}

func TestDueRowMaxAge_DefaultAndOverride(t *testing.T) {
	assert.Equal(t, 24*time.Hour, DefaultMaxDueRowAge)
	assert.Equal(t, DefaultMaxDueRowAge, (&Manager{}).dueRowMaxAge(), "an unset cap is the conservative default, never no cap")
	assert.Equal(t, 2*time.Hour, NewManager(ManagerConfig{MaxDueRowAge: 2 * time.Hour}).dueRowMaxAge())
}

// staleRefusalStore expects the refusal's writes for exec-age: the guarded
// failed save (captured) and the suppression release in one transaction.
func staleRefusalStore(store *MockConfigStore, saved **config.PurchaseExecution) {
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	store.On("SaveExecutionIfStatusTx", mock.Anything, mock.Anything, mock.Anything, executorOwnedStatuses).
		Run(func(args mock.Arguments) {
			c := *args.Get(2).(*config.PurchaseExecution)
			*saved = &c
		}).Return(nil).Once()
	store.On("DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "exec-age").Return(nil).Once()
}

func assertStaleFailure(t *testing.T, saved *config.PurchaseExecution) {
	t.Helper()
	require.NotNil(t, saved, "the refused row must be saved as failed")
	assert.Equal(t, config.StatusFailed, saved.Status)
	assert.True(t, saved.NoSubmission, "the refusal is proven before submission")
	assert.Contains(t, saved.Error, ErrDueRowTooOld.Error(), "History must say why the row was skipped")
}

func overdueScheduledRow(late time.Duration) config.PurchaseExecution {
	row := dueExec("exec-age")
	at := time.Now().Add(-late)
	row.ScheduledExecutionAt = &at
	row.ScheduledDate = time.Now()
	row.Recommendations = approvalTestRecs()
	return row
}

func TestFireScheduledDelayedPurchases_PastTheCapIsFailedUnbought(t *testing.T) {
	for _, tc := range []struct {
		name   string
		maxAge time.Duration
		late   time.Duration
	}{
		{"default cap", 0, 25 * time.Hour},
		{"configured cap", 2 * time.Hour, 3 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := new(MockConfigStore)
			factory := new(MockProviderFactory)
			failOnProviderCall(factory)
			mailer := new(MockEmailSender)
			mailer.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).Return(nil).Once()

			row := overdueScheduledRow(tc.late)
			claimed := row
			claimed.Status = "running"
			store.On("GetScheduledExecutionsDue", ctx).Return([]config.PurchaseExecution{row}, nil)
			store.On("TransitionExecutionStatus", ctx, "exec-age", []string{"scheduled"}, "running", (*string)(nil)).Return(&claimed, nil).Once()
			var saved *config.PurchaseExecution
			staleRefusalStore(store, &saved)

			m := &Manager{config: store, email: mailer, providerFactory: factory, maxDueRowAge: tc.maxAge}
			result, err := m.FireScheduledDelayedPurchases(ctx)
			require.NoError(t, err)
			assert.Equal(t, FireResult{Found: 1, Stale: 1}, *result)
			assertStaleFailure(t, saved)
			assert.Nil(t, saved.ApprovedBy, "a refused row is not stamped as approved by the scheduler")
			factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
			store.AssertExpectations(t)
			mailer.AssertExpectations(t)
		})
	}
}

// Inside the cap the row goes on to the purchase: here the configured cap is
// wider than the default and the row is 30h late, so it reaches the stamp.
func TestFireScheduledDelayedPurchases_ConfiguredCapWiderThanDefaultStillFires(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	row := overdueScheduledRow(30 * time.Hour)
	claimed := row
	claimed.Status = "running"
	store.On("GetScheduledExecutionsDue", ctx).Return([]config.PurchaseExecution{row}, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-age", []string{"scheduled"}, "running", (*string)(nil)).Return(&claimed, nil).Once()
	// The stamp finds the claim gone, which stops the run right after the
	// cap check: reaching it proves the cap let the row through.
	store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, executorOwnedStatuses).Return(lostGuard()).Once()

	m := &Manager{config: store, maxDueRowAge: 48 * time.Hour}
	result, err := m.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, FireResult{Found: 1, RaceLost: 1}, *result)
	store.AssertExpectations(t)
}

// A refusal whose failed save does not land leaves the row running for the
// reaper: that is a real error, not a clean stale skip.
func TestFireScheduledDelayedPurchases_StaleRefusalSaveFailureIsErrored(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	mailer := allowFailedNotification(new(MockEmailSender))
	row := overdueScheduledRow(25 * time.Hour)
	claimed := row
	claimed.Status = "running"
	store.On("GetScheduledExecutionsDue", ctx).Return([]config.PurchaseExecution{row}, nil)
	store.On("TransitionExecutionStatus", ctx, "exec-age", []string{"scheduled"}, "running", (*string)(nil)).Return(&claimed, nil).Once()
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{}, nil).Maybe()
	store.On("SaveExecutionIfStatusTx", mock.Anything, mock.Anything, mock.Anything, executorOwnedStatuses).Return(errors.New("db down")).Once()

	m := &Manager{config: store, email: mailer}
	result, err := m.FireScheduledDelayedPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, FireResult{Found: 1, Errored: 1}, *result)
	store.AssertExpectations(t)
}

func TestProcessScheduledPurchases_PastTheCapIsFailedUnbought(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	factory := new(MockProviderFactory)
	failOnProviderCall(factory)
	mailer := new(MockEmailSender)
	mailer.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).Return(nil).Once()

	plan := &config.PurchasePlan{ID: "plan-gate", AutoPurchase: true, MaxStepCommitment: f64(1000)}
	row := dueRecExec("notified", upfrontRec(10))
	row.ExecutionID = "exec-age"
	row.ScheduledDate = time.Now().Add(-25 * time.Hour)
	claimed := row
	claimed.Status = "running"
	store.On("GetStaleApprovedExecutions", ctx, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.On("GetPendingExecutions", ctx).Return([]config.PurchaseExecution{row}, nil)
	store.On("GetPurchasePlan", mock.Anything, "plan-gate").Return(plan, nil)
	store.On("GetGlobalConfig", mock.Anything).Return(autoPurchaseOnCfg(), nil)
	store.On("TransitionExecutionStatusPinned", ctx, "exec-age", []string{"approved", "pending", "notified"}, "running", (*string)(nil), false).
		Return(&claimed, nil).Once()
	var saved *config.PurchaseExecution
	staleRefusalStore(store, &saved)

	m := &Manager{config: store, email: mailer, providerFactory: factory}
	result, err := m.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Processed)
	assert.Equal(t, 0, result.Executed)
	assert.Equal(t, 1, result.Failed, "a refused row is a failed row, as for the other post-claim refusals")
	assert.Equal(t, 1, result.Stale)
	require.Len(t, result.Errors, 1)
	assert.Contains(t, result.Errors[0], ErrDueRowTooOld.Error())
	assertStaleFailure(t, saved)
	factory.AssertNotCalled(t, "CreateAndValidateProvider", mock.Anything, mock.Anything, mock.Anything)
	store.AssertExpectations(t)
	mailer.AssertExpectations(t)
}

// The cap only applies to rows the sweep would buy. An overdue row on a manual
// plan stays the human's to approve: the sweep never claims or fails it.
func TestProcessScheduledPurchases_CapNeverTouchesRowsAwaitingApproval(t *testing.T) {
	row := dueRecExec("notified", upfrontRec(10))
	row.ScheduledDate = time.Now().Add(-10 * 24 * time.Hour)
	result := gateHarness(t, &config.PurchasePlan{ID: "plan-gate"}, row, autoPurchaseOnCfg(), nil)
	assert.Equal(t, ProcessResult{}, *result)
}

// The SQS execute_purchase path shares the claim funnel but passes no cap.
func TestClaimedRowRefusal_NoCapWithoutMaxAge(t *testing.T) {
	ancient := &config.PurchaseExecution{ScheduledDate: time.Now().Add(-365 * 24 * time.Hour)}
	require.NoError(t, (&Manager{}).claimedRowRefusal(context.Background(), ancient, false, 0))
	require.ErrorIs(t, (&Manager{}).claimedRowRefusal(context.Background(), ancient, false, time.Hour), ErrDueRowTooOld)
}
