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
			store.On("TransitionExecutionStatus", ctx, notifiedExecID, []string{"paused"}, "pending", mock.Anything).Return(row, nil)
			store.On("GetPurchasePlan", ctx, "plan-1").Return(&config.PurchasePlan{ID: "plan-1", NotificationDaysBefore: 3}, nil).Maybe()
			var saved *config.PurchaseExecution
			store.On("SavePurchaseExecution", ctx, mock.Anything).Run(func(a mock.Arguments) {
				c := *a.Get(1).(*config.PurchaseExecution)
				saved = &c
			}).Return(nil).Maybe()

			before := time.Now()
			_, err := handler.resumePlannedPurchase(ctx, req, notifiedExecID)
			require.NoError(t, err)
			if !tc.wantMoved {
				assert.Nil(t, saved)
				assert.Equal(t, tc.date, row.ScheduledDate)
				return
			}
			require.NotNil(t, saved)
			assert.Equal(t, "pending", saved.Status)
			assert.WithinDuration(t, before.AddDate(0, 0, 3), saved.ScheduledDate, time.Minute)
		})
	}
}
