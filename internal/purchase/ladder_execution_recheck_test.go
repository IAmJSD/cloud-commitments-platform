package purchase

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Platform#118 PR4r: the execute-time recheck for ladder purchases. Like the
// armed re-drive tests, every case asserts on the provider call count: the
// only thing that matters is whether money moved. The store is a mock
// (FIXTURE); the real text-compare lookup is covered by the integration test in
// internal/config.

const ladderTrancheKey = "0f0f0f0f-1111-2222-3333-444444444444"

func ptr[T any](v T) *T { return &v }

// openGate is a gate that allows execution; each case flips one field.
func openGate() *config.LadderExecutionGate {
	return &config.LadderExecutionGate{ConfigEnabled: ptr(true), ConfigMode: ptr("auto_approve"), RunStatus: ptr("executing"), TrancheStatus: ptr("fired")}
}

func ladderGlobal(planning, execution bool) *config.GlobalConfig {
	return &config.GlobalConfig{LadderingEnabled: planning, LadderExecutionEnabled: execution}
}

// ladderHarness is armedHarness with the global config and the tranche lookup
// replaced by the case under test.
func ladderHarness(t *testing.T, key string, gate *config.LadderExecutionGate, gateErr error, global *config.GlobalConfig, globalErr error) (*Manager, *MockConfigStore, *armedPurchaseRecorder, *config.PurchaseExecution) {
	t.Helper()
	exec := armedExecution("aws", "savingsplans", 0, "approved")
	exec.IdempotencyKey = key
	mgr, store, rec := armedHarness(t, common.ServiceSavingsPlansAll)
	kept := store.ExpectedCalls[:0]
	for _, c := range store.ExpectedCalls {
		if c.Method != "GetGlobalConfig" {
			kept = append(kept, c)
		}
	}
	store.ExpectedCalls = kept
	store.On("GetGlobalConfig", mock.Anything).Return(global, globalErr).Maybe()
	store.On("GetLadderExecutionGate", mock.Anything, key).Return(gate, gateErr).Maybe()
	store.On("GetExecutionByID", mock.Anything, exec.ExecutionID).Return(exec, nil).Maybe()
	return mgr, store, rec, exec
}

func runViaSQS(t *testing.T, mgr *Manager, store *MockConfigStore, exec *config.PurchaseExecution) {
	t.Helper()
	expectClaim(store, exec, []string{"approved", "pending", "notified"})
	body, err := json.Marshal(AsyncMessage{Type: MessageTypeExecutePurchase, ExecutionID: exec.ExecutionID})
	require.NoError(t, err)
	_ = mgr.ProcessMessage(context.Background(), string(body))
}

// TestLadderRecheck_RefusesBeforeProviderCall covers every way the switches can
// be off, and the fail-closed lookups. Each must stop the buy and end the row
// failed with the no_submission proof, so the reconciler can release netting.
func TestLadderRecheck_RefusesBeforeProviderCall(t *testing.T) {
	gateWith := func(mut func(*config.LadderExecutionGate)) *config.LadderExecutionGate {
		g := openGate()
		mut(g)
		return g
	}
	cases := []struct {
		name      string
		gate      *config.LadderExecutionGate
		gateErr   error
		global    *config.GlobalConfig
		globalErr error
		want      string
	}{
		{"execution flag off", openGate(), nil, ladderGlobal(true, false), nil, "execution is switched off"},
		{"planning flag off", openGate(), nil, ladderGlobal(false, true), nil, "planning is switched off"},
		{"config disabled", gateWith(func(g *config.LadderExecutionGate) { g.ConfigEnabled = ptr(false) }), nil, ladderGlobal(true, true), nil, "config is disabled"},
		{"config mode email_approval", gateWith(func(g *config.LadderExecutionGate) { g.ConfigMode = ptr("email_approval") }), nil, ladderGlobal(true, true), nil, "not auto_approve"},
		{"run canceled", gateWith(func(g *config.LadderExecutionGate) { g.RunStatus = ptr(string(ladder.RunStatusCancelled)) }), nil, ladderGlobal(true, true), nil, "run was canceled"},
		{"config deleted", gateWith(func(g *config.LadderExecutionGate) { g.ConfigEnabled, g.ConfigMode = nil, nil }), nil, ladderGlobal(true, true), nil, "config no longer exists"},
		{"run deleted", gateWith(func(g *config.LadderExecutionGate) { g.RunStatus = nil }), nil, ladderGlobal(true, true), nil, "run no longer exists"},
		{"tranche failed by operator", gateWith(func(g *config.LadderExecutionGate) { g.TrancheStatus = ptr(string(ladder.TrancheStatusFailed)) }), nil, ladderGlobal(true, true), nil, "its tranche is failed"},
		{"tranche completed", gateWith(func(g *config.LadderExecutionGate) { g.TrancheStatus = ptr(string(ladder.TrancheStatusCompleted)) }), nil, ladderGlobal(true, true), nil, "its tranche is completed"},
		{"tranche cancelled", gateWith(func(g *config.LadderExecutionGate) { g.TrancheStatus = ptr(string(ladder.TrancheStatusCancelled)) }), nil, ladderGlobal(true, true), nil, "its tranche is cancelled"}, //nolint:misspell // "cancelled" is the stored ladder status value
		{"tranche status not known to the code", gateWith(func(g *config.LadderExecutionGate) { g.TrancheStatus = ptr("bogus") }), nil, ladderGlobal(true, true), nil, "its tranche is bogus"},
		{"tranche status unknown", gateWith(func(g *config.LadderExecutionGate) { g.TrancheStatus = nil }), nil, ladderGlobal(true, true), nil, "tranche status is unknown"},
		{"tranche lookup DB error", nil, errors.New("connection reset"), ladderGlobal(true, true), nil, "tranche lookup failed"},
		{"global config read error", openGate(), nil, nil, errors.New("connection reset"), "global config unreadable"},
		{"global config nil", openGate(), nil, nil, nil, "global config unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, store, rec, exec := ladderHarness(t, ladderTrancheKey, tc.gate, tc.gateErr, tc.global, tc.globalErr)
			var saved []config.PurchaseExecution
			store.SavePurchaseExecutionFn = func(_ context.Context, e *config.PurchaseExecution) error {
				saved = append(saved, *e)
				return nil
			}
			runViaSQS(t, mgr, store, exec)

			assert.Equal(t, 0, rec.count(), "a refused ladder execution must reach no provider")
			require.NotEmpty(t, saved)
			final := saved[len(saved)-1]
			assert.Equal(t, "failed", final.Status)
			assert.True(t, final.NoSubmission, "the refusal is a beforeSubmission error, so no_submission must be stamped")
			assert.Contains(t, final.Error, tc.want)
		})
	}
}

