package purchase

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Fixture rows (mock store and mock provider): nothing attaches
// recommendations to a plan step yet, so notified rows are built by hand.
//
// Run-now on a notified row is a human action standing in for the approval,
// so it reaches executeAndFinalize and its guards exactly like approve does.
// The spend cap and the kill switch are not re-checked there (same as approve).

var runNowFrom = []string{"pending", "notified", "paused"}

func TestRunNow_NotifiedRowReachesArmedRedriveGuard(t *testing.T) {
	exec := armedExecution("azure", "savingsplans", 1, "notified")
	mgr, store, rec := armedHarness(t, common.ServiceSavingsPlansAll)
	store.On("GetExecutionByID", mock.Anything, exec.ExecutionID).Return(exec, nil).Maybe()
	expectClaim(store, exec, runNowFrom)

	_, _, err := mgr.RunPlannedPurchaseNow(context.Background(), exec.ExecutionID, "operator@example.com", nil)

	assert.Zero(t, rec.count())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to execute retry attempt 1")
}

func TestRunNow_NotifiedRowReachesStaleAzurePricingRefusal(t *testing.T) {
	exec := armedExecution("azure", "compute", 0, "notified")
	exec.Recommendations[0].ID = "azure-current"
	mgr, store, rec := armedHarness(t, common.ServiceCompute)
	repriced := exec.Recommendations[0]
	repriced.UpfrontCost++
	store.On("ListStoredRecommendations", mock.Anything, config.RecommendationFilter{Provider: "azure", RequireAzurePricingMigration: true}).
		Return([]config.RecommendationRecord{repriced}, nil).Maybe()
	store.On("GetExecutionByID", mock.Anything, exec.ExecutionID).Return(exec, nil).Maybe()
	expectClaim(store, exec, runNowFrom)

	_, _, err := mgr.RunPlannedPurchaseNow(context.Background(), exec.ExecutionID, "operator@example.com", nil)

	assert.Zero(t, rec.count())
	require.ErrorIs(t, err, ErrStaleAzurePricing)
}

// The scheduler honors scheduled_date: a notified row whose date was pushed
// into the future is not claimed; once the date passes it is.
func TestScheduler_NotifiedRowRespectsPushedScheduledDate(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		date      time.Time
		wantClaim bool
	}{
		{"inside the veto window", time.Now().Add(48 * time.Hour), false},
		{"after the window", time.Now().Add(-time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, factory := new(MockConfigStore), new(MockProviderFactory)
			exec := dueRecExec("notified", upfrontRec(10))
			exec.ScheduledDate = tc.date
			manager := newPinManager(ctx, store, factory, exec, autoPlan(f64(1000)), autoPlan(f64(1000)))
			store.On("TransitionExecutionStatusPinned", ctx, "exec-gate", mock.Anything, "running", (*string)(nil), false).
				Return(nil, config.ErrExecutionNotInExpectedStatus).Maybe()

			_, err := manager.ProcessScheduledPurchases(ctx)
			require.NoError(t, err)
			if tc.wantClaim {
				store.AssertCalled(t, "TransitionExecutionStatusPinned", ctx, "exec-gate", mock.Anything, "running", (*string)(nil), false)
			} else {
				store.AssertNotCalled(t, "TransitionExecutionStatusPinned", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			}
		})
	}
}
