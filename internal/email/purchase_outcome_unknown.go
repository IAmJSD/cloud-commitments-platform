package email

// purchaseOutcomeUnknownSubject replaces the purchase-failed subject when
// NotificationData.OutcomeUnknown is set (issue #1415).
const purchaseOutcomeUnknownSubject = "CUDly - Purchase Outcome Unknown - Verify in the Cloud Console"

// purchaseOutcomeUnknownTemplate is the purchase-failed email for a row that a
// recovery sweep (the stuck-row reaper or the stranded-approval sweep) failed
// because its purchase run was interrupted. The provider may already have
// created the commitment, so unlike purchaseFailedTemplate it never asks the
// operator to retry: buying again without checking could create a duplicate
// (issue #1415).
const purchaseOutcomeUnknownTemplate = `CUDly - Purchase Outcome Unknown
================================

The purchase run for execution {{.ExecutionID}}{{if .PlanID}} (plan {{.PlanID}}){{end}} was interrupted
before it recorded a result, and a recovery sweep marked it failed. CUDly
cannot tell whether the commitments below were bought: the cloud provider
may already have created them.

Verify each one in the cloud provider's console before buying it again.
Buying it again without checking can create a duplicate commitment.

Outcome unknown:
{{range .Recommendations}}
- {{.Count}}x {{.ResourceType}}{{if .Engine}} ({{.Engine}}){{end}} in {{.Region}}
  Service: {{.Service}}
{{end}}

Execution details:
{{.DashboardURL}}/history

This is an automated message from CUDly.
`

// purchaseFailedSubject returns failedSubject, the sender's usual
// purchase-failed subject, or the outcome-unknown subject when data is for a
// row whose purchase may already exist.
func purchaseFailedSubject(data NotificationData, failedSubject string) string {
	if data.OutcomeUnknown {
		return purchaseOutcomeUnknownSubject
	}
	return failedSubject
}
