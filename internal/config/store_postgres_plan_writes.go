package config

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *PostgresStore) createPurchasePlan(ctx context.Context, executor globalConfigExecutor, plan *PurchasePlan) error {
	if plan.ID == "" {
		plan.ID = uuid.New().String()
	}

	now := time.Now()
	plan.CreatedAt = now
	plan.UpdatedAt = now

	servicesJSON, err := json.Marshal(plan.Services)
	if err != nil {
		return fmt.Errorf("failed to marshal services: %w", err)
	}

	rampScheduleJSON, err := json.Marshal(plan.RampSchedule)
	if err != nil {
		return fmt.Errorf("failed to marshal ramp_schedule: %w", err)
	}

	query := `
		INSERT INTO purchase_plans (
			id, name, enabled, auto_purchase, notification_days_before,
			services, ramp_schedule, created_at, updated_at,
			next_execution_date, last_execution_date, last_notification_sent,
			max_step_commitment, ramp_ledger_state
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'new')
		RETURNING updated_at
	`

	var updatedAt time.Time
	err = executor.QueryRow(ctx, query,
		plan.ID,
		plan.Name,
		plan.Enabled,
		plan.AutoPurchase,
		plan.NotificationDaysBefore,
		servicesJSON,
		rampScheduleJSON,
		plan.CreatedAt,
		plan.UpdatedAt,
		plan.NextExecutionDate,
		plan.LastExecutionDate,
		plan.LastNotificationSent,
		plan.MaxStepCommitment,
	).Scan(&updatedAt)

	if err != nil {
		return fmt.Errorf("failed to create purchase plan: %w", err)
	}
	plan.UpdatedAt = updatedAt
	return nil
}

func (s *PostgresStore) setPlanAccountsTx(ctx context.Context, tx pgx.Tx, planID string, accountIDs []string) error {
	if err := s.validatePlanAccountProvidersTx(ctx, tx, planID, accountIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM plan_accounts WHERE plan_id = $1`, planID); err != nil {
		return fmt.Errorf("failed to clear plan accounts: %w", err)
	}
	for _, accountID := range accountIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO plan_accounts (plan_id, account_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			planID, accountID,
		); err != nil {
			return fmt.Errorf("failed to insert plan account: %w", err)
		}
	}
	return nil
}
