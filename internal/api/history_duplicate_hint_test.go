package api

import (
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestHistoryRow_DuplicateGuardHint(t *testing.T) {
	ec2 := []config.RecommendationRecord{{Provider: "aws", Service: "ec2"}}
	denied := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: fakeAWSDenied, Recommendations: ec2}, "", "")
	assert.False(t, strings.Contains(denied.OpsHint, "duplicate guard"), "definite refusal hides the warning")
	unknown := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: "purchase outcome unknown", Recommendations: ec2}, "", "")
	assert.Contains(t, unknown.OpsHint, "duplicate guard")
	assert.True(t, unknown.RedriveUnsafe)

	// One refused rec does not prove that another, whose outcome is unknown, bought nothing.
	mixed := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: "t4g.nano: purchase outcome unknown; t4g.micro: " + fakeAWSDenied, Recommendations: ec2}, "", "")
	assert.Contains(t, mixed.OpsHint, "duplicate guard", "a mixed failure keeps the warning")
	allDenied := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: "t4g.nano: " + fakeAWSDenied + "; t4g.micro: " + fakeAWSDenied, Recommendations: ec2}, "", "")
	assert.NotContains(t, allDenied.OpsHint, "duplicate guard", "every rec refused hides the warning")
}

func TestHistoryRow_DuplicateHintKeptOutsideDefiniteAWSRefusals(t *testing.T) {
	const gcpRate = "commitment creation failed: googleapi: Error 403: Rate Limit Exceeded, rateLimitExceeded"
	const gcpNotFound = "commitment creation failed: googleapi: Error 404: operations/op-1 was not found, notFound"
	azure500 := azureHTTPError(t, 500, "/providers/Microsoft.Capacity/reservationOrders/x").Error()
	azure429 := "PUT https://management.azure.com/x\nRESPONSE 429: 429 Too Many\nERROR CODE: TooManyRequests"
	aws503 := strings.Replace(fakeAWSDenied, "StatusCode: 403", "StatusCode: 503", 1)
	ec2 := config.RecommendationRecord{Provider: "aws", Service: "ec2"}
	azSP := config.RecommendationRecord{Provider: "azure", Service: "savingsplans"}
	gcp := config.RecommendationRecord{Provider: "gcp", Service: "compute"}
	cases := []struct {
		name string
		recs []config.RecommendationRecord
		err  string
	}{
		{"gcp rate limit", []config.RecommendationRecord{gcp, ec2}, gcpRate},
		{"gcp not found", []config.RecommendationRecord{gcp, ec2}, gcpNotFound},
		{"azure 500 on a capacity url", []config.RecommendationRecord{azSP}, azure500},
		{"azure 429", []config.RecommendationRecord{azSP}, azure429},
		{"aws 5xx", []config.RecommendationRecord{ec2}, aws503},
		{"aws without a code", []config.RecommendationRecord{ec2}, "operation error EC2: X, https response error StatusCode: 403"},
		{"non-aws rec beside a definite aws refusal", []config.RecommendationRecord{ec2, gcp}, fakeAWSDenied},
	}
	for _, c := range cases {
		row := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: c.err, Recommendations: c.recs}, "", "")
		assert.True(t, row.RedriveUnsafe, c.name)
		assert.Contains(t, row.OpsHint, "duplicate guard", c.name)
	}
}
