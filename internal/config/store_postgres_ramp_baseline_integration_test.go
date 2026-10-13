//go:build integration
// +build integration

package config

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (f *rampFixture) rowWithRecs(ctx context.Context, t *testing.T, step int, account *string, status string, noSubmission bool, recs ...RecommendationRecord) *PurchaseExecution {
	t.Helper()
	exec := &PurchaseExecution{
		ExecutionID: uuid.New().String(), IdempotencyKey: uuid.New().String(), PlanID: f.planID,
		CloudAccountID: account, Status: status, StepNumber: step, ScheduledDate: time.Now(),
		Recommendations: recs, NoSubmission: noSubmission,
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, exec))
	return exec
}

func (f *rampFixture) committed(ctx context.Context, t *testing.T) map[string]PlanRampQuantity {
	t.Helper()
	var got map[string]PlanRampQuantity
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = f.store.ListPlanCommittedTx(ctx, tx, f.planID)
		return err
	}))
	return got
}

func (f *rampFixture) baseline(ctx context.Context, t *testing.T) PlanRampBaseline {
	t.Helper()
	var got PlanRampBaseline
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		got, err = f.store.GetPlanRampBaselineTx(ctx, tx, f.planID)
		return err
	}))
	return got
}

// A 4-step weekly plan over two accounts, with every row shape the ledger has
// to classify, read back from a real Postgres.
func TestListPlanCommittedTx_RealRowShapes(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 2, 0, 4)
	a1, a2 := &f.accounts[0], &f.accounts[1]
	r1 := baselineRec(*a1, "m5.large", 20)
	r2 := baselineRec(*a2, "m5.large", 20)
	k1, k2 := baselineKey(t, r1), baselineKey(t, r2)

	// Step 1: a fan-out. The root row carries both accounts' recs, each account
	// row its own; both accounts bought.
	f.rowWithRecs(ctx, t, 1, nil, StatusCompleted, false, r1, r2)
	f.rowWithRecs(ctx, t, 1, a1, StatusCompleted, false, r1)
	f.rowWithRecs(ctx, t, 1, a2, StatusCompleted, false, r2)

	// Step 2: account 1 failed with an unknown outcome and was retried; the
	// retry completed. Account 2 completed.
	failed := f.rowWithRecs(ctx, t, 2, a1, StatusFailed, false, r1)
	retry := f.retryInOneTx(ctx, t, failed, uuid.New().String(), StatusCompleted)
	retry.Recommendations = []RecommendationRecord{r1}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, retry))
	f.rowWithRecs(ctx, t, 2, a2, StatusCompleted, false, r2)

	// Step 3: canceled, legacy-canceled and a failed row proven unsubmitted hold nothing.
	f.rowWithRecs(ctx, t, 3, a1, StatusCanceled, false, r1)
	f.rowWithRecs(ctx, t, 3, a1, LegacyStatusCanceled, false, r1)
	f.rowWithRecs(ctx, t, 3, a2, StatusFailed, true, r2)

	got := f.committed(ctx, t)
	assert.Equal(t, PlanRampQuantity{Count: 40}, got[k1], "steps 1 and 2 for account 1, once each")
	assert.Equal(t, PlanRampQuantity{Count: 40}, got[k2], "steps 1 and 2 for account 2, once each")

	// Step 3 retried by an operator: rows that DO hold capacity join the ledger.
	f.rowWithRecs(ctx, t, 3, a1, "paused", false, r1)
	f.rowWithRecs(ctx, t, 3, a2, StatusFailed, false, r2)
	got = f.committed(ctx, t)
	assert.Equal(t, 60, got[k1].Count, "a paused row holds its step")
	assert.Equal(t, 60, got[k2].Count, "a failed row with an unknown outcome is assumed bought")

	partial := baselineRec(*a1, "c5.large", 6)
	partial.Error = "context deadline exceeded"
	f.rowWithRecs(ctx, t, 4, a1, StatusPartiallyCompleted, false, partial)
	got = f.committed(ctx, t)
	assert.Equal(t, 6, got[baselineKey(t, partial)].Count, "a partially_completed row counts its errored rec")
}

// The baseline is written once, by its own method, and nothing else touches it.
func TestPlanRampBaseline_ImmutableAndOutOfFullRowWrites(t *testing.T) {
	ctx := context.Background()
	f := newRampFixture(ctx, t, 1, 0, 4)
	r := baselineRec(f.accounts[0], "m5.large", 100)
	key := baselineKey(t, r)

	// A plan struct read BEFORE the baseline exists, as a handler's GET would hold.
	stale, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	assert.Nil(t, f.baseline(ctx, t), "no baseline until step 1 writes one")

	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		wrote, setErr := f.store.SetPlanRampBaselineTx(ctx, tx, f.planID, PlanRampBaseline{key: {Count: 100}})
		require.NoError(t, setErr)
		assert.True(t, wrote)
		wrote, setErr = f.store.SetPlanRampBaselineTx(ctx, tx, f.planID, PlanRampBaseline{key: {Count: 999}})
		require.NoError(t, setErr)
		assert.False(t, wrote, "a second write is refused")
		return nil
	}))
	assert.Equal(t, PlanRampBaseline{key: {Count: 100}}, f.baseline(ctx, t))

	// The ramp advances through CompletePlanStep (a full-row plan UPDATE).
	f.rowWithRecs(ctx, t, 1, &f.accounts[0], StatusCompleted, false, r)
	require.NoError(t, f.store.CompletePlanStep(ctx, f.planID, 1))
	assert.Equal(t, PlanRampBaseline{key: {Count: 100}}, f.baseline(ctx, t), "CompletePlanStep must not touch the baseline")

	// A plan edit from a struct read before the baseline existed.
	fresh, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	fresh.Name = "renamed"
	require.NoError(t, f.store.UpdatePurchasePlan(ctx, fresh))
	assert.Equal(t, PlanRampBaseline{key: {Count: 100}}, f.baseline(ctx, t), "a plan edit must not touch the baseline")
	stale.Name = "stale rename"
	_ = f.store.UpdatePurchasePlan(ctx, stale) // rejected as stale or applied: either way the baseline survives
	assert.Equal(t, PlanRampBaseline{key: {Count: 100}}, f.baseline(ctx, t), "a stale plan save must not clear the baseline")

	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := f.store.SetPlanRampBaselineTx(ctx, tx, uuid.New().String(), PlanRampBaseline{})
		require.Error(t, err, "an empty baseline is refused")
		_, err = f.store.GetPlanRampBaselineTx(ctx, tx, uuid.New().String())
		require.ErrorIs(t, err, ErrNotFound)
		return nil
	}))
}
