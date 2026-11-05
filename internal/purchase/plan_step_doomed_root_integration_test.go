//go:build integration

package purchase

import (
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// doomedRootCase is one plan with one root-shaped row at its next step. The read
// returns one flag per plan, so every case owns its plan and a probe on any one
// condition flips exactly one case.
type doomedRootCase struct {
	name    string
	days    int           // notification_days_before
	managed bool          // ramp_schedule.dates_managed
	step    int           // execution step_number; 0 means the plan's next step (1)
	offset  time.Duration // scheduled_date relative to now
	status  string        // "" means pending
	account bool          // child row of a fanned-out step
	retry   int           // retry_attempt_n
	recs    string        // "nil" (jsonb null, as Add Purchases writes), "empty" ('[]') or "full"
	want    bool
}

func TestDoomedNextRoots_FlagsExactlyTheCertainlyDoomed(t *testing.T) {
	f := newB3C1Fixture(t, false, false, b3C1Root{})
	day := 24 * time.Hour
	cases := []doomedRootCase{
		{name: "flag_nil_recs_inside_window", days: 3, offset: day, recs: "nil", want: true},
		{name: "flag_empty_array_inside_window", days: 3, offset: day, recs: "empty", want: true},
		{name: "flag_notified_status", days: 3, offset: day, status: "notified", recs: "nil", want: true},
		{name: "flag_due_exactly_now", days: 3, offset: 0, recs: "nil", want: true},
		{name: "flag_one_microsecond_inside_window", days: 3, offset: 3*day - time.Microsecond, recs: "nil", want: true},
		{name: "flag_no_notice_window_far_future", days: 0, offset: 30 * day, recs: "nil", want: true},

		{name: "skip_exactly_at_window_end", days: 3, offset: 3 * day, recs: "nil"},
		{name: "skip_beyond_window", days: 3, offset: 4 * day, recs: "nil"},
		{name: "skip_child_row_with_account", days: 3, offset: day, account: true, recs: "nil"},
		{name: "skip_retry_row", days: 3, offset: day, retry: 1, recs: "nil"},
		{name: "skip_overdue_by_one_second", days: 3, offset: -time.Second, recs: "nil"},
		{name: "skip_failed_root", days: 3, offset: day, status: "failed", recs: "nil"},
		{name: "skip_dates_managed_plan", days: 3, offset: day, managed: true, recs: "nil"},
		{name: "skip_attached_recommendations", days: 3, offset: day, recs: "full"},
		{name: "skip_not_the_next_step", days: 3, offset: day, step: 2, recs: "nil"},
	}
	ids := make(map[string]string, len(cases))
	for _, tc := range cases {
		ids[tc.name] = seedDoomedRootCase(f, tc)
	}

	got, err := f.store.GetDoomedNextRoots(f.ctx, f.now)
	require.NoError(t, err)
	for _, tc := range cases {
		block, flagged := got[ids[tc.name]]
		require.Equal(t, tc.want, flagged, tc.name)
		if tc.want {
			require.Equal(t, 1, block.StepNumber, tc.name)
		}
	}
	_, flagged := got[f.planID]
	require.False(t, flagged, "the fixture plan has no pending root at its next step")
}

func seedDoomedRootCase(f *b3C1Fixture, tc doomedRootCase) string {
	f.t.Helper()
	next := f.now.Add(24 * time.Hour)
	plan := &config.PurchasePlan{
		Name: "doomed-" + tc.name, Enabled: true, AutoPurchase: true, NotificationDaysBefore: tc.days, MaxStepCommitment: f64(1e9),
		Services: map[string]config.ServiceConfig{"ec2": {Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: cumCoverage}},
		RampSchedule: config.RampSchedule{Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7, TotalSteps: 4,
			StartDate: next.AddDate(0, 0, -7), DatesManaged: tc.managed},
		NextExecutionDate: &next,
	}
	require.NoError(f.t, plan.Validate())
	require.NoError(f.t, f.store.CreatePurchasePlan(f.ctx, plan))
	step := tc.step
	if step == 0 {
		step = 1
	}
	exec := &config.PurchaseExecution{PlanID: plan.ID, ExecutionID: uuid.NewString(), StepNumber: step,
		Status: "pending", ScheduledDate: f.now.Add(tc.offset), RetryAttemptN: tc.retry}
	if tc.status != "" {
		exec.Status = tc.status
	}
	if tc.account {
		exec.CloudAccountID = &f.account
	}
	switch tc.recs {
	case "empty":
		exec.Recommendations = []config.RecommendationRecord{}
	case "full":
		exec.Recommendations = []config.RecommendationRecord{cumRec(f.account, "m5.t0", 1)}
	}
	require.NoError(f.t, f.store.SavePurchaseExecution(f.ctx, exec))
	var stored string
	require.NoError(f.t, f.pool.QueryRow(f.ctx, `SELECT recommendations::text FROM purchase_executions WHERE execution_id = $1`, exec.ExecutionID).Scan(&stored))
	switch tc.recs {
	case "nil":
		require.Equal(f.t, "null", stored, tc.name+": Add Purchases roots store jsonb null")
	case "empty":
		require.Equal(f.t, "[]", stored, tc.name)
	}
	return plan.ID
}
