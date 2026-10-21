package purchase

import (
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/require"
)

func TestB3C1VetoHelpers(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		offset time.Duration
		clamp  bool
	}{
		{"equal", 0, false},
		{"microsecond_before", -time.Microsecond, true},
		{"microsecond_after", time.Microsecond, false},
		{"half_day_before", -12 * time.Hour, true},
		{"half_day_after", 12 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			date := end.Add(tc.offset)
			plan := config.PurchasePlan{NotificationDaysBefore: 3, NextExecutionDate: &date}
			before := plan
			beforeDate := *plan.NextExecutionDate
			before.NextExecutionDate = &beforeDate
			require.True(t, end.Equal(vetoWindowEnd(&plan, now)))
			want := date
			if tc.clamp {
				want = end
			}
			require.True(t, want.Equal(attachScheduledDate(&plan, now)))
			require.Equal(t, before, plan, "pure helpers preserve the caller's clock")
		})
	}
}

func TestB3C1OverdueHelpers(t *testing.T) {
	date := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	plan := config.PurchasePlan{Enabled: true, AutoPurchase: true, NotificationDaysBefore: 3, NextExecutionDate: &date,
		Services: map[string]config.ServiceConfig{
			"ec2":   {Provider: "aws", Enabled: true},
			"vm":    {Provider: "azure", Enabled: true},
			"spare": {Provider: "gcp", Enabled: false},
		}}
	global := &config.GlobalConfig{GracePeriodDays: map[string]int{"aws": 2, "azure": 5, "gcp": 90}}
	require.Equal(t, 5, planGraceDays(&plan, global), "disabled larger grace cannot extend the window")
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"entry_before", date.Add(-time.Microsecond), false},
		{"entry_equal", date, false},
		{"entry_after", date.Add(time.Microsecond), true},
		{"exit_before", cutoff.Add(-time.Microsecond), true},
		{"exit_equal", cutoff, false},
		{"exit_after", cutoff.Add(time.Microsecond), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, shouldRetryOverduePlan(plan, true, global, tc.now))
		})
	}
}

func TestB3C1NotifyBuckets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hours      int
		low, high  float64
		want, none bool
	}{
		{"n_minus_half", 60, 48, 72, true, false},
		{"n_plus_half", 84, 72, 96, true, false},
		{"n_plus_one_half", 108, 96, 120, false, false},
		{"negative_half", -12, -24, 0, true, false},
		{"negative_one_half", -36, -48, -24, false, false},
		{"nil", 0, 0, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			date := time.Now().UTC().Add(time.Duration(tc.hours) * time.Hour)
			plan := config.PurchasePlan{Enabled: true, AutoPurchase: true, NotificationDaysBefore: 3, NextExecutionDate: &date}
			if tc.none {
				plan.NextExecutionDate = nil
			}
			before := time.Now()
			got := (&Manager{}).shouldNotifyPlan(plan, true)
			after := time.Now()
			if !tc.none {
				for _, clock := range []time.Time{before, after} {
					require.Greater(t, date.Sub(clock).Hours(), tc.low, "inconclusive: crossed the bucket boundary")
					require.Less(t, date.Sub(clock).Hours(), tc.high, "inconclusive: crossed the bucket boundary")
				}
			}
			require.Equal(t, tc.want, got)
		})
	}
}
