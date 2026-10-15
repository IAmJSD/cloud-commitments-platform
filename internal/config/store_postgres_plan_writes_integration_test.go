//go:build integration

package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func TestPlanWriteBodiesOuterTransaction(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)
	account := &CloudAccount{Name: "transaction account", Provider: "aws", ExternalID: uuid.NewString()}
	require.NoError(t, store.CreateCloudAccount(ctx, account))
	abort := errors.New("abort composed plan write")
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "commit"}[commit], func(t *testing.T) {
			stamp := time.Now().UTC().Truncate(time.Microsecond).Add(48 * time.Hour)
			cap := 25.5
			plan := &PurchasePlan{
				Name: "composed plan", Enabled: true, NotificationDaysBefore: 1,
				Services:     map[string]ServiceConfig{"aws/ec2": {Provider: "aws", Service: "ec2", Enabled: true, Term: 1}},
				RampSchedule: PresetRampSchedules["weekly-25pct"], NextExecutionDate: &stamp,
				LastExecutionDate: &stamp, LastNotificationSent: &stamp, MaxStepCommitment: &cap,
			}
			err := store.WithTx(ctx, func(tx pgx.Tx) error {
				if err := store.createPurchasePlan(ctx, tx, plan); err != nil {
					return err
				}
				if err := store.setPlanAccountsTx(ctx, tx, plan.ID, []string{account.ID, account.ID}); err != nil {
					return err
				}
				var plans, accounts int
				require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM purchase_plans WHERE id=$1`, plan.ID).Scan(&plans))
				require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM plan_accounts WHERE plan_id=$1`, plan.ID).Scan(&accounts))
				require.Equal(t, 1, plans)
				require.Equal(t, 1, accounts)
				if !commit {
					return abort
				}
				return nil
			})
			if !commit {
				require.ErrorIs(t, err, abort)
				var plans, accounts int
				require.NoError(t, store.db.QueryRow(ctx, `SELECT count(*) FROM purchase_plans WHERE id=$1`, plan.ID).Scan(&plans))
				require.NoError(t, store.db.QueryRow(ctx, `SELECT count(*) FROM plan_accounts WHERE plan_id=$1`, plan.ID).Scan(&accounts))
				require.Zero(t, plans, "outer rollback must remove the new plan")
				require.Zero(t, accounts, "outer rollback must remove the new associations")
				return
			}
			require.NoError(t, err)
			got, err := store.GetPurchasePlan(ctx, plan.ID)
			require.NoError(t, err)
			require.NotEmpty(t, plan.ID)
			require.Equal(t, plan.Services, got.Services)
			require.Equal(t, plan.RampSchedule, got.RampSchedule)
			require.Equal(t, plan.Name, got.Name)
			require.Equal(t, plan.Enabled, got.Enabled)
			require.Equal(t, plan.AutoPurchase, got.AutoPurchase)
			require.Equal(t, plan.NotificationDaysBefore, got.NotificationDaysBefore)
			require.Equal(t, plan.MaxStepCommitment, got.MaxStepCommitment)
			require.WithinDuration(t, plan.CreatedAt, got.CreatedAt, time.Microsecond)
			require.True(t, plan.UpdatedAt.Equal(got.UpdatedAt))
			for _, date := range []*time.Time{got.NextExecutionDate, got.LastExecutionDate, got.LastNotificationSent} {
				require.NotNil(t, date)
				require.True(t, stamp.Equal(*date))
			}
			accounts, err := store.GetPlanAccounts(ctx, plan.ID)
			require.NoError(t, err)
			require.Len(t, accounts, 1)
			require.Equal(t, account.ID, accounts[0].ID)
			var baseline []byte
			require.NoError(t, store.db.QueryRow(ctx, `SELECT ramp_baseline FROM purchase_plans WHERE id=$1`, plan.ID).Scan(&baseline))
			require.Nil(t, baseline)
			duplicate := *plan
			duplicate.Name = "must not replace original"
			var pgErr *pgconn.PgError
			require.ErrorAs(t, store.CreatePurchasePlan(ctx, &duplicate), &pgErr)
			require.Equal(t, "23505", pgErr.Code)
			after, err := store.GetPurchasePlan(ctx, plan.ID)
			require.NoError(t, err)
			require.Equal(t, got, after)
		})
	}
}

func TestPlanWriteBodiesAssociationCompatibility(t *testing.T) {
	ctx := context.Background()
	store := setupRampStepStore(ctx, t)
	plan := &PurchasePlan{ID: uuid.NewString(), Name: "existing plan", Services: map[string]ServiceConfig{"aws/ec2": {Provider: "aws", Service: "ec2", Enabled: true}}}
	id := plan.ID
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))
	require.Equal(t, id, plan.ID)
	ids := make([]string, 0, 3)
	for _, provider := range []string{"aws", "aws", "azure"} {
		account := &CloudAccount{Name: provider, Provider: provider, ExternalID: uuid.NewString()}
		require.NoError(t, store.CreateCloudAccount(ctx, account))
		ids = append(ids, account.ID)
	}
	require.NoError(t, store.SetPlanAccounts(ctx, plan.ID, ids[:1]))
	require.NoError(t, store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := store.SetPlanRampBaselineTx(ctx, tx, plan.ID, PlanRampBaseline{baselineKey(t, baselineRec(ids[0], "m5.large", 7)): {Count: 7}})
		return err
	}))
	_, err := store.db.Exec(ctx, `UPDATE purchase_plans SET services=services || '{"future":{"unknown":true}}'::jsonb, ramp_schedule='{"type":"weekly","current_step":2,"future":"retained"}'::jsonb WHERE id=$1`, plan.ID)
	require.NoError(t, err)
	var before string
	require.NoError(t, store.db.QueryRow(ctx, `SELECT row_to_json(p)::text FROM purchase_plans p WHERE id=$1`, plan.ID).Scan(&before))
	for _, useWrapper := range []bool{true, false} {
		set := func(planID string, accounts []string) error {
			if useWrapper {
				return store.SetPlanAccounts(ctx, planID, accounts)
			}
			return store.WithTx(ctx, func(tx pgx.Tx) error { return store.setPlanAccountsTx(ctx, tx, planID, accounts) })
		}
		require.NoError(t, set(plan.ID, []string{ids[1], ids[1]}))
		for _, invalid := range []struct {
			planID   string
			accounts []string
			missing  bool
		}{
			{plan.ID, []string{ids[2]}, false},
			{plan.ID, []string{uuid.NewString()}, true},
			{uuid.NewString(), nil, true},
		} {
			err := set(invalid.planID, invalid.accounts)
			if invalid.missing {
				require.ErrorIs(t, err, ErrNotFound)
			} else {
				require.ErrorContains(t, err, "plan provider mismatch:")
			}
			accounts, err := store.GetPlanAccounts(ctx, plan.ID)
			require.NoError(t, err)
			require.Len(t, accounts, 1)
			require.Equal(t, ids[1], accounts[0].ID)
		}
		require.NoError(t, set(plan.ID, nil))
		accounts, err := store.GetPlanAccounts(ctx, plan.ID)
		require.NoError(t, err)
		require.Empty(t, accounts)
		var after string
		require.NoError(t, store.db.QueryRow(ctx, `SELECT row_to_json(p)::text FROM purchase_plans p WHERE id=$1`, plan.ID).Scan(&after))
		require.Equal(t, before, after, "associations must preserve raw plan, baseline and progress")
	}
}
