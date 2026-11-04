package purchase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
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
	// outcomeUnknown is whether the alert is for a run a sweep failed after it
	// was interrupted, whose commitment may already exist (issue #1415).
	outcomeUnknown bool
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
		{name: "safeFail won CAS", alerts: true, outcomeUnknown: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatus", transition...)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
			counted, err := m.safeFail(context.Background(), writerRow("approved"))
			require.NoError(t, err)
			assert.True(t, counted)
		}},
		{name: "safeFail won CAS, reason save failed", alerts: true, outcomeUnknown: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatus", transition...)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(errors.New("db down"))
			counted, err := m.safeFail(context.Background(), writerRow("approved"))
			require.NoError(t, err)
			assert.True(t, counted)
		}},
		// The row was recovered before the guarded stamp: the failed outcome
		// is stale, so nothing alerts (#1417).
		{name: "safeFail won CAS, row recovered before the stamp", run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatus", transition...)
			store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{failedStatus}).Return(config.ErrExecutionNotInExpectedStatus)
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
		// claimAndRedrive won the approved -> running claim but the claimed row
		// no longer qualifies for a re-drive (#1037): the claim holder alerts,
		// even when its terminal save fails.
		{name: "claimed redrive row failed", alerts: true, outcomeUnknown: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"running"}).Return(nil)
			counted, err := m.failClaimedRedrive(context.Background(), writerRow("running"))
			require.NoError(t, err)
			assert.True(t, counted)
		}},
		{name: "claimed redrive row failed, terminal save failed", alerts: true, outcomeUnknown: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{"running"}).Return(errors.New("db down"))
			counted, err := m.failClaimedRedrive(context.Background(), writerRow("running"))
			require.ErrorIs(t, err, config.ErrAuditLoss)
			assert.False(t, counted)
		}},
		{name: "reaper won CAS", alerts: true, outcomeUnknown: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatus", mock.Anything, "exec-w", []string{"running"}, failedStatus, (*string)(nil))
			store.On("SaveExecutionIfStatus", mock.Anything, mock.Anything, []string{failedStatus}).Return(nil)
			result := &ReapResult{}
			m.reapOne(context.Background(), writerRow("running"), DefaultReapAfter, time.Now(), result)
			assert.Equal(t, 1, result.Reaped)
		}},
		{name: "reaper won CAS, reason save failed", alerts: true, outcomeUnknown: true, run: func(t *testing.T, m *Manager, store *MockConfigStore) {
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
		// The CAS, reason write and suppression release share one transaction
		// (#1052), so a later error rolls the CAS back: the row stays pending
		// for the next tick and nothing alerts.
		{name: "plan step won CAS, reason save rolled back", run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatusPinned", pinned...)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(errors.New("db down"))
			err := m.failExistingPlanStep(context.Background(), writerRow("notified"), "refused")
			assert.ErrorContains(t, err, "db down")
		}},
		{name: "plan step won CAS, suppression release rolled back", run: func(t *testing.T, m *Manager, store *MockConfigStore) {
			failedRowFromCAS(store, "TransitionExecutionStatusPinned", pinned...)
			store.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
			store.On("DeleteSuppressionsByExecutionTx", mock.Anything, mock.Anything, "exec-w").Return(errors.New("db down"))
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
			cfgRead := store.On("GetGlobalConfig", mock.Anything).Return(failureNotifyCfg(), nil)
			sender := new(MockEmailSender)
			assertMocksOnCleanup(t, store, sender)
			rec := &failedEmailRecorder{}
			send := rec.expect(sender)
			if !tc.alerts {
				// A writer that lost the row must not alert (Len 0 below); the
				// stubs only keep such a regression from panicking the mock.
				cfgRead.Maybe()
				send.Maybe()
			}
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
				assertFailureEmailBody(t, sent[0], tc.outcomeUnknown)
			}
		})
	}
}

// genuineFailureEmail is the purchase-failed email for writerRow, unchanged
// since before issue #1415.
const genuineFailureEmail = `CUDly - Purchase Failed
=======================

Some purchases could not be completed. Please review and retry manually.

Failed Purchases:

- 1x Compute in eastus
  Service: savingsplans


Review failed purchases:
/history

This is an automated message from CUDly.
`

// assertFailureEmailBody renders the email the writer sent. A row a sweep
// failed after an interrupted run may already have bought its commitment, so
// its email must name the execution, ask for a console check and never say
// "retry" (issue #1415). A genuinely failed row keeps the old email.
func assertFailureEmailBody(t *testing.T, data email.NotificationData, outcomeUnknown bool) {
	t.Helper()
	body, err := email.RenderPurchaseFailedEmail(data)
	require.NoError(t, err)
	if !outcomeUnknown {
		assert.Equal(t, genuineFailureEmail, body)
		return
	}
	assert.NotContains(t, strings.ToLower(body), "retry")
	assert.NotContains(t, body, "Failed Purchases")
	assert.Contains(t, body, "Purchase Outcome Unknown")
	assert.Contains(t, body, "execution exec-w (plan plan-w)")
	assert.Contains(t, body, "Verify each one in the cloud provider's console before buying it again.")
	assert.Contains(t, body, "Outcome unknown:\n\n- 1x Compute in eastus\n  Service: savingsplans\n")
}
