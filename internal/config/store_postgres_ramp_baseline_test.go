package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baselineRec(account, resourceType string, count int) RecommendationRecord {
	acct := account
	return RecommendationRecord{
		Provider: "aws", Service: "ec2", Region: "us-east-1", ResourceType: resourceType,
		Term: 1, Payment: "no-upfront", Count: count, Selected: true, CloudAccountID: &acct,
	}
}

func baselineKey(t *testing.T, rec RecommendationRecord) string {
	t.Helper()
	k, err := RecIdentityKey(&rec)
	require.NoError(t, err)
	return k
}

func baselineExec(id string, step int, status string, recs ...RecommendationRecord) PurchaseExecution {
	return PurchaseExecution{ExecutionID: id, StepNumber: step, Status: status, Recommendations: recs}
}

// The status set decides how much of a plan's ramp is already held: a status
// wrongly counted under-buys, a status wrongly dropped over-buys.
func TestSumPlanCommitted_StatusSet(t *testing.T) {
	rec := baselineRec("a1", "m5.large", 20)
	key := baselineKey(t, rec)

	counted := []string{"pending", "notified", "approved", "scheduled", "running", "paused",
		StatusCompleted, StatusPartiallyCompleted, "a-status-added-later"}
	for _, status := range counted {
		got, err := sumPlanCommitted([]PurchaseExecution{baselineExec("e", 1, status, rec)})
		require.NoError(t, err)
		assert.Equal(t, 20, got[key].Count, "status %q holds capacity", status)
	}

	for _, status := range []string{StatusCanceled, LegacyStatusCanceled, StatusExpired} {
		got, err := sumPlanCommitted([]PurchaseExecution{baselineExec("e", 1, status, rec)})
		require.NoError(t, err)
		assert.Empty(t, got, "status %q holds nothing", status)
	}

	unknownOutcome := baselineExec("e", 1, StatusFailed, rec)
	got, err := sumPlanCommitted([]PurchaseExecution{unknownOutcome})
	require.NoError(t, err)
	assert.Equal(t, 20, got[key].Count, "a failed row with an unknown outcome is assumed bought")

	neverSubmitted := baselineExec("e", 1, StatusFailed, rec)
	neverSubmitted.NoSubmission = true
	got, err = sumPlanCommitted([]PurchaseExecution{neverSubmitted})
	require.NoError(t, err)
	assert.Empty(t, got, "a failed row proven unsubmitted holds nothing")
}

// A 4-step weekly plan with 20 units a step: steps add up, while the root row
// of a fan-out, its per-account rows and a retry of the same step are one
// purchase and count once.
func TestSumPlanCommitted_StepsAddAndSameStepRowsCountOnce(t *testing.T) {
	rec := baselineRec("a1", "m5.large", 20)
	key := baselineKey(t, rec)
	other := baselineRec("a2", "m5.large", 7)
	otherKey := baselineKey(t, other)

	execs := []PurchaseExecution{
		baselineExec("root1", 1, StatusCompleted, rec, other),
		baselineExec("acct1-s1", 1, StatusCompleted, rec),
		baselineExec("acct2-s1", 1, StatusCompleted, other),
		baselineExec("s2-failed", 2, StatusFailed, rec),
		baselineExec("s2-retry", 2, StatusCompleted, rec),
		baselineExec("s3", 3, "approved", rec),
	}
	got, err := sumPlanCommitted(execs)
	require.NoError(t, err)
	assert.Equal(t, 60, got[key].Count, "20 per step over 3 steps")
	assert.Equal(t, 7, got[otherKey].Count, "the second account is counted once")
}

// A recommendation that errored inside a partially_completed row may still have
// been accepted by the provider (a timeout), so every selected rec counts; an
// unselected rec was never sent.
func TestSumPlanCommitted_PartialRowCountsEveryRecSelectedOnly(t *testing.T) {
	bought := baselineRec("a1", "m5.large", 10)
	bought.Purchased = true
	errored := baselineRec("a1", "c5.large", 4)
	errored.Error = "context deadline exceeded"
	skipped := baselineRec("a1", "r5.large", 9)
	skipped.Selected = false

	got, err := sumPlanCommitted([]PurchaseExecution{baselineExec("p", 1, StatusPartiallyCompleted, bought, errored, skipped)})
	require.NoError(t, err)
	assert.Equal(t, 10, got[baselineKey(t, bought)].Count)
	assert.Equal(t, 4, got[baselineKey(t, errored)].Count)
	assert.NotContains(t, got, baselineKey(t, skipped))
}

func TestSumPlanCommitted_HourlyCommitment(t *testing.T) {
	sp := baselineRec("a1", "compute", 1)
	sp.Service = "savingsplans"
	sp.Details = json.RawMessage(`{"hourly_commitment":1.25,"platform":"x"}`)
	got, err := sumPlanCommitted([]PurchaseExecution{
		baselineExec("s1", 1, StatusCompleted, sp), baselineExec("s2", 2, StatusCompleted, sp)})
	require.NoError(t, err)
	assert.InDelta(t, 2.5, got[baselineKey(t, sp)].Hourly, 1e-9)

	sp.Details = json.RawMessage(`{"hourly_commitment":"abc"}`)
	_, err = sumPlanCommitted([]PurchaseExecution{baselineExec("bad", 1, StatusCompleted, sp)})
	require.Error(t, err, "an unreadable hourly commitment fails loud")
}
