package api

import (
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
)

// ec2Head is the single-attempt EC2 library head the executor stores before the SDK error.
const ec2Head = "failed to purchase EC2 RI: "

func TestHistoryRow_DuplicateGuardHint(t *testing.T) {
	ec2 := []config.RecommendationRecord{{Provider: "aws", Service: "ec2"}}
	denied := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: ec2Head + fakeAWSDenied, Recommendations: ec2}, "", "")
	assert.False(t, strings.Contains(denied.OpsHint, "duplicate guard"), "definite refusal hides the warning")
	unknown := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: "purchase outcome unknown", Recommendations: ec2}, "", "")
	assert.Contains(t, unknown.OpsHint, "duplicate guard")
	assert.True(t, unknown.RedriveUnsafe)

	// One refused rec does not prove that another, whose outcome is unknown, bought nothing.
	mixed := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: "t4g.nano: purchase outcome unknown; t4g.micro: " + fakeAWSDenied, Recommendations: ec2}, "", "")
	assert.Contains(t, mixed.OpsHint, "duplicate guard", "a mixed failure keeps the warning")
	allDenied := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: "t4g.nano: " + ec2Head + fakeAWSDenied + "; t4g.micro: " + ec2Head + fakeAWSDenied, Recommendations: ec2}, "", "")
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

func TestHistoryRow_ExecutorStoredAWSRefusalDropsDuplicateHint(t *testing.T) {
	const ec2Stored = "m5.large: purchase failed: failed to purchase EC2 RI: operation error EC2: PurchaseReservedInstancesOffering, https response error StatusCode: 403, RequestID: 11111111-2222-3333-4444-555555555555, api error UnauthorizedOperation: You are not authorized (account 123456789012)"
	const redshiftStored = "ra3.xlplus: purchase failed: failed to purchase Redshift Reserved Node: operation error Redshift: PurchaseReservedNodeOffering, https response error StatusCode: 403, RequestID: 11111111-2222-3333-4444-555555555555, api error AccessDenied: denied"
	// RDS calls carry an idempotency token and keep the SDK retries, so its 4xx proves nothing.
	const rdsStored = "db.r6g.large: purchase failed: failed to purchase RDS RI: operation error RDS: PurchaseReservedDBInstancesOffering, https response error StatusCode: 403, RequestID: 11111111-2222-3333-4444-555555555555, api error AccessDenied: denied"
	ec2 := config.RecommendationRecord{Provider: "aws", Service: "ec2"}
	redshift := config.RecommendationRecord{Provider: "aws", Service: "redshift"}
	for name, stored := range map[string]string{"ec2": ec2Stored, "redshift beside ec2": ec2Stored + "; " + redshiftStored} {
		row := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: stored, Recommendations: []config.RecommendationRecord{ec2, redshift}}, "", "")
		assert.NotContains(t, row.OpsHint, "duplicate guard", name)
		assert.NotEmpty(t, row.OpsHint, name+": ops_hint stays set so Retry stays hidden")
		assert.True(t, row.RedriveUnsafe, name)
	}
	for name, c := range map[string]struct {
		recs   []config.RecommendationRecord
		stored string
	}{
		"unknown head":   {[]config.RecommendationRecord{ec2}, strings.Replace(ec2Stored, "failed to purchase EC2 RI", "failed to purchase GPU RI", 1)},
		"5xx":            {[]config.RecommendationRecord{ec2}, strings.Replace(ec2Stored, "StatusCode: 403", "StatusCode: 503", 1)},
		"empty provider": {[]config.RecommendationRecord{{Service: "ec2"}}, fakeAWSDenied},
		"mixed run":      {[]config.RecommendationRecord{ec2}, ec2Stored + "; t4g.nano: purchase failed: purchase outcome unknown"},
		"rds beside ec2": {[]config.RecommendationRecord{ec2, {Provider: "aws", Service: "rds"}}, ec2Stored + "; " + rdsStored},
		"retried ec2":    {[]config.RecommendationRecord{ec2}, strings.Replace(ec2Stored, ", https response error", ", exceeded maximum number of attempts, 3, https response error", 1)},
	} {
		row := executionToHistoryRow(config.PurchaseExecution{Status: "failed", Error: c.stored, Recommendations: c.recs}, "", "")
		assert.Contains(t, row.OpsHint, "duplicate guard", name)
	}
}
