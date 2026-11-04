//go:build integration

package purchase

// Real PostgreSQL plus the ramp-step fixture's counting fake provider (labeled
// fixture; nothing here reaches a cloud API). Issue #712: when the fire and
// process sweeps are first scheduled, a backlog of overdue rows must not be
// bought. Rows due more than the age cap ago end failed and unbought, with
// the reason on the row, while rows inside the cap are still bought, each
// exactly once, however many sweeps overlap.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// agedRow saves a row on accounts[0] that is due `late` ago: a "scheduled"
// (delayed) purchase is due at scheduled_execution_at, every other row at
// scheduled_date.
func agedRow(ctx context.Context, t *testing.T, f *rampStepFixture, status string, late time.Duration) *config.PurchaseExecution {
	t.Helper()
	due := time.Now().Add(-late)
	row := &config.PurchaseExecution{
		PlanID: f.plan.ID, ExecutionID: uuid.New().String(), Status: status, StepNumber: 3,
		ScheduledDate: due, CloudAccountID: &f.accounts[0].ID,
		Recommendations: []config.RecommendationRecord{withAccount(rampStepRecommendation()[0], f.accounts[0].ID)},
	}
	if status == "scheduled" {
		row.ScheduledDate = time.Now()
		row.ScheduledExecutionAt = &due
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, row))
	return row
}

// countFailureEmails records each purchase-failed email by execution ID.
func countFailureEmails(f *rampStepFixture) func() map[string]int {
	var mu sync.Mutex
	sent := map[string]int{}
	f.email.On("SendPurchaseFailedNotification", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			sent[args.Get(1).(email.NotificationData).ExecutionID]++
		}).Return(nil).Maybe()
	return func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(sent))
		for k, v := range sent {
			out[k] = v
		}
		return out
	}
}

// Overlapping fire and process sweeps over a backlog that holds one overdue
// and one fresh row of each kind. Before the cap, every overdue row was bought.
func TestDueRowAgeCap_OverlappingSweepsBuyOnlyRowsInsideTheCap(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	f.repairAccounts(ctx, t)
	emails := countFailureEmails(f)

	overdue := []*config.PurchaseExecution{
		agedRow(ctx, t, f, "notified", 48*time.Hour),
		agedRow(ctx, t, f, "scheduled", 48*time.Hour),
	}
	fresh := []*config.PurchaseExecution{
		agedRow(ctx, t, f, "notified", time.Hour),
		agedRow(ctx, t, f, "scheduled", time.Hour),
	}

	const sweeps = 4
	var mu sync.Mutex
	fired := 0
	var wg sync.WaitGroup
	for i := 0; i < sweeps; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = f.manager.ProcessScheduledPurchases(ctx)
		}()
		go func() {
			defer wg.Done()
			if res, err := f.manager.FireScheduledDelayedPurchases(ctx); err == nil {
				mu.Lock()
				fired += res.Fired
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, len(fresh), f.purchaseCount(), "only the rows inside the cap reach the provider, each once")
	assert.Equal(t, 1, fired, "the fire sweeps fire the fresh delayed purchase once and the overdue one never")
	sent := emails()
	for _, row := range overdue {
		got := rowStatus(ctx, t, f, row.ExecutionID)
		assert.Equal(t, config.StatusFailed, got.Status, "overdue %s row", row.Status)
		assert.True(t, got.NoSubmission, "overdue %s row is proven unbought", row.Status)
		assert.Contains(t, got.Error, "due too long ago", "overdue %s row says why it was skipped", row.Status)
		assert.Equal(t, 1, sent[row.ExecutionID], "operators hear about overdue %s row exactly once", row.Status)
	}
	for _, row := range fresh {
		assert.Equal(t, "completed", rowStatus(ctx, t, f, row.ExecutionID).Status, "fresh %s row", row.Status)
		assert.Zero(t, sent[row.ExecutionID], "fresh %s row sends no failure email", row.Status)
	}
}

// A later tick finds nothing left to do: the refused rows are terminal and
// are neither bought nor reported again.
func TestDueRowAgeCap_RefusedRowsStayRefused(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	f.repairAccounts(ctx, t)
	emails := countFailureEmails(f)
	notified := agedRow(ctx, t, f, "notified", 48*time.Hour)
	scheduled := agedRow(ctx, t, f, "scheduled", 48*time.Hour)

	for tick := 0; tick < 2; tick++ {
		_, err := f.manager.ProcessScheduledPurchases(ctx)
		require.NoError(t, err)
		_, err = f.manager.FireScheduledDelayedPurchases(ctx)
		require.NoError(t, err)
	}

	assert.Zero(t, f.purchaseCount())
	sent := emails()
	for _, row := range []*config.PurchaseExecution{notified, scheduled} {
		assert.Equal(t, config.StatusFailed, rowStatus(ctx, t, f, row.ExecutionID).Status)
		assert.Equal(t, 1, sent[row.ExecutionID])
	}
}
