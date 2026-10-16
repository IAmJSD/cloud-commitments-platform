package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
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
