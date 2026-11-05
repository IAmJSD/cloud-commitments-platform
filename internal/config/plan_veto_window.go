package config

import (
	"errors"
	"fmt"
	"time"
)

// ErrNoVetoWindow marks a plan whose notification window is empty: with
// NotificationDaysBefore < 1 the email goes out when the purchase is already
// due, so the human veto the notified status stands for has no time to happen.
var ErrNoVetoWindow = errors.New("plan has no veto window")

// RequireVetoWindow refuses a plan that cannot give an emailed purchase at
// least one day before it runs. Attaching recommendations to a plan step and
// re-scheduling a resumed recommendation-bearing row both call it.
func (p *PurchasePlan) RequireVetoWindow() error {
	if p.NotificationDaysBefore < 1 {
		return fmt.Errorf("%w: plan %s has notification_days_before=%d, need at least 1", ErrNoVetoWindow, p.ID, p.NotificationDaysBefore)
	}
	return nil
}

// VetoWindowEnd is the earliest time a freshly emailed step may be bought:
// NotificationDaysBefore fixed 24h days from now, in UTC. Fixed hours, not
// calendar days, so a non-UTC time.Local or a DST change cannot shift it; this
// matches the notification tick's own arithmetic (notifications.go).
func (p *PurchasePlan) VetoWindowEnd(now time.Time) time.Time {
	return now.UTC().Add(time.Duration(p.NotificationDaysBefore) * 24 * time.Hour)
}
