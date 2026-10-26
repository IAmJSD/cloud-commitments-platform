package server

import (
	"context"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
)

// outcomeUnknownAutoResult is the shape cloud-commitments-go returns when the
// accept response for an auto exchange is lost: the outcome stays in Failed
// with OutcomeUnknown set (go#497/#499), and the run halts after it.
func outcomeUnknownAutoResult() *exchange.AutoExchangeResult {
	return &exchange.AutoExchangeResult{
		Mode: "auto",
		Completed: []exchange.ExchangeOutcome{{
			RecordID:           "rec-done",
			SourceRIID:         "ri-done",
			SourceInstanceType: "m5.large",
			TargetInstanceType: "m6i.large",
			TargetCount:        1,
			PaymentDue:         "12.00",
			ExchangeID:         "exch-done",
		}},
		Failed: []exchange.ExchangeOutcome{
			{
				RecordID:           "rec-unknown",
				SourceRIID:         "ri-unknown",
				SourceInstanceType: "c5.large",
				TargetInstanceType: "c6i.large",
				TargetCount:        2,
				PaymentDue:         "34.00",
				Error:              "purchase outcome unknown: the accept may have been processed and the exchange may exist; reconcile before retrying: connection reset",
				OutcomeUnknown:     true,
			},
			{
				RecordID:           "rec-failed",
				SourceRIID:         "ri-failed",
				SourceInstanceType: "r5.large",
				TargetInstanceType: "r6i.large",
				TargetCount:        1,
				PaymentDue:         "5.00",
				Error:              "InvalidParameterValue: offering not available",
			},
		},
	}
}

// TestSendExchangeNotification_OutcomeUnknownRendersReconciliation drives an
// auto run with an unknown-outcome exchange through sendExchangeNotification,
// captures the data handed to the (recording, never-sending) email sender, and
// renders the real completed-email template from it. The unknown-outcome
// exchange must show up in its own reconciliation group, apart from both the
// completed exchanges and the definite failures.
func TestSendExchangeNotification_OutcomeUnknownRendersReconciliation(t *testing.T) {
	var got *email.RIExchangeNotificationData
	app := &Application{
		Email: &mockEmailSender{
			sendCompletedFunc: func(_ context.Context, data email.RIExchangeNotificationData) error {
				got = &data
				return nil
			},
			sendApprovalFunc: func(_ context.Context, _ email.RIExchangeNotificationData) error {
				t.Error("an auto run must not send the approval email")
				return nil
			},
		},
	}

	app.sendExchangeNotification(context.Background(), outcomeUnknownAutoResult(), "")
	if got == nil {
		t.Fatal("expected the completed email to be sent")
	}

	body, err := email.RenderRIExchangeCompletedEmail(*got)
	if err != nil {
		t.Fatalf("render completed email: %v", err)
	}

	const reconcileHeading = "NEEDS RECONCILIATION"
	const completedHeading = "The following RI exchanges have been automatically completed:"
	reconcileAt := strings.Index(body, reconcileHeading)
	completedAt := strings.Index(body, completedHeading)
	if reconcileAt < 0 {
		t.Fatalf("email has no %q group for the unknown-outcome exchange:\n%s", reconcileHeading, body)
	}
	if completedAt < 0 {
		t.Fatalf("email lost its completed group:\n%s", body)
	}
	if reconcileAt > completedAt {
		t.Fatalf("reconciliation group must come before the completed group:\n%s", body)
	}

	reconcile := body[reconcileAt:completedAt]
	completed := body[completedAt:]
	for _, want := range []string{"ri-unknown", "rec-unknown", "c6i.large x2", "purchase outcome unknown", "/api/ri-exchange/resolve/"} {
		if !strings.Contains(reconcile, want) {
			t.Errorf("reconciliation group is missing %q:\n%s", want, reconcile)
		}
	}
	if strings.Contains(completed, "ri-unknown") {
		t.Errorf("unknown-outcome exchange is listed as completed:\n%s", completed)
	}
	if !strings.Contains(completed, "ri-done") {
		t.Errorf("completed exchange is missing from the completed group:\n%s", completed)
	}
	// A definite failure is not an unknown outcome: it must not be put in the
	// reconciliation group.
	if strings.Contains(reconcile, "ri-failed") {
		t.Errorf("definite failure is listed as needing reconciliation:\n%s", reconcile)
	}
}
