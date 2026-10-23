package api

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Fixture rows (mock store): nothing attaches recommendations to a plan step
// yet, so rec-bearing rows are built by hand.

const notifiedExecID = "11111111-1111-1111-1111-111111111111"

func notifiedAdminHandler(ctx context.Context) (*Handler, *MockConfigStore, *events.LambdaFunctionURLRequest) {
	store := new(MockConfigStore)
	auth := new(MockAuthService)
	auth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "admin@example.com"}, nil)
	auth.grantAdmin()
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}}
	return &Handler{config: store, auth: auth}, store, req
}

func TestPausePlannedPurchase_AcceptsNotifiedRow(t *testing.T) {
	ctx := context.Background()
	handler, store, req := notifiedAdminHandler(ctx)
	store.On("GetExecutionByID", ctx, notifiedExecID).Return(&config.PurchaseExecution{ExecutionID: notifiedExecID, Status: "notified"}, nil)
	store.On("TransitionExecutionStatus", ctx, notifiedExecID, []string{"pending", "notified"}, "paused", mock.Anything).
		Return(&config.PurchaseExecution{ExecutionID: notifiedExecID, Status: "paused"}, nil)

	res, err := handler.pausePlannedPurchase(ctx, req, notifiedExecID)
	require.NoError(t, err)
	assert.Equal(t, "paused", res.Status)
}

func TestResumePlannedPurchase_PastScheduledDate(t *testing.T) {
	ctx := context.Background()
	recs := []config.RecommendationRecord{{ID: "rec-1", Count: 1}}
	cases := []struct {
		name      string
		recs      []config.RecommendationRecord
		date      time.Time
		wantMoved bool
	}{
		{"rec-bearing past date is moved", recs, time.Now().Add(-48 * time.Hour), true},
		{"rec-bearing future date is kept", recs, time.Now().Add(72 * time.Hour), false},
		{"empty row past date is kept", nil, time.Now().Add(-48 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, store, req := notifiedAdminHandler(ctx)
			row := &config.PurchaseExecution{ExecutionID: notifiedExecID, PlanID: "plan-1", Status: "pending", Recommendations: tc.recs, ScheduledDate: tc.date}
			store.On("GetExecutionByID", ctx, notifiedExecID).Return(row, nil).Maybe()
			store.On("TransitionExecutionStatus", ctx, notifiedExecID, []string{"paused"}, "pending", mock.Anything).Return(row, nil)
			store.On("GetPurchasePlan", ctx, "plan-1").Return(&config.PurchasePlan{ID: "plan-1", NotificationDaysBefore: 3}, nil).Maybe()
			var movedTo *time.Time
			store.On("RescheduleAwaitingExecution", ctx, notifiedExecID, mock.Anything).Run(func(a mock.Arguments) {
				d := a.Get(2).(time.Time)
				movedTo = &d
			}).Return(true, nil).Maybe()

			before := time.Now()
			_, err := handler.resumePlannedPurchase(ctx, req, notifiedExecID)
			require.NoError(t, err)
			if !tc.wantMoved {
				assert.Nil(t, movedTo)
				return
			}
			require.NotNil(t, movedTo)
			assert.WithinDuration(t, before.AddDate(0, 0, 3), *movedTo, time.Minute)
			store.AssertNotCalled(t, "SavePurchaseExecution", mock.Anything, mock.Anything)
		})
	}
}

func TestCancelOrRecoverExecution_ReleasesSuppressionsOfNotifiedRow(t *testing.T) {
	ctx := context.Background()
	handler, store, _ := notifiedAdminHandler(ctx)
	store.On("TransitionExecutionStatus", ctx, notifiedExecID, []string{"pending", "notified", "paused"}, "canceled", mock.Anything).
		Return(&config.PurchaseExecution{ExecutionID: notifiedExecID, Status: "canceled"}, nil)
	store.On("DeleteSuppressionsByExecution", ctx, notifiedExecID).Return(nil)

	_, err := handler.cancelOrRecoverExecution(ctx, notifiedExecID, nil)
	require.NoError(t, err)
	store.AssertCalled(t, "DeleteSuppressionsByExecution", ctx, notifiedExecID)
}

// S13 on the resume path: a plan without a veto window must not have a due
// recommendation-bearing row moved to "now"; nothing is changed.
func TestResumePlannedPurchase_RefusesWithoutVetoWindow(t *testing.T) {
	ctx := context.Background()
	recs := []config.RecommendationRecord{{ID: "rec-1", Count: 1}}
	for _, tc := range []struct {
		name    string
		recs    []config.RecommendationRecord
		date    time.Time
		refused bool
	}{
		{"rec-bearing past date, 0 days", recs, time.Now().Add(-time.Hour), true},
		{"rec-bearing future date, 0 days", recs, time.Now().Add(time.Hour), false},
		{"empty row past date, 0 days", nil, time.Now().Add(-time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, store, req := notifiedAdminHandler(ctx)
			row := &config.PurchaseExecution{ExecutionID: notifiedExecID, PlanID: "plan-1", Status: "paused", Recommendations: tc.recs, ScheduledDate: tc.date}
			store.On("GetExecutionByID", ctx, notifiedExecID).Return(row, nil)
			store.On("GetPurchasePlan", ctx, "plan-1").Return(&config.PurchasePlan{ID: "plan-1", NotificationDaysBefore: 0}, nil).Maybe()
			store.On("TransitionExecutionStatus", ctx, notifiedExecID, []string{"paused"}, "pending", mock.Anything).Return(row, nil).Maybe()
			store.On("RescheduleAwaitingExecution", ctx, notifiedExecID, mock.Anything).Return(true, nil).Maybe()

			_, err := handler.resumePlannedPurchase(ctx, req, notifiedExecID)
			if !tc.refused {
				require.NoError(t, err)
				return
			}
			ce, ok := IsClientError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, 409, ce.code)
			store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}
