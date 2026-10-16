package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgresStore) persistManagedRampPosition(ctx context.Context, tx pgx.Tx, plan *PurchasePlan) error {
	next, err := s.planScheduleNextDateTx(ctx, tx, plan)
	if err != nil {
		return err
	}
	now := time.Now()
	var updated time.Time
	err = tx.QueryRow(ctx, `UPDATE purchase_plans SET
		ramp_schedule = jsonb_set(ramp_schedule, '{current_step}', to_jsonb($2::int)),
		next_execution_date = $3, last_execution_date = $4
		WHERE id = $1 AND updated_at = $5 RETURNING updated_at`,
		plan.ID, plan.RampSchedule.CurrentStep, next, now, plan.UpdatedAt).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPurchasePlanConflict
	}
	if err != nil {
		return fmt.Errorf("persist managed ramp position: %w", err)
	}
	plan.NextExecutionDate, plan.LastExecutionDate, plan.UpdatedAt = next, &now, updated
	return nil
}

func (s *PostgresStore) planScheduleNextDateTx(ctx context.Context, tx pgx.Tx, plan *PurchasePlan) (*time.Time, error) {
	if plan.RampSchedule.IsComplete() {
		return nil, nil
	}
	if !plan.RampSchedule.DatesManaged {
		next := plan.RampSchedule.GetNextPurchaseDate()
		return &next, nil
	}
	root, err := s.managedPlanNextRootTx(ctx, tx, plan)
	if err != nil || root == nil {
		return nil, err
	}
	return &root.ScheduledDate, nil
}

func (s *PostgresStore) managedPlanNextRootTx(ctx context.Context, tx pgx.Tx, plan *PurchasePlan) (*PurchaseExecution, error) {
	rows, err := s.ListExecutionsForPlanStepTx(ctx, tx, plan.ID, plan.RampSchedule.CurrentStep+1)
	if err != nil {
		return nil, err
	}
	root, err := managedPlanNextRoot(plan, rows)
	if err != nil || root == nil {
		return nil, err
	}
	var incoming bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM purchase_executions WHERE retry_execution_id = $1)`, root.ExecutionID).Scan(&incoming); err != nil {
		return nil, fmt.Errorf("check managed root retry references: %w", err)
	}
	if incoming {
		return nil, nil
	}
	return root, nil
}

func managedPlanNextRoot(plan *PurchasePlan, rows []PurchaseExecution) (*PurchaseExecution, error) {
	var root *PurchaseExecution
	blocked := false
	for i := range rows {
		row := &rows[i]
		if row.StepNumber != plan.RampSchedule.CurrentStep+1 {
			continue
		}
		if row.Status == StatusCanceled || row.Status == LegacyStatusCanceled {
			blocked = blocked || managedRootPriorAction(row)
			continue
		}
		if !managedRootIdentity(row) {
			blocked = true
			continue
		}
		if root != nil {
			return nil, fmt.Errorf("%w: duplicate managed roots for step %d", ErrPurchasePlanConflict, row.StepNumber)
		}
		root = row
	}
	if blocked || !managedRootClockAvailable(root) {
		return nil, nil
	}
	return root, nil
}

func managedRootIdentity(row *PurchaseExecution) bool {
	lineage := row.IdempotencyKey
	if lineage == "" {
		lineage = row.ExecutionID
	}
	return row.CloudAccountID == nil && row.RetryAttemptN == 0 && row.RetryExecutionID == nil && !strings.Contains(lineage, ":")
}

func managedRootPriorAction(row *PurchaseExecution) bool {
	if row.ExecutedAt != nil || row.CompletedAt != nil || row.PreApprovalSkipReason != nil && *row.PreApprovalSkipReason != "" {
		return true
	}
	for i := range row.Recommendations {
		if row.Recommendations[i].Purchased {
			return true
		}
	}
	return false
}

func managedRootClockAvailable(row *PurchaseExecution) bool {
	if row == nil || row.ScheduledDate.IsZero() || managedRootPriorAction(row) {
		return false
	}
	switch row.Status {
	case "pending", "notified":
		return row.ScheduledExecutionAt == nil
	case "scheduled":
		return row.ScheduledExecutionAt != nil && !row.ScheduledExecutionAt.IsZero()
	}
	return false
}
