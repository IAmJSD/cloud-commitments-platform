package purchase

import (
	"errors"
	"regexp"
	"strings"
)

// serverNotePatterns lists every server-authored string stored into
// purchase_executions.error or a recommendation's error (enumerated from the
// assignments in purchase/execution.go, manager.go, reaper.go, recover_reaped.go,
// notifications.go, plan_step_attach.go, stranded_redrive.go, step_cap.go,
// due_age_cap.go, claim.go, ladder_execution_recheck.go and the api retry
// handler's email reasons). Anything else in a stored error is treated as
// provider text. Each pattern is anchored and closed: no pattern matches free text or a newline.
// The em dash in the reaper note is written as \x{2014}.
var serverNotePatterns = []string{
	`history_write_failed: commitment [^;\s]+ purchased but its history record failed to save`,
	`recovered after reap; provider purchase succeeded`,
	`reaped after \d+m in [a-z_]+ state \x{2014} executor did not complete(?:; safe to retry)?`,
	`execution was approved but its purchase run was interrupted before completing and never finalized; failed by the recovery sweep so it is not silently stuck \(issue #632\)\. (?:Verify on the cloud provider that no commitment was created, then Retry\.|Retry is refused for this purchase because a repeat could buy a second commitment\. Verify on the cloud provider whether the commitment was created; if it was not, start a new purchase\.)`,
	`refusing to execute retry attempt \d+: (?:` + redriveReasonPattern + `)`,
	`stale Azure recommendation pricing; refresh recommendations and submit a new purchase(?:: the latest collection state is missing, so Azure prices cannot be confirmed current)?`,
	`(?:execution [^:\s]+: )?plan step has no recommendations attached, so it cannot buy anything(?: \(platform#631: plan steps cannot yet carry recommendations\))?`,
	`plan step refused by commitment cap(?:: plan has no max_step_commitment set \(auto-purchase of steps with recommendations is refused until one is set\)|: step total commitment \d+\.\d{2} exceeds max_step_commitment \d+\.\d{2})?`,
	`scheduled purchase skipped: it was due too long ago(?:: it was due at \d{4}-\d{2}-\d{2}T[0-9:.]+Z, [0-9a-z.]+ ago, which is more than the [0-9a-z.]+ limit \(SCHEDULED_PURCHASE_MAX_OVERDUE\); nothing was submitted, retry it if the purchase is still wanted)?`,
	`purchase refused after claim: the claimed execution no longer passes the authorization checks`,
	`refusing to execute ladder purchase: (?:global config unavailable|ladder planning is switched off|ladder execution is switched off|its tranche status is unknown|its tranche is [a-z_]+|its ladder config no longer exists|its ladder config is disabled|ladder config mode is "[a-z_]*", not auto_approve|its ladder run no longer exists|its ladder run was canceled)`,
	`scheduled plan step reached its scheduled date without recommendations; no purchase was submitted`,
	`no current recommendation matched plan [^;\s]+ step \d+ by \d{4}-\d{2}-\d{2} \(\d+-day grace after the plan date\)(?:; ` + skippedNotePattern + `)?`,
	`target already met: target \d+, committed \d+(?:; ` + skippedNotePattern + `)?; \d+ failed row\(s\) of unknown outcome counted as committed`,
	`selected recommendations do not resolve to a single cloud account: (?:\d+ selected recommendation\(s\) target \d+ cloud accounts \(` + idListPattern + `\); submit one purchase per account|\d+ selected recommendation\(s\) carry no cloud_account_id while \d+ target account ` + idPattern + `)`,
	`purchase was not successful`,
	`EC2 recommendation for \S+ has zonal scope but no AvailabilityZone: refusing to buy a zonal RI in an arbitrary AZ; re-fetch the recommendation`,
	`zonal RI offering needs an availability zone; re-fetch the recommendation`,
	`email notifier not configured for this deployment`,
	`no notification email set in Settings \x{2192} General(?: and no account contact emails configured)?`,
	`FROM_EMAIL not configured for this deployment`,
}

const (
	idPattern          = `[^;\s,()]+`
	idListPattern      = idPattern + `(?:, ` + idPattern + `)*`
	skippedNotePattern = `\d+ recommendation\(s\) without a frozen baseline skipped \(under-buy\): ` + idListPattern
)

// serverNoteHeads are server notes whose tail can carry a wrapped store or
// config error. Only the fixed head is ever shown; the tail is dropped.
var serverNoteHeads = []string{
	`ramp not advanced`,
	`refusing to execute: ladder tranche lookup failed`,
	`refusing to execute ladder purchase: global config unreadable`,
	`stale Azure recommendation pricing; refresh recommendations and submit a new purchase`,
	`plan step refused by commitment cap`,
	`purchase refused after claim: the claimed execution no longer passes the authorization checks`,
	`scheduled purchase skipped: it was due too long ago`,
}

var redriveReasonPattern = regexp.QuoteMeta(redriveReasonAWSNoGuard) + `|` + regexp.QuoteMeta(redriveReasonAzureSavingsPlan) +
	`|provider "[^"]*" is not known to reject a duplicate purchase, so re-driving this could buy a second commitment`

// A rec's "<resourceType>: " head, as the writer's per-rec join adds it.
var serverNoteRE = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9._-]*: )?(?:` + strings.Join(serverNotePatterns, "|") + `)(?:; |$)`)

var serverNoteHeadRE = regexp.MustCompile(`^((?:[A-Za-z0-9][A-Za-z0-9._-]*: )?(?:` + strings.Join(serverNoteHeads, "|") + `))(?:: [^;]*)?(?:; |$)`)

// renderServerNotes returns s with every "; "-separated item either kept
// verbatim (a closed allowlisted note) or cut to its fixed head (a known note
// whose tail is not closed). ok is false when any item is neither. Matching
// consumes whole notes, so a "; " inside a provider message cannot smuggle a
// tail through.
func renderServerNotes(s string) (string, bool) {
	var out strings.Builder
	for s != "" {
		if m := serverNoteRE.FindString(s); m != "" {
			out.WriteString(m)
			s = s[len(m):]
			continue
		}
		loc := serverNoteHeadRE.FindStringSubmatchIndex(s)
		if loc == nil {
			return "", false
		}
		out.WriteString(s[loc[2]:loc[3]])
		if strings.HasSuffix(s[:loc[1]], "; ") {
			out.WriteString("; ")
		}
		s = s[loc[1]:]
	}
	return out.String(), true
}

// ServerNoteMessage returns fixed server text for a known server-authored
// refusal so live responses can show it. A sentinel yields only its own text,
// never the wrapped chain; any other error must be allowlisted notes.
func ServerNoteMessage(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	for _, sentinel := range []error{ErrPlanStepNoRecommendations, ErrStaleAzurePricing, ErrStepCommitmentCap,
		ErrDueRowTooOld, ErrClaimedRowRefused, errAmbiguousAccountScope} {
		if errors.Is(err, sentinel) {
			return sentinel.Error(), true
		}
	}
	return renderServerNotes(err.Error())
}
