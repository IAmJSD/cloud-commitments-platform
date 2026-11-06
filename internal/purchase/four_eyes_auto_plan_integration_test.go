//go:build integration

package purchase

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1771: with 4-eyes on, an armed auto plan's due step waits for a human
// approval instead of being bought by the sweep. Real PostgreSQL; the cloud is
// the labeled fake provider of the ramp-step fixture.

func fourEyesUser(ctx context.Context, t *testing.T, f *rampStepFixture, name string) string {
	t.Helper()
	id := uuid.New().String()
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO users (id, email, password_hash, group_ids) VALUES ($1, $2, 'x', ARRAY[$1::uuid])`, id, name+"@example.invalid")
		return err
	}))
	return id
}

func fourEyesAutoRow(ctx context.Context, t *testing.T, f *rampStepFixture, creator *string) *config.PurchaseExecution {
	t.Helper()
	row := &config.PurchaseExecution{
		PlanID: f.plan.ID, ExecutionID: uuid.New().String(), Status: "notified", StepNumber: 3,
		ScheduledDate: time.Now().Add(-time.Hour), CloudAccountID: &f.accounts[0].ID,
		Recommendations: []config.RecommendationRecord{withAccount(rampStepRecommendation()[0], f.accounts[0].ID)},
		CreatedByUserID: creator, CreatedBySystem: creator == nil,
	}
	require.NoError(t, f.store.SavePurchaseExecution(ctx, row))
	return row
}

func setFourEyes(ctx context.Context, t *testing.T, f *rampStepFixture, on bool) {
	t.Helper()
	global, err := f.store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	global.RequireDifferentApprover = on
	require.NoError(t, f.store.SaveGlobalConfig(ctx, global))
}

func TestFourEyes_AutoPlanSweepLeavesRowForHumanApproval(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	setFourEyes(ctx, t, f, true)
	row := fourEyesAutoRow(ctx, t, f, nil)

	result, err := f.manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Processed)
	assert.Equal(t, 0, f.purchaseCount())
	assert.Equal(t, "notified", rowStatus(ctx, t, f, row.ExecutionID).Status)

	userB := fourEyesUser(ctx, t, f, "b")
	_, _, err = f.manager.ApproveAndExecute(ctx, row.ExecutionID, "b@example.invalid", &userB, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, f.purchaseCount())
	assert.Equal(t, "completed", rowStatus(ctx, t, f, row.ExecutionID).Status)
}

func TestFourEyes_AutoPlanPreCreatedRootCreatorCannotApprove(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	setFourEyes(ctx, t, f, true)
	userA := fourEyesUser(ctx, t, f, "a")
	userB := fourEyesUser(ctx, t, f, "b")
	row := fourEyesAutoRow(ctx, t, f, &userA)

	result, err := f.manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Processed)
	assert.Equal(t, 0, f.purchaseCount())

	_, _, err = f.manager.ApproveAndExecute(ctx, row.ExecutionID, "a@example.invalid", &userA, nil)
	require.ErrorIs(t, err, ErrFourEyesDenied)
	assert.Equal(t, 0, f.purchaseCount())
	assert.Equal(t, "notified", rowStatus(ctx, t, f, row.ExecutionID).Status)

	_, _, err = f.manager.ApproveAndExecute(ctx, row.ExecutionID, "b@example.invalid", &userB, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, f.purchaseCount())
	assert.Equal(t, "completed", rowStatus(ctx, t, f, row.ExecutionID).Status)
}

func TestFourEyes_AutoPlanSweepExecutesWhenFlagOff(t *testing.T) {
	ctx := context.Background()
	f := newRampStepFixture(ctx, t)
	setFourEyes(ctx, t, f, false)
	row := fourEyesAutoRow(ctx, t, f, nil)

	result, err := f.manager.ProcessScheduledPurchases(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Executed)
	assert.Equal(t, 1, f.purchaseCount())
	assert.Equal(t, "completed", rowStatus(ctx, t, f, row.ExecutionID).Status)
}
