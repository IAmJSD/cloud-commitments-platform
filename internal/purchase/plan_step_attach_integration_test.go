//go:build integration

package purchase

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Real PostgreSQL; the resolver, the email sender and the recommendations are
// fixtures (nothing here reaches a cloud API or an inbox).

const attachAccount = "11111111-2222-3333-4444-555555555555"

func attachGlobal(ctx context.Context, t *testing.T, store *config.PostgresStore) {
	t.Helper()
	global, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	to := "fixture@example.invalid"
	global.NotificationEmail, global.AutoPurchaseEnabled = &to, true
	require.NoError(t, store.SaveGlobalConfig(ctx, global))
}

func attachPlanInDB(ctx context.Context, t *testing.T, store *config.PostgresStore, name string, next time.Time) *config.PurchasePlan {
	t.Helper()
	plan := &config.PurchasePlan{
		Name: name, Enabled: true, AutoPurchase: true, NotificationDaysBefore: 30, MaxStepCommitment: f64(1000),
		Services:          map[string]config.ServiceConfig{"ec2": {Provider: "aws", Service: "ec2", Enabled: true}},
		RampSchedule:      config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4, StartDate: next.AddDate(0, 0, -7)},
		NextExecutionDate: &next,
	}
	require.NoError(t, plan.Validate())
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))
	return plan
}

func attachRec(id string, count int) config.RecommendationRecord {
	acct := attachAccount
	return config.RecommendationRecord{ID: id, Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: "m5.large",
		Count: count, Term: 1, UpfrontCost: float64(count), Savings: 1, CloudAccountID: &acct}
}

type countingSender struct {
	*MockEmailSender
	mu    sync.Mutex
	sends int
}

func newCountingSender(ctx context.Context) *countingSender {
	s := &countingSender{MockEmailSender: new(MockEmailSender)}
	s.On("SendScheduledPurchaseNotification", ctx, mock.Anything).Run(func(mock.Arguments) { s.mu.Lock(); s.sends++; s.mu.Unlock() }).Return(nil)
	return s
}

