package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// requireManagedPlanUpdateTx refuses a legacy plan write that would change what
// the schedule editor owns on an editor-managed plan. old is the row locked by
// the caller's transaction; judging it, never a pre-lock copy, is what lets a
// writer that waited behind the editor see the committed marker. Only the
// editor command sets or clears dates_managed (it is part of ramp_schedule, so
// adopting or stripping it is refused as a ramp change); it does not come
// through here.
func requireManagedPlanUpdateTx(old, next *PurchasePlan) error {
	if old == nil || (!old.RampSchedule.DatesManaged && !next.RampSchedule.DatesManaged) {
		return nil
	}
	if field := managedPlanSensitiveChange(old, next); field != "" {
		return fmt.Errorf("%w: %s changed on plan %s", ErrEditorReconciliationRequired, field, old.ID)
	}
	return nil
}

// managedPlanSensitiveChange names the first editor-owned field whose
// persisted value differs, or "" when only name, enabled, disarming, the cap
// and the bookkeeping timestamps changed.
func managedPlanSensitiveChange(old, next *PurchasePlan) string {
	switch {
	case !sameJSON(old.RampSchedule, next.RampSchedule):
		return "ramp_schedule"
	case !sameJSON(old.Services, next.Services):
		return "services"
	case !sameDate(old.NextExecutionDate, next.NextExecutionDate):
		return "next_execution_date"
	case old.NotificationDaysBefore != next.NotificationDaysBefore:
		return "notification_days_before"
	case !old.AutoPurchase && next.AutoPurchase:
		return "auto_purchase"
	}
	return ""
}

// sameJSON compares what would be persisted: nil and empty slices marshal
// equal under omitempty, so a rebuilt but identical value is not a change.
func sameJSON(a, b any) bool {
	aj, aErr := json.Marshal(a)
	bj, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aj, bj)
}

// sameDate compares instants; both sides come from the database or a copy of it.
func sameDate(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// requireManagedAccountsTx refuses a changed account set on an editor-managed
// plan. The plan row is read FOR SHARE, the lock SetPlanAccounts already takes
// for its provider check, so an editor command holding FOR UPDATE makes the
// writer wait and then see the committed marker. An identical set passes: the
// legacy edit modal re-sends the unchanged selection after every rename.
func requireManagedAccountsTx(ctx context.Context, tx pgx.Tx, planID string, accountIDs []string) error {
	var managed bool
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(ramp_schedule->>'dates_managed' = 'true', false) FROM purchase_plans WHERE id = $1 FOR SHARE`,
		planID).Scan(&managed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !managed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check plan management: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT account_id FROM plan_accounts WHERE plan_id = $1`, planID)
	if err != nil {
		return fmt.Errorf("failed to read plan accounts: %w", err)
	}
	current, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("failed to read plan accounts: %w", err)
	}
	same := sameAccountSet(current, accountIDs)
	if !same {
		return fmt.Errorf("%w: account set changed on plan %s", ErrEditorReconciliationRequired, planID)
	}
	return nil
}

// sameAccountSet compares account id lists as sets, so request duplicates
// collapse the way ON CONFLICT DO NOTHING collapses them on write. Ids are
// lower-cased because the uuid column returns lower-case text while request
// validation accepts upper-case hex.
func sameAccountSet(a, b []string) bool {
	set := func(ids []string) map[string]struct{} {
		m := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			m[strings.ToLower(id)] = struct{}{}
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		return false
	}
	for id := range sa {
		if _, ok := sb[id]; !ok {
			return false
		}
	}
	return true
}
