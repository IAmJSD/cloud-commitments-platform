package cicdpermissions_test

import (
	"strings"
	"testing"
)

// TestDeployPolicyGrantsEveryActionTheMonitoringModuleNeeds pins grants that
// policy_monitoring.tf and policy_compute.tf already hold for
// enable_monitoring and enable_migration_alarm, both default-false and so
// unexercised by any deploy. The ARNs are the real shapes from the Service
// Authorization Reference: an SNS topic ARN has NO type segment, a CloudWatch
// dashboard ARN has an empty region and a `/` before the name, and an alarm ARN
// uses `alarm:` (#157).
func TestDeployPolicyGrantsEveryActionTheMonitoringModuleNeeds(t *testing.T) {
	const (
		topic     = "arn:aws:sns:us-east-1:123456789012:cudly-prod-abc-alerts"
		dashboard = "arn:aws:cloudwatch::123456789012:dashboard/cudly-prod-abc-prod"
		alarm     = "arn:aws:cloudwatch:us-east-1:123456789012:alarm:cudly-prod-abc-lambda-errors"
		lambdaLG  = "arn:aws:logs:us-east-1:123456789012:log-group:/aws/lambda/cudly-prod-abc-api-20260101"
		ecsLG     = "arn:aws:logs:us-east-1:123456789012:log-group:/ecs/cudly-prod-abc"
	)
	var needs []grantNeed
	add := func(arn string, actions ...string) {
		for _, a := range actions {
			needs = append(needs, grantNeed{action: a, arn: arn})
		}
	}
	add(topic, "sns:CreateTopic", "sns:DeleteTopic", "sns:GetTopicAttributes", "sns:SetTopicAttributes",
		"sns:Subscribe", "sns:Unsubscribe", "sns:GetSubscriptionAttributes", "sns:SetSubscriptionAttributes",
		"sns:ListSubscriptionsByTopic", "sns:TagResource", "sns:UntagResource", "sns:ListTagsForResource")
	add(dashboard, "cloudwatch:PutDashboard", "cloudwatch:GetDashboard", "cloudwatch:DeleteDashboards")
	add(alarm, "cloudwatch:PutMetricAlarm", "cloudwatch:DeleteAlarms", "cloudwatch:TagResource")
	// The migration alarm's filter is on the Lambda log group; the monitoring
	// module's filters are on the Lambda and ECS groups.
	add(lambdaLG, "logs:PutMetricFilter", "logs:DeleteMetricFilter")
	add(ecsLG, "logs:PutMetricFilter", "logs:DeleteMetricFilter")
	// No resource-level permissions for these.
	for _, a := range []string{"logs:DescribeMetricFilters", "logs:PutQueryDefinition", "logs:DescribeQueryDefinitions", "logs:DeleteQueryDefinition"} {
		needs = append(needs, grantNeed{action: a, noResource: true})
	}
	assertGranted(t, "monitoring", needs)

	for _, stmt := range findStatements(t, "policy_monitoring.tf", func(string) bool { return true }) {
		for _, r := range statementResources(stmt) {
			if strings.Contains(r, ":topic/") || strings.Contains(r, ":sns:*:*:topic") {
				t.Errorf("statement %q: Resource %q has a topic type segment, which an SNS topic ARN does not have", statementSid(stmt), r)
			}
		}
	}
}