func suppressedTotal(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(SUM(suppressed_count), 0) FROM purchase_suppressions WHERE account_id = $1`, attachAccount).Scan(&n))
	return n
}

func TestAttachIntegration_NewRowAttachesSuppressesAndBecomesNotified(t *testing.T) {
	ctx := context.Background()
	store, pool := rampStepStore(ctx, t)
	attachGlobal(ctx, t, store)
	plan := attachPlanInDB(ctx, t, store, "attach-one", time.Now().Add(24*time.Hour))
	sender := newCountingSender(ctx)
	m := &Manager{config: store, email: sender, resolver: &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-b", 2), attachRec("rec-a", 3)}}}

	require.True(t, m.sendPlanNotification(ctx, plan))

	rows := stepRowsInDB(ctx, t, store, plan.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "notified", rows[0].Status, "a successful email leaves the new row notified")
	assert.NotEmpty(t, rows[0].ApprovalToken)
	assert.NotEmpty(t, rows[0].IdempotencyKey)
	require.Len(t, rows[0].Recommendations, 2)
	assert.Equal(t, "rec-a", rows[0].Recommendations[0].ID)
	assert.Equal(t, 5, suppressedTotal(ctx, t, pool))
	assert.Equal(t, 1, sender.sends)
}

// S11 crash point: committed but the email failed. The row stays pending (the
// scheduler refuses it), and the next tick notifies it without resolving again.
func TestAttachIntegration_EmailFailureLeavesPendingAndNextTickDoesNotResolveAgain(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	attachGlobal(ctx, t, store)
	plan := attachPlanInDB(ctx, t, store, "attach-crash", time.Now().Add(24*time.Hour))
	res := &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 2)}}
	sender := new(MockEmailSender)
	sender.On("SendScheduledPurchaseNotification", ctx, mock.Anything).Return(fmt.Errorf("ses down")).Once()
	sender.On("SendScheduledPurchaseNotification", ctx, mock.Anything).Return(nil).Once()
	m := &Manager{config: store, email: sender, resolver: res}

	require.False(t, m.sendPlanNotification(ctx, plan))
	rows := stepRowsInDB(ctx, t, store, plan.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "pending", rows[0].Status)

	require.True(t, m.sendPlanNotification(ctx, plan))
	assert.Equal(t, "notified", stepRowsInDB(ctx, t, store, plan.ID)[0].Status)
	assert.EqualValues(t, 1, res.calls.Load(), "the second tick adopts the committed row")
}

func TestAttachIntegration_NoMatchAndCap(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	attachGlobal(ctx, t, store)

	t.Run("no match inside the grace window writes no row", func(t *testing.T) {
		plan := attachPlanInDB(ctx, t, store, "attach-nomatch-early", time.Now().Add(24*time.Hour))
		m := &Manager{config: store, email: newCountingSender(ctx), resolver: &fakeResolver{err: planstep.ErrNoMatch}}
		require.False(t, m.sendPlanNotification(ctx, plan))
		assert.Empty(t, stepRowsInDB(ctx, t, store, plan.ID))
	})
	t.Run("cap lowered between list and lock fails the step on the locked plan", func(t *testing.T) {
		plan := attachPlanInDB(ctx, t, store, "attach-cap", time.Now().Add(24*time.Hour))
		snapshot := *plan
		lowered := *plan
		lowered.MaxStepCommitment = f64(1)
		require.NoError(t, store.UpdatePurchasePlan(ctx, &lowered))
		m := &Manager{config: store, email: newCountingSender(ctx), resolver: &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 5)}}}

		require.False(t, m.sendPlanNotification(ctx, &snapshot))

		rows := stepRowsInDB(ctx, t, store, plan.ID)
		require.Len(t, rows, 1)
		assert.Equal(t, "failed", rows[0].Status)
		assert.Empty(t, rows[0].Recommendations)
		assert.Contains(t, rows[0].Error, "rec-a")
		assert.Contains(t, rows[0].Error, "exceeds max_step_commitment 1.00")
	})
	t.Run("pre-created empty root row is attached in place", func(t *testing.T) {
		plan := attachPlanInDB(ctx, t, store, "attach-root", time.Now().Add(24*time.Hour))
		root := &config.PurchaseExecution{PlanID: plan.ID, ExecutionID: "00000000-0000-4000-8000-0000000000a1", Status: "pending", StepNumber: 1, ScheduledDate: time.Now().Add(24 * time.Hour)}
		require.NoError(t, store.SavePurchaseExecution(ctx, root))
		m := &Manager{config: store, email: newCountingSender(ctx), resolver: &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 2)}}}

		require.True(t, m.sendPlanNotification(ctx, plan))

		rows := stepRowsInDB(ctx, t, store, plan.ID)
		require.Len(t, rows, 1, "no second row")
		assert.Equal(t, root.ExecutionID, rows[0].ExecutionID)
		assert.Equal(t, "notified", rows[0].Status)
		assert.Len(t, rows[0].Recommendations, 1)
	})
}

// S7a/S7b: ticks of different plans racing on one account and provider, with a
// barrier between Resolve's read and the write. The fixture resolver reads the
// committed suppressions, so a tick that resolves before another commits sees
// stale capacity; the per-provider lock makes it wait instead.
func TestAttachIntegration_PlansOnOneAccountNeverOverAttach(t *testing.T) {
	ctx := context.Background()
	store, pool := rampStepStore(ctx, t)
	attachGlobal(ctx, t, store)
	const capacity, want, ticks = 10, 3, 4
	require.LessOrEqual(t, ticks, int(pool.Config().MaxConns)/2-1, "each tick holds a tx connection plus a pooled read")

	var mu sync.Mutex
	resolved := 0
	allIn := make(chan struct{})
	barrier := func() {
		mu.Lock()
		resolved++
		if resolved == ticks {
			close(allIn)
		}
		mu.Unlock()
		select {
		case <-allIn:
		case <-time.After(500 * time.Millisecond): // with the lock the others never arrive
		}
	}
	sender := newCountingSender(ctx)
	var wg sync.WaitGroup
	for i := 0; i < ticks; i++ {
		plan := attachPlanInDB(ctx, t, store, fmt.Sprintf("attach-race-%d", i), time.Now().Add(24*time.Hour))
		id := fmt.Sprintf("rec-%d", i)
		res := &fakeResolver{}
		res.before = func() {
			shown := capacity - suppressedTotal(ctx, t, pool)
			barrier()
			res.recs = nil
			if shown > 0 {
				res.recs = []config.RecommendationRecord{attachRec(id, min(want, shown))}
			}
			if shown <= 0 {
				res.err = planstep.ErrNoMatch
			}
		}
		m := &Manager{config: store, email: sender, resolver: res}
		wg.Add(1)
		go func() { defer wg.Done(); m.sendPlanNotification(ctx, plan) }()
	}
	wg.Wait()

	assert.Equal(t, capacity, suppressedTotal(ctx, t, pool), "attached plus suppressed never exceeds what was shown")
}

// S7b: overlapping ticks of ONE plan leave one row, one set of suppressions and
// one resolve; every later tick adopts the committed row.
func TestAttachIntegration_ConcurrentTicksOnOnePlanMintOneRow(t *testing.T) {
	ctx := context.Background()
	store, pool := rampStepStore(ctx, t)
	attachGlobal(ctx, t, store)
	require.LessOrEqual(t, 4, int(pool.Config().MaxConns)/2-1)
	plan := attachPlanInDB(ctx, t, store, "attach-one-plan", time.Now().Add(24*time.Hour))
	res := &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 4)}}
	m := &Manager{config: store, email: newCountingSender(ctx), resolver: res}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _, _ = m.getOrCreateExecution(ctx, plan) }()
	}
	wg.Wait()

	assert.Len(t, stepRowsInDB(ctx, t, store, plan.ID), 1)
	assert.Equal(t, 4, suppressedTotal(ctx, t, pool))
	assert.EqualValues(t, 1, res.calls.Load())
}
