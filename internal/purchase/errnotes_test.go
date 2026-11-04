package purchase

import (
	"fmt"
	"slices"
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
		legacyPlanStepNoRecommendationsNote,
		"execution 00000000-0000-0000-0000-000000000000: " + legacyPlanStepNoRecommendationsNote,
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
		"config load failed",
		"exchange record has no region; cannot execute safely",
		"daily spending cap is not configured (RIExchangeMaxDailyUSD is 0)",
		"per-exchange spending cap is not configured (RIExchangeMaxPerExchangeUSD is 0)",
		"RI exchange daily cap exceeded: spend 900.000000 exceeds cap 800.000000",
		"RI exchange daily cap exceeded: initial 10.000000, remaining 5.000000",
		"daily spending cap check failed",
		`reserved ceiling "x" is not a decimal`,
		"exchange reservation returned no record ID",
		`exchange reservation returned invalid ceiling "x"`,
		`unknown provider "AWS" (valid: aws, azure, gcp)`,
		ErrCategoryMessage(ErrCategoryAccessDenied),
	}
	exchangeNotes := notes[slices.Index(notes, "config load failed"):slices.Index(notes, ErrCategoryMessage(ErrCategoryAccessDenied))]
	for _, n := range notes {
		assert.NotEmpty(t, n)
		assert.Equal(t, n, SanitizeStoredError(n), n)
		if slices.Contains(exchangeNotes, n) {
			continue
		}
		head := "m5.large: "
		assert.Equal(t, n, SanitizeStoredError(partialRunPrefix + head + n)[len(partialRunPrefix)+len(head):], "behind a resource head: "+n)
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

// Rows stored before #1708 carry the old text that named an internal issue;
// it must keep passing the allowlist, and the live text must not name one.
const legacyPlanStepNoRecommendationsNote = "plan step has no recommendations attached, so it cannot buy anything (platform#631: plan steps cannot yet carry recommendations)"

func TestErrPlanStepNoRecommendations_NamesNoInternalIssue(t *testing.T) {
	assert.NotContains(t, ErrPlanStepNoRecommendations.Error(), "#")
}

// A recommendation collection failure is stored as one "<provider>: <category
// message>" item per failed provider.
func TestSanitizeStoredError_CollectionCategoryForm(t *testing.T) {
	stored := "aws: " + ErrCategoryMessage(ErrCategoryAccessDenied) + "; azure: " + ErrCategoryMessage(ErrCategoryUnknown)
	assert.Equal(t, stored, SanitizeStoredError(stored))
	assert.Equal(t, ErrCategoryMessage(ErrCategoryUnknown), SanitizeStoredError(stored+"; gcp: raw text for project my-project"))
}

// A variable part outside its closed alphabet (a newline, a provider or DB
// error text) never reaches the output: only the fixed head or nothing shows.
func TestSanitizeStoredError_ExchangeNoteTailsAreClosed(t *testing.T) {
	cases := []struct{ in, want string }{
		{"RI exchange daily cap exceeded: spend 1\nDETAIL pq: boom exceeds cap 2", "RI exchange daily cap exceeded"},
		{"RI exchange daily cap exceeded: initial 1.0, remaining pq: connection refused", "RI exchange daily cap exceeded"},
		{"RI exchange daily cap exceeded: spend x y exceeds cap 2.0", "RI exchange daily cap exceeded"},
		{"reserved ceiling \"a\nb\" is not a decimal", "reserved ceiling"},
		{"reserved ceiling \"pq: boom\" is not a decimal", "reserved ceiling"},
		{"exchange reservation returned invalid ceiling \"x\ny\"", "exchange reservation returned invalid ceiling"},
		{"unknown provider \"evil\nprovider\" (valid: aws, azure, gcp)", "unknown provider"},
		{"unknown provider \"db error: password for x\" (valid: aws, azure, gcp)", "unknown provider"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, SanitizeStoredError(c.in), c.in)
	}
	for _, in := range []string{
		"config load failed\nsecret",
		"config load failed: pq: boom",
		"exchange record has no region; cannot execute safely\nx",
		"daily spending cap check failed: pq: boom",
		"exchange reservation returned no record ID: pq: boom",
		"daily spending cap is not configured (RIExchangeMaxDailyUSD is 0)\nx",
		"aws: " + ErrCategoryMessage(ErrCategoryUnknown) + "\nraw",
	} {
		assert.Equal(t, ErrCategoryMessage(ErrCategoryUnknown), SanitizeStoredError(in), in)
	}
}

// The head ahead of a note must be a real resource type (per-rec notes) or
// aws|azure|gcp (collection notes); an account number, address, host or DB user
// in that slot is never shown.
func TestSanitizeStoredError_HeadsAreClosed(t *testing.T) {
	note := "plan step refused by commitment cap"
	category := ErrCategoryMessage(ErrCategoryUnavailable)
	for _, h := range []string{
		"m5.large", "db.t4g.micro", "cache.t3.micro", "dc2.large", "ra3.xlplus", "ra3.16xlarge",
		"t3.small.search", "Standard_D2_v3", "Basic_A1", "n2-standard-2", "e2-micro", "c3-highcpu-8",
		// Savings Plan types (aws-sdk SavingsPlanType, azure computeResourceType).
		"Compute", "EC2Instance", "SageMaker", "Database",
		"db.serverless", "u-6tb1.metal", "u-6tb1.112xlarge", "mac2-m2pro.metal", "ultrawarm1.medium.search",
		"m7i.metal-24xl", "n2-custom-4-8192", "n2d-custom-2-8192-ext", "e2-custom-4-8192", "e2-custom-medium-2048",
		"a2-ultragpu-8g", "a2-highgpu-1g", "a3-megagpu-8g", "c3d-highmem-360-lssd", "m1-ultramem-40", "h4d-highmem-192",
		"Standard_D2", "Standard_B2ats_v2", "Standard_NV24ads_A10_v5", "Standard_NC6s_v3", "Basic_C0",
		"D2", "D2s_v3", "NC6s_v3", "A1",
		"GP_Gen5_8", "BC_Gen5_16", "GENERAL_PURPOSE", "c7i-flex.large",
	} {
		assert.Equal(t, h+": "+note, SanitizeStoredError(h+": "+note), h)
		assert.Equal(t, h+": "+category, SanitizeStoredError(h+": "+category), h)
	}
	for _, h := range []string{"aws", "azure", "gcp"} {
		assert.Equal(t, h+": "+category, SanitizeStoredError(h+": "+category), h)
	}
	for _, h := range []string{
		"123456789012", "10.20.30.40", "db-prod.internal", "db.internal.example", "cudly_app", "web1.prod.example",
		"m5.large.example.com", "ec2.large.example", "arn:aws:iam::123456789012:role/x", "evil", "AWS",
	} {
		unknown := ErrCategoryMessage(ErrCategoryUnknown)
		assert.Equal(t, unknown, SanitizeStoredError(h+": "+category), h)
		assert.Equal(t, unknown, SanitizeStoredError(h+": "+note), h)
	}
	for _, in := range []string{
		"123456789012: config load failed",
		"10.20.30.40: config load failed",
		"db-prod.internal: daily spending cap check failed",
		"cudly_app: the requested commitment is not available from the provider",
		"m5.large: config load failed",
		"aws: " + note,
	} {
		out := SanitizeStoredError(in)
		assert.NotContains(t, out, ": ", in)
		assert.Equal(t, ErrCategoryMessage(ErrCategoryUnknown), out, in)
	}
}

func TestSanitizeStoredError_ResolveNoteExchangeIDIsClosed(t *testing.T) {
	base := `operator:completed: account="` + "1234" + "56789012" + `" region="us-east-1" checked_at=2026-01-02T03:04:05Z`
	ok := base + ` exchange_id="abc-123" reason="done"`
	assert.Equal(t, ok, SanitizeStoredError(ok))
	for _, id := range []string{`a b`, `db-prod.internal`, `x\"y`, `a_b`, ``} {
		in := base + ` exchange_id="` + id + `" reason="done"`
		assert.Equal(t, ErrCategoryMessage(ErrCategoryUnknown), SanitizeStoredError(in), id)
	}
}
