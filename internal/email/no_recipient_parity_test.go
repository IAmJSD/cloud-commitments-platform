package email

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// tokenBearingSend is one of the senders' methods whose body carries a live,
// recipient-scoped link (approve, revoke, exchange approval).
type tokenBearingSend struct {
	name string
	call func(ctx context.Context, s SenderInterface) error
}

func tokenBearingSends() []tokenBearingSend {
	recs := []RecommendationSummary{{Service: "ec2", ResourceType: "m5.large", Region: "us-east-1", Count: 1}}
	return []tokenBearingSend{
		{name: "SendRIExchangePendingApproval", call: func(ctx context.Context, s SenderInterface) error {
			return s.SendRIExchangePendingApproval(ctx, RIExchangeNotificationData{
				DashboardURL: "https://dashboard.example.com",
				Exchanges:    []RIExchangeItem{{RecordID: "r1", ApprovalToken: "tok-1", SourceRIID: "ri-1"}},
			})
		}},
		{name: "SendPurchaseApprovalRequest", call: func(ctx context.Context, s SenderInterface) error {
			return s.SendPurchaseApprovalRequest(ctx, NotificationData{
				DashboardURL: "https://dashboard.example.com", ApprovalToken: "tok", ExecutionID: "exec-1", Recommendations: recs,
			})
		}},
		{name: "SendPurchaseScheduledNotification", call: func(ctx context.Context, s SenderInterface) error {
			return s.SendPurchaseScheduledNotification(ctx, NotificationData{
				DashboardURL: "https://dashboard.example.com", ExecutionID: "exec-1",
				RevokeURL: "https://dashboard.example.com/purchases#history?execution=exec-1", Recommendations: recs,
			})
		}},
		{name: "SendPurchaseExecutedNotification", call: func(ctx context.Context, s SenderInterface) error {
			return s.SendPurchaseExecutedNotification(ctx, NotificationData{
				DashboardURL: "https://dashboard.example.com", ExecutionID: "exec-1", RevocationToken: "tok", Recommendations: recs,
			})
		}},
	}
}

// TestSenders_NoRecipientParity asserts that SES and SMTP agree on the
// no-recipient contract for every token-bearing email (issue #111, A06-014):
// both return ErrNoRecipient and neither delivers anything. Before the fix the
// SMTP sender fell back to its static notify inbox and mailed the live link
// there.
func TestSenders_NoRecipientParity(t *testing.T) {
	for _, tc := range tokenBearingSends() {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			ses := new(MockSESClient)
			ses.On("GetAccount", mock.Anything, mock.Anything).
				Return(&sesv2.GetAccountOutput{ProductionAccessEnabled: true}, nil).Maybe()
			sesSender := &Sender{sesClient: ses, fromEmail: "noreply@example.com"}
			sesErr := tc.call(ctx, sesSender)
			require.ErrorIs(t, sesErr, ErrNoRecipient, "SES sender")
			ses.AssertNotCalled(t, "SendEmail", mock.Anything, mock.Anything)

			// A live SMTP server and a configured notify inbox: any fallback
			// would deliver successfully and show up in receivedMsg.
			server := newMockSMTPServer(t, false)
			server.start(t)
			smtpSender := &SMTPSender{
				host:        "127.0.0.1",
				port:        server.port,
				fromEmail:   "notifications@example.com",
				notifyEmail: "static-notify@example.com",
			}
			smtpErr := tc.call(ctx, smtpSender)
			server.stop()
			require.ErrorIs(t, smtpErr, ErrNoRecipient, "SMTP sender")
			server.mu.Lock()
			received := server.receivedMsg
			server.mu.Unlock()
			assert.Empty(t, received, "SMTP sender must not open a session to the notify inbox")
		})
	}
}
