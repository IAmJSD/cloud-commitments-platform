package api

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// startDateProbePlanID is the plan the start_date regression tests post to.
const startDateProbePlanID = "11111111-1111-1111-1111-111111111111"

// postPlannedPurchaseAt sends POST /api/plans/{id}/purchases through the real
// router with the handler clock pinned to now, and returns the response plus
// the store mock so callers can check whether any execution row was written.
// The execution and plan writes are expected only when the date is accepted.
func postPlannedPurchaseAt(t *testing.T, now time.Time, startDate string, accepted bool) (*events.LambdaFunctionURLResponse, *MockConfigStore) {
	t.Helper()
	return postPlannedPurchaseForPlan(t, now, startDate, accepted, nil)
}

// postPlannedPurchaseForPlan is postPlannedPurchaseAt with a hook that edits the
// fixture plan before the request is sent.
func postPlannedPurchaseForPlan(t *testing.T, now time.Time, startDate string, accepted bool, edit func(*config.PurchasePlan)) (*events.LambdaFunctionURLResponse, *MockConfigStore) {
	t.Helper()
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })

	// The admin X-API-Key authenticates the request, so the bearer session is
	// never validated.
	mockAuth.grantAdmin()

	plan := bareStepAPIPlan()
	plan.ID = startDateProbePlanID
	plan.AutoPurchase = false
	if edit != nil {
		edit(plan)
	}
	// Maybe: a malformed date is refused before the plan is read; a
	// well-formed past date is refused after it.
	mockStore.On("GetPurchasePlan", mock.Anything, startDateProbePlanID).Return(plan, nil).Maybe()
	mockStore.On("GetPlanAccounts", mock.Anything, startDateProbePlanID).Return([]config.CloudAccount{{Provider: "azure"}}, nil).Maybe()
	if accepted {
		mockStore.On("SavePurchaseExecution", mock.Anything, mock.Anything).Return(nil)
		mockStore.On("UpdatePurchasePlan", mock.Anything, mock.Anything).Return(nil)
	}

	handler := &Handler{
		config:            mockStore,
		auth:              mockAuth,
		corsAllowedOrigin: "*",
		apiKey:            "test-key",
		now:               func() time.Time { return now },
	}
	req := &events.LambdaFunctionURLRequest{
		Headers: map[string]string{
			"X-API-Key":     "test-key",
			"Content-Type":  "application/json",
			"Authorization": "Bearer test-token",
			"X-CSRF-Token":  "test-csrf",
		},
		Body: `{"count": 1, "start_date": "` + startDate + `"}`,
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method: "POST",
				Path:   "/api/plans/" + startDateProbePlanID + "/purchases",
			},
		},
	}

	resp, err := handler.HandleRequest(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp, mockStore
}

// TestCreatePlannedPurchases_StartDateMustBeInTheFuture pins the server-side
// start_date guard added in #873 (issue #50). start_date is a date-only value
// parsed as midnight UTC and must be after the current instant, so today's
// date (UTC) and anything earlier are refused with a 400; tomorrow (UTC) is
// the first accepted date. The clock is injected so the UTC-midnight boundary
// is exercised deterministically.
func TestCreatePlannedPurchases_StartDateMustBeInTheFuture(t *testing.T) {
	midday := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	losAngeles, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)

	cases := []struct {
		name      string
		now       time.Time
		startDate string
		status    int
		message   string
	}{
		{"yesterday is refused", midday, "2026-03-09", 400, "start_date must be in the future"},
		{"long past date is refused", midday, "2020-01-01", 400, "start_date must be in the future"},
		{"today is refused", midday, "2026-03-10", 400, "start_date must be in the future"},
		// bareStepAPIPlan has a 2-day notice window, so D5 (#631) refuses dates
		// before now+3d and accepts the first midnight at or after it.
		{"tomorrow is inside the notice margin", midday, "2026-03-11", 409, noticeMarginMessage("2026-03-14")},
		{"three days out is still inside the margin", midday, "2026-03-13", 409, noticeMarginMessage("2026-03-14")},
		{"the first date past the margin is accepted", midday, "2026-03-14", 200, ""},
		{"malformed date keeps the format error", midday, "03/11/2026", 400, "invalid start_date format, expected YYYY-MM-DD"},
		{"today is refused one second after UTC midnight", time.Date(2026, 3, 10, 0, 0, 1, 0, time.UTC), "2026-03-10", 400, "start_date must be in the future"},
		{"the first date past the margin is accepted one second before UTC midnight", time.Date(2026, 3, 10, 23, 59, 59, 0, time.UTC), "2026-03-14", 200, ""},
		{"the date is refused exactly at its own UTC midnight", time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC), "2026-03-11", 400, "start_date must be in the future"},
		// 20:00 on 10 March in Los Angeles is 03:00 UTC on 11 March. The
		// caller's local "tomorrow" is already today in UTC, so it is refused:
		// the comparison is in UTC, not in the clock's or the server's zone.
		{"UTC decides, not the local zone", time.Date(2026, 3, 10, 20, 0, 0, 0, losAngeles), "2026-03-11", 400, "start_date must be in the future"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, store := postPlannedPurchaseAt(t, tc.now, tc.startDate, tc.status == 200)
			assert.Equal(t, tc.status, resp.StatusCode, "body: %s", resp.Body)
			if tc.status == 200 {
				assert.JSONEq(t, `{"created":1}`, resp.Body)
				store.AssertNumberOfCalls(t, "SavePurchaseExecution", 1)
				return
			}
			if tc.status == 409 {
				assert.JSONEq(t, `{"error":"`+tc.message+`","code":"start_date_inside_notice_margin"}`, resp.Body)
			} else {
				assert.JSONEq(t, `{"error":"`+tc.message+`"}`, resp.Body)
			}
			store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "UpdatePurchasePlan", mock.Anything, mock.Anything)
		})
	}
}

