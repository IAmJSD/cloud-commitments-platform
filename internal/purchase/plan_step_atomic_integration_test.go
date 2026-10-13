//go:build integration

package purchase

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func atomicStepPlan(ctx context.Context, t *testing.T, store *config.PostgresStore) *config.PurchasePlan {
	t.Helper()
	next := time.Now().Add(24 * time.Hour).Truncate(time.Microsecond)
	plan := &config.PurchasePlan{
		Name: "atomic step", Enabled: true, AutoPurchase: true, NotificationDaysBefore: 30,
		Services: map[string]config.ServiceConfig{"ec2": {Provider: "aws", Service: "ec2"}},
		RampSchedule: config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7,
			CurrentStep: 0, TotalSteps: 4, StartDate: next.AddDate(0, 0, -7)},
		NextExecutionDate: &next,
	}
	require.NoError(t, plan.Validate())
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))
	return plan
}

// stepRowsInDB lists every row of step 1 of the plan.
func stepRowsInDB(ctx context.Context, t *testing.T, store *config.PostgresStore, planID string) []config.PurchaseExecution {
	t.Helper()
	var rows []config.PurchaseExecution
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = store.ListExecutionsForPlanStepTx(ctx, tx, planID, 1)
		return err
	}))
	return rows
}

// A4: concurrent ticks (SQS redelivery, cron overlap, scheduler retries) must
// leave exactly one row for the step. Without the plan lock every tick reads
// "nothing there" and inserts its own.
func TestGetOrCreateExecution_ConcurrentTicksMintOneRow(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store)
	// No max_step_commitment on the plan: the cap refuses the step, giving it one failed row.
	m := &Manager{config: store, resolver: &fakeResolver{recs: []config.RecommendationRecord{attachRec("rec-a", 1)}}}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, _ = m.getOrCreateExecution(ctx, plan)
		}()
	}
	wg.Wait()

	rows := stepRowsInDB(ctx, t, store, plan.ID)
	require.Len(t, rows, 1, "concurrent ticks must mint exactly one row for the step")
	assert.Equal(t, "failed", rows[0].Status)
}

// E1: a settled root (failed here; partially_completed is the same branch)
// blocks creation, so no second root can be bought.
func TestGetOrCreateExecution_SettledRootBlocksNewRoot(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store)
	for _, status := range []string{"failed", "partially_completed"} {
		p := atomicStepPlan(ctx, t, store)
		root := &config.PurchaseExecution{PlanID: p.ID, ExecutionID: uuid.New().String(), Status: status, StepNumber: 1,
			ScheduledDate: *p.NextExecutionDate, Recommendations: []config.RecommendationRecord{scopedTestRec("acct-A")}}
		require.NoError(t, store.SavePurchaseExecution(ctx, root))

		_, _, _, err := (&Manager{config: store}).getOrCreateExecution(ctx, p)

		require.ErrorIs(t, err, errExecutionNotNotifiable, status)
		assert.Len(t, stepRowsInDB(ctx, t, store, p.ID), 1, status)
	}
	_ = plan
}

// A5/CAS: racing resolvers attach exactly once, with exactly one suppression
// set written in the same transaction. Skipping the suppressions fails this.
func TestAttachResolvedRecommendations_ConcurrentAttachesOnce(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store)
	rootID := uuid.New().String()
	require.NoError(t, store.SavePurchaseExecution(ctx, &config.PurchaseExecution{
		PlanID: plan.ID, ExecutionID: rootID, Status: "pending", StepNumber: 1, ScheduledDate: *plan.NextExecutionDate}))
	rec := scopedTestRec("")
	rec.CloudAccountID = nil
	recs := []config.RecommendationRecord{rec}
	m := &Manager{config: store}

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := m.AttachResolvedRecommendations(ctx, &config.PurchaseExecution{ExecutionID: rootID}, recs)
			assert.NoError(t, err)
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, wins, "exactly one resolver may attach")
	got, err := store.GetExecutionByID(ctx, rootID)
	require.NoError(t, err)
	assert.Len(t, got.Recommendations, 1)
	assert.Equal(t, 300.0, got.TotalUpfrontCost)
	sups, err := store.ListActiveSuppressions(ctx)
	require.NoError(t, err)
	require.Len(t, sups, 1, "the winner's suppressions must exist, and only once")
	assert.Equal(t, rootID, sups[0].ExecutionID)

	// A row that already has recs is never overwritten.
	ok, err := m.AttachResolvedRecommendations(ctx, &config.PurchaseExecution{ExecutionID: rootID}, recs)
	require.NoError(t, err)
	assert.False(t, ok)
}

// E3: expiry flips the status and drops the suppressions in one tx; a row that
// already left pending/notified is left alone.
func TestExpireExecutionAtomic_ReleasesSuppressions(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store)
	rootID := uuid.New().String()
	require.NoError(t, store.SavePurchaseExecution(ctx, &config.PurchaseExecution{
		PlanID: plan.ID, ExecutionID: rootID, Status: "pending", StepNumber: 1, ScheduledDate: *plan.NextExecutionDate}))
	rec := scopedTestRec("")
	rec.CloudAccountID = nil
	m := &Manager{config: store}
	ok, err := m.AttachResolvedRecommendations(ctx, &config.PurchaseExecution{ExecutionID: rootID}, []config.RecommendationRecord{rec})
	require.NoError(t, err)
	require.True(t, ok)

	expire := func() bool {
		var expired bool
		require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
			var e error
			expired, e = store.ExpireExecutionAtomic(ctx, tx, rootID)
			if e != nil || !expired {
				return e
			}
			return store.DeleteSuppressionsByExecutionTx(ctx, tx, rootID)
		}))
		return expired
	}
	require.True(t, expire())
	got, err := store.GetExecutionByID(ctx, rootID)
	require.NoError(t, err)
	assert.Equal(t, "expired", got.Status)
	sups, err := store.ListActiveSuppressions(ctx)
	require.NoError(t, err)
	assert.Empty(t, sups)
	assert.False(t, expire(), "an already-expired row must not be flipped again")
}

// The compare-and-set also matches a row stored with an empty JSON array (not
// only the null that a nil slice marshals to).
func TestSetExecutionRecommendationsIfEmpty_MatchesEmptyArray(t *testing.T) {
	ctx := context.Background()
	store, _ := rampStepStore(ctx, t)
	plan := atomicStepPlan(ctx, t, store)
	rootID := uuid.New().String()
	require.NoError(t, store.SavePurchaseExecution(ctx, &config.PurchaseExecution{
		PlanID: plan.ID, ExecutionID: rootID, Status: "pending", StepNumber: 1, ScheduledDate: *plan.NextExecutionDate,
		Recommendations: []config.RecommendationRecord{}}))
	m := &Manager{config: store}

	ok, err := m.AttachResolvedRecommendations(ctx, &config.PurchaseExecution{ExecutionID: rootID}, []config.RecommendationRecord{scopedTestRec("")})

	require.NoError(t, err)
	assert.True(t, ok)
}