func TestLadderRecheck_AllowsWhenEverythingIsOn(t *testing.T) {
	for _, status := range []ladder.TrancheStatus{ladder.TrancheStatusFired, ladder.TrancheStatusScheduled} {
		t.Run(string(status), func(t *testing.T) {
			gate := openGate()
			gate.TrancheStatus = ptr(string(status))
			mgr, store, rec, exec := ladderHarness(t, ladderTrancheKey, gate, nil, ladderGlobal(true, true), nil)
			runViaSQS(t, mgr, store, exec)
			assert.Equal(t, 1, rec.count(), "an armed ladder execution on a %s tranche must still buy exactly once", status)
		})
	}
}

// Non-ladder keys must be untouched: the lookup returns no tranche for them,
// and the recheck must not read the global flags or refuse. The flags are OFF
// here, so a recheck that ignored "not a ladder execution" would refuse.
func TestLadderRecheck_NonLadderKeysUnaffected(t *testing.T) {
	for name, key := range map[string]string{
		"multi-account key":  ladderTrancheKey + ":123456789012",
		"legacy ExecutionID": "44444444-5555-6666-7777-888888888801",
		"empty key":          "",
	} {
		t.Run(name, func(t *testing.T) {
			mgr, store, rec, exec := ladderHarness(t, key, nil, nil, ladderGlobal(false, false), nil)
			runViaSQS(t, mgr, store, exec)
			assert.Equal(t, 1, rec.count(), "a non-ladder purchase must not be gated by the ladder switches")
		})
	}
}

// Every executor entry point reaches executeAndFinalize, so the recheck must
// stop each one. ProcessScheduledPurchases and the SQS message both land in
// claimAndExecute, the first case below.
func TestLadderRecheck_EveryExecutorEntryRefuses(t *testing.T) {
	off := ladderGlobal(true, false)
	t.Run("approve", func(t *testing.T) {
		mgr, store, rec, exec := ladderHarness(t, ladderTrancheKey, openGate(), nil, off, nil)
		exec.Status = "pending"
		expectClaim(store, exec, []string{"pending", "notified"})
		_, _, err := mgr.ApproveAndExecute(context.Background(), exec.ExecutionID, "operator@example.com", nil)
		require.Error(t, err)
		assert.Equal(t, 0, rec.count())
	})
	t.Run("claimAndExecute (SQS and cron sweep)", func(t *testing.T) {
		mgr, store, rec, exec := ladderHarness(t, ladderTrancheKey, openGate(), nil, off, nil)
		runViaSQS(t, mgr, store, exec)
		assert.Equal(t, 0, rec.count())
	})
	t.Run("scheduler fire", func(t *testing.T) {
		mgr, store, rec, exec := ladderHarness(t, ladderTrancheKey, openGate(), nil, off, nil)
		exec.Status = "scheduled"
		store.On("GetScheduledExecutionsDue", mock.Anything).Return([]config.PurchaseExecution{*exec}, nil).Once()
		expectClaim(store, exec, []string{"scheduled"})
		result, err := mgr.FireScheduledDelayedPurchases(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 0, rec.count())
		assert.Equal(t, 0, result.Fired)
	})
	t.Run("stranded re-drive", func(t *testing.T) {
		mgr, store, rec, exec := ladderHarness(t, ladderTrancheKey, openGate(), nil, off, nil)
		store.On("GetStaleApprovedExecutions", mock.Anything, mock.Anything).Return([]config.PurchaseExecution{*exec}, nil).Once()
		expectClaim(store, exec, []string{"approved"})
		_, err := mgr.RecoverStrandedApprovals(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 0, rec.count())
	})
}