// noticeMarginMessage is the fixed D5 refusal text for the first accepted date.
func noticeMarginMessage(earliest string) string {
	return "start_date leaves too little notice before the first purchase; choose a start date on or after " + earliest
}

// TestCreatePlannedPurchases_StartDateNoticeMargin pins D5 of #631: a start date
// D is refused with a 409 when D < now + (N+1) x 24h. The tick sees a root only
// when D-now < (N+1)d and attaches only when D >= now+N d, so a root attaches
// from a tick in the 24h slice (D-(N+1)d, D-Nd]; D >= now+(N+1)d keeps that
// whole slice in the future. N is 2 here. For each clock the latest midnight
// below now+3d is refused and the first midnight at or after it is accepted;
// the message names that first accepted midnight (a ceiling, so a threshold
// that is itself a midnight names that day, not the next).
func TestCreatePlannedPurchases_StartDateNoticeMargin(t *testing.T) {
	for _, tc := range []struct {
		name             string
		now              time.Time
		refused, firstOK string
	}{
		{"one second before UTC midnight", time.Date(2026, 3, 10, 23, 59, 59, 0, time.UTC), "2026-03-13", "2026-03-14"},
		{"one second after UTC midnight", time.Date(2026, 3, 10, 0, 0, 1, 0, time.UTC), "2026-03-13", "2026-03-14"},
		{"exactly at UTC midnight", time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), "2026-03-12", "2026-03-13"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, store := postPlannedPurchaseAt(t, tc.now, tc.refused, false)
			assert.Equal(t, 409, resp.StatusCode, "body: %s", resp.Body)
			assert.JSONEq(t, `{"error":"`+noticeMarginMessage(tc.firstOK)+`","code":"start_date_inside_notice_margin"}`, resp.Body)
			store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "UpdatePurchasePlan", mock.Anything, mock.Anything)

			resp, _ = postPlannedPurchaseAt(t, tc.now, tc.firstOK, true)
			assert.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
		})
	}
}

// TestCreatePlannedPurchases_NoNoticeWindowIsRefused pins that a plan with
// notification_days_before 0 can't schedule purchases at all, however far out
// the start date is. Attach refuses every root of such a plan (RequireVetoWindow),
// so each one would fail on its date; validateScheduledRamp returns the 409
// before the D5 margin is computed.
func TestCreatePlannedPurchases_NoNoticeWindowIsRefused(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	resp, store := postPlannedPurchaseForPlan(t, now, "2026-06-01", false, func(p *config.PurchasePlan) {
		p.NotificationDaysBefore = 0
	})
	assert.Equal(t, 409, resp.StatusCode, "body: %s", resp.Body)
	assert.Contains(t, resp.Body, "notification_days_before=0, need at least 1")
	store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "UpdatePurchasePlan", mock.Anything, mock.Anything)
}

// TestHandlerClock_DefaultsToWallClock checks that a Handler built without an
// injected clock (every production constructor) still uses time.Now.
func TestHandlerClock_DefaultsToWallClock(t *testing.T) {
	before := time.Now()
	got := (&Handler{}).clock()
	assert.False(t, got.Before(before), "clock() must not be earlier than time.Now() taken before it")
	assert.WithinDuration(t, time.Now(), got, time.Minute)
}
