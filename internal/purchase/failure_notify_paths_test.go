package purchase

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

// Issue #1195 (maintainer scope): every terminal-failure writer outside
// executeAndFinalize alerts too, once per row and only when it won the row.
// Each case runs one writer and reports the row it acted on.

type writerCase struct {
	name string
	// run wires store and calls the writer for execution "exec-w".
	run func(t *testing.T, m *Manager, store *MockConfigStore)
	// alerts is whether the writer must alert (it won the row).
	alerts bool
}

func writerRow(status string) *config.PurchaseExecution {
	return &config.PurchaseExecution{
		ExecutionID: "exec-w", PlanID: "plan-w", Status: status,
		Recommendations: []config.RecommendationRecord{
			{Provider: "azure", Service: "savingsplans", ResourceType: "Compute", Region: "eastus", Count: 1, Selected: true},
		},
	}
}

func failedRowFromCAS(store *MockConfigStore, method string, args ...interface{}) {
	store.On(method, args...).Return(writerRow("failed"), nil)
}

func strandedWriterCases() []writerCase {
	transition := []interface{}{mock.Anything, "exec-w", []string{"approved"}, "failed", (*string)(nil)}
	return []writerCase{
		{name: "safeFail won CAS", alerts: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatus", transition...)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
			counted, err := m.safeFail(context.Background(), writerRow("approved"))
			require.NoError(t, err)
			assert.True(t, counted)
		}},
		{name: "safeFail won CAS, reason save failed", alerts: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatus", transition...)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(errors.New("db down"))
			counted, err := m.safeFail(context.Background(), writerRow("approved"))
			require.NoError(t, err)
			assert.True(t, counted)
		}},
		{name: "safeFail lost CAS", run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			store.On("TransitionExecutionStatus", transition...).Return(nil, config.ErrExecutionNotInExpectedStatus)
			counted, err := m.safeFail(context.Background(), writerRow("approved"))
			require.NoError(t, err)
			assert.False(t, counted)
		}},
		{name: "reaper won CAS", alerts: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatus", mock.Anything, "exec-w", []string{"running"}, failedStatus, (*string)(nil))
			store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{failedStatus}).Return(nil)
			result := &ReapResult{}
			m.reapOne(context.Background(), writerRow("running"), DefaultReapAfter, time.Now(), result)
			assert.Equal(t, 1, result.Reaped)
		}},
		{name: "reaper won CAS, reason save failed", alerts: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatus", mock.Anything, "exec-w", []string{"running"}, failedStatus, (*string)(nil))
			store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{failedStatus}).Return(errors.New("db down"))
			result := &ReapResult{}
			m.reapOne(context.Background(), writerRow("running"), DefaultReapAfter, time.Now(), result)
			assert.Equal(t, ReapResult{Reaped: 1, Errored: 1}, *result)
		}},
		// The executor finished late and recovered the row to completed before
		// the reap annotation: the purchase did not fail, so nothing alerts.
		{name: "reaper won CAS, executor recovered the row", run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatus", mock.Anything, "exec-w", []string{"running"}, failedStatus, (*string)(nil))
			store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{failedStatus}).Return(config.ErrExecutionNotInExpectedStatus)
			result := &ReapResult{}
			m.reapOne(context.Background(), writerRow("running"), DefaultReapAfter, time.Now(), result)
			assert.Equal(t, ReapResult{Reaped: 1}, *result)
		}},
		{name: "reaper lost CAS", run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			store.On("TransitionExecutionStatus", mock.Anything, "exec-w", []string{"running"}, failedStatus, (*string)(nil)).
				Return(nil, config.ErrExecutionNotInExpectedStatus)
			result := &ReapResult{}
			m.reapOne(context.Background(), writerRow("running"), DefaultReapAfter, time.Now(), result)
			assert.Equal(t, 1, result.RaceLost)
		}},
	}
}

func planStepWriterCases() []writerCase {
	pinned := []interface{}{mock.Anything, "exec-w", []string{"pending", "notified"}, "failed", (*string)(nil), false}
	return []writerCase{
		{name: "plan step won CAS", alerts: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatusPinned", pinned...)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
			err := m.failExistingPlanStep(context.Background(), writerRow("notified"), "refused")
			assert.ErrorIs(t, err, errExecutionNotNotifiable)
		}},
		{name: "plan step won CAS, reason save failed", alerts: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatusPinned", pinned...)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(errors.New("db down"))
			err := m.failExistingPlanStep(context.Background(), writerRow("notified"), "refused")
			assert.ErrorContains(t, err, "db down")
		}},
		{name: "plan step lost CAS", run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			store.On("TransitionExecutionStatusPinned", pinned...).Return(nil, config.ErrExecutionNotInExpectedStatus)
			err := m.failExistingPlanStep(context.Background(), writerRow("notified"), "refused")
			assert.ErrorIs(t, err, errExecutionNotNotifiable)
		}},
		{name: "claimed row refused before submission", alerts: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
			err := m.failClaimedBeforeSubmission(context.Background(), writerRow("running"), errors.New("cap exceeded"))
			assert.ErrorContains(t, err, "cap exceeded")
		}},
		{name: "claimed row refused, terminal save failed", alerts: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(errors.New("db down"))
			err := m.failClaimedBeforeSubmission(context.Background(), writerRow("running"), errors.New("cap exceeded"))
			assert.ErrorIs(t, err, config.ErrAuditLoss)
		}},
	}
}

func TestTerminalFailureWritersAlertOncePerWonRow(t *testing.T) {
	for _, tc := range append(strandedWriterCases(), planStepWriterCases()...) {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureMarkers(t)
			store := new(MockConfigStore)
			store.On("GetGlobalConfig", mock.Anything).Return(failureNotifyCfg(), nil)
			sender := new(MockEmailSender)
			rec := &failedEmailRecorder{}
			rec.expect(sender)
			m := &Manager{config: store, email: sender}

			tc.run(t, m, store)

			want := 0
			if tc.alerts {
				want = 1
			}
			assert.Equal(t, want, logs.count("exec-w"), "purchase-failed log marker")
			sent := rec.all()
			require.Len(t, sent, want, "purchase-failed email")
			if want == 1 {
				assert.Equal(t, "exec-w", sent[0].ExecutionID)
				assert.Equal(t, failureNotifyRecipient, sent[0].RecipientEmail)
			}
		})
	}
}
