package purchase

import (
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
)

// A bare plan step fails from pending/notified, before the executor ever runs,
// so the failure carries the no_submission proof (platform#118 PR-C1).
func TestStampFailedBarePlanStep_RecordsNoSubmissionProof(t *testing.T) {
	exec := &config.PurchaseExecution{ExecutionID: "e", PlanID: "p", Status: "pending"}
	stampFailedBarePlanStep(exec)
	assert.Equal(t, "failed", exec.Status)
	assert.True(t, exec.NoSubmission)
}
