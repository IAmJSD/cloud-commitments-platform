package email

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	reconcileHeading = "NEEDS RECONCILIATION"
	completedHeading = "The following RI exchanges have been automatically completed:"
)

// outcomeUnknownExchangeData is an auto run with one completed exchange, one
// whose accept outcome is unknown, and one definite failure.
func outcomeUnknownExchangeData() RIExchangeNotificationData {
	return RIExchangeNotificationData{
		DashboardURL: "https://dashboard.example.com",
		Mode:         "auto",
		Exchanges: []RIExchangeItem{
			{RecordID: "rec-done", SourceRIID: "ri-done", SourceInstanceType: "m5.large",
				TargetInstanceType: "m6i.large", TargetCount: 1, PaymentDue: "12.00", ExchangeID: "exch-done"},
			{RecordID: "rec-unknown", SourceRIID: "ri-unknown", SourceInstanceType: "c5.large",
				TargetInstanceType: "c6i.large", TargetCount: 2, PaymentDue: "34.00",
				Error: "purchase outcome unknown: reconcile before retrying", OutcomeUnknown: true},
			{RecordID: "rec-failed", SourceRIID: "ri-failed", SourceInstanceType: "r5.large",
				TargetInstanceType: "r6i.large", TargetCount: 1, PaymentDue: "5.00",
				Error: "InvalidParameterValue: offering not available"},
		},
	}
}

// splitCompletedEmail returns the reconciliation group and the completed group
// of a rendered completed email. The reconciliation group comes first.
func splitCompletedEmail(t *testing.T, body string) (reconcile, completed string) {
	t.Helper()
	reconcileAt := strings.Index(body, reconcileHeading)
	completedAt := strings.Index(body, completedHeading)
	require.GreaterOrEqual(t, reconcileAt, 0, "no reconciliation group:\n%s", body)
	require.GreaterOrEqual(t, completedAt, 0, "no completed group:\n%s", body)
	require.Less(t, reconcileAt, completedAt, "reconciliation group must come first:\n%s", body)
	return body[reconcileAt:completedAt], body[completedAt:]
}

func TestRenderRIExchangeCompletedEmail_OutcomeUnknownNeedsReconciliation(t *testing.T) {
	body, err := RenderRIExchangeCompletedEmail(outcomeUnknownExchangeData())
	require.NoError(t, err)

	reconcile, completed := splitCompletedEmail(t, body)
	for _, want := range []string{
		"ri-unknown (c5.large)",
		"c6i.large x2",
		"$34.00",
		"Record: rec-unknown",
		"POST /api/ri-exchange/resolve/rec-unknown",
		"purchase outcome unknown",
		"do not retry",
	} {
		assert.Contains(t, reconcile, want)
	}
	assert.NotContains(t, reconcile, "ri-done", "a completed exchange is not unknown")
	assert.NotContains(t, reconcile, "ri-failed", "a definite failure is not unknown")
	assert.NotContains(t, completed, "ri-unknown", "an unknown outcome is not a completion")
	assert.Contains(t, completed, "ri-done")
	assert.NotContains(t, body, "ri-failed", "definite failures stay out of this email, as before")
}

// TestRenderRIExchangeCompletedEmail_OutcomeUnknownWithoutError pins that the
// flag alone decides the group: an unknown outcome with an empty Error must
// not slip into the completed list.
func TestRenderRIExchangeCompletedEmail_OutcomeUnknownWithoutError(t *testing.T) {
	data := outcomeUnknownExchangeData()
	data.Exchanges[1].Error = ""

	body, err := RenderRIExchangeCompletedEmail(data)
	require.NoError(t, err)

	reconcile, completed := splitCompletedEmail(t, body)
	assert.Contains(t, reconcile, "ri-unknown")
	assert.NotContains(t, completed, "ri-unknown")
}

func TestRenderRIExchangeCompletedEmail_NoUnknownOutcomeHasNoReconciliationGroup(t *testing.T) {
	data := outcomeUnknownExchangeData()
	data.Exchanges[1].OutcomeUnknown = false

	body, err := RenderRIExchangeCompletedEmail(data)
	require.NoError(t, err)

	assert.NotContains(t, body, reconcileHeading)
	assert.NotContains(t, body, "/api/ri-exchange/resolve/")
	assert.Contains(t, body, completedHeading)
}

func TestRIExchangeCompletedSubject(t *testing.T) {
	data := outcomeUnknownExchangeData()
	assert.Equal(t, "CUDly - RI Exchanges Need Reconciliation (1 of 3 exchanges)", riExchangeCompletedSubject(data))

	data.Exchanges[1].OutcomeUnknown = false
	assert.Equal(t, "CUDly - RI Exchanges Completed (3 exchanges)", riExchangeCompletedSubject(data))
}

// recordingSNSPublisher captures what would be published, never touching AWS.
type recordingSNSPublisher struct {
	subject string
	message string
	calls   int
}

func (r *recordingSNSPublisher) Publish(_ context.Context, params *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
	r.calls++
	r.subject = aws.ToString(params.Subject)
	r.message = aws.ToString(params.Message)
	return &sns.PublishOutput{}, nil
}

func TestSender_SendRIExchangeCompleted_OutcomeUnknown(t *testing.T) {
	pub := &recordingSNSPublisher{}
	sender := NewSenderWithClients(pub, &mockSESEmailSender{}, SenderConfig{
		FromEmail: "from@example.com",
		TopicARN:  "arn:aws:sns:us-east-1:123:topic",
	})

	require.NoError(t, sender.SendRIExchangeCompleted(context.Background(), outcomeUnknownExchangeData()))

	require.Equal(t, 1, pub.calls)
	assert.Equal(t, "CUDly - RI Exchanges Need Reconciliation (1 of 3 exchanges)", pub.subject)
	reconcile, _ := splitCompletedEmail(t, pub.message)
	assert.Contains(t, reconcile, "ri-unknown")
}

func TestSMTPSender_SendRIExchangeCompleted_OutcomeUnknown(t *testing.T) {
	server := newMockSMTPServer(t, false)
	server.start(t)

	sender := &SMTPSender{
		host:        "127.0.0.1",
		port:        server.port,
		fromEmail:   "notifications@cudly.io",
		fromName:    "CUDly",
		notifyEmail: "ops@example.com",
	}

	require.NoError(t, sender.SendRIExchangeCompleted(context.Background(), outcomeUnknownExchangeData()))
	server.stop() // flush the connection goroutine before reading receivedMsg

	got := server.receivedMsg
	assert.Contains(t, got, "Subject: CUDly - RI Exchanges Need Reconciliation (1 of 3 exchanges)")
	assert.Contains(t, got, reconcileHeading)
	assert.Contains(t, got, "POST /api/ri-exchange/resolve/rec-unknown")
}
