package purchase

import (
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
)

// Every server-authored string stored into purchase_executions.error or a
// recommendation's error must come back verbatim.
func TestSanitizeStoredError_AllowlistedNotesPassThroughVerbatim(t *testing.T) {
	fresh := &config.PurchaseExecution{}
	recordHistoryAuditGap(fresh, "c-1", fmt.Errorf("db down"))
	retry := &config.PurchaseExecution{RetryAttemptN: 2, Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "ec2"}}}
	azSP := &config.PurchaseExecution{RetryAttemptN: 1, Recommendations: []config.RecommendationRecord{{Provider: "azure", Service: "savingsplans"}}}
	gcpRetry := &config.PurchaseExecution{RetryAttemptN: 1, Recommendations: []config.RecommendationRecord{{Provider: "weird"}}}
	notes := []string{
		fresh.Error,
		recoveredAfterReapNote,
		"ramp not advanced",
		"reaped after 30m in running state — executor did not complete",
		"reaped after 30m in approved state — executor did not complete; safe to retry",
		strandedRecoveryError(&config.PurchaseExecution{}),
		strandedRecoveryError(retry),
		armedRedriveRefusal(retry).Error(),
		armedRedriveRefusal(azSP).Error(),
		armedRedriveRefusal(gcpRetry).Error(),
		ErrStaleAzurePricing.Error(),
		ErrPlanStepNoRecommendations.Error(),
		fmt.Errorf("execution %s: %w", "00000000-0000-0000-0000-000000000000", ErrPlanStepNoRecommendations).Error(),
		fmt.Errorf("%w: plan has no max_step_commitment set (auto-purchase of steps with recommendations is refused until one is set)", ErrStepCommitmentCap).Error(),
		fmt.Errorf("%w: step total commitment 10.00 exceeds max_step_commitment 5.00", ErrStepCommitmentCap).Error(),
		dueRowAgeRefusal(&config.PurchaseExecution{ScheduledDate: time.Now().Add(-100 * time.Hour)}, time.Hour, time.Now()).Error(),
		ErrClaimedRowRefused.Error(),
		"refusing to execute ladder purchase: its ladder run was canceled",
		"refusing to execute ladder purchase: its tranche is paused",
		"refusing to execute: ladder tranche lookup failed",
		"refusing to execute ladder purchase: global config unreadable",
		"scheduled plan step reached its scheduled date without recommendations; no purchase was submitted",
		"no current recommendation matched plan 00000000-0000-0000-0000-000000000000 step 2 by 2026-01-02 (3-day grace after the plan date)",
		"no current recommendation matched plan p step 2 by 2026-01-02 (3-day grace after the plan date); " + skippedNote([]string{"a\x1fb", "c"}),
		"target already met: target 10, committed 12; 0 failed row(s) of unknown outcome counted as committed",
		"target already met: target 10, committed 12; " + skippedNote([]string{"a"}) + "; 1 failed row(s) of unknown outcome counted as committed",
		fmt.Errorf("%w: 2 selected recommendation(s) target 2 cloud accounts (a, b); submit one purchase per account", errAmbiguousAccountScope).Error(),
		fmt.Errorf("%w: 1 selected recommendation(s) carry no cloud_account_id while 2 target account a", errAmbiguousAccountScope).Error(),
		"purchase was not successful",
		"EC2 recommendation for m5.large has zonal scope but no AvailabilityZone: refusing to buy a zonal RI in an arbitrary AZ; re-fetch the recommendation",
		"zonal RI offering needs an availability zone; re-fetch the recommendation",
		"email notifier not configured for this deployment",
		"no notification email set in Settings → General and no account contact emails configured",
		"no notification email set in Settings → General",
		"FROM_EMAIL not configured for this deployment",
	}
	for _, n := range notes {
		assert.NotEmpty(t, n)
		assert.Equal(t, n, SanitizeStoredError(n), n)
		assert.Equal(t, n, SanitizeStoredError(partialRunPrefix + "x: " + n)[len(partialRunPrefix)+3:], "behind a resource head: "+n)
	}
	joined := notes[0] + "; " + notes[1] + "; " + notes[6]
	assert.Equal(t, joined, SanitizeStoredError(joined))
	assert.Equal(t, partialRunPrefix+joined, SanitizeStoredError(partialRunPrefix+joined))
}

func TestSanitizeStoredError_UnknownTextFailsClosed(t *testing.T) {
	for _, in := range []string{
		"some unlisted text",
		"history_write_failed: commitment c purchased; and then provider text",
		"ramp not advanced: x; some tail",
		"purchase failed: boom",
		"send failed: SES said no",
		"failed to get plan: db",
	} {
		out := SanitizeStoredError(in)
		assert.Equal(t, ErrCategoryMessage(ErrCategoryUnknown), out, in)
	}
}
