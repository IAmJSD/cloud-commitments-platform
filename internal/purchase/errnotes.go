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

// bareNotePatterns are server-authored RI exchange record and collection notes.
// They are written bare, never behind a resource or provider head, so none is allowed.
var bareNotePatterns = []string{
	// Scheduler notes recorded when a collection ends before saving
	// (scheduler.go collectionStopped*); the cause is logged, never stored.
	`recommendations collection stopped before saving: (?:could not load settings|it was canceled or ran out of time|could not save the collected recommendations)`,
	// RI exchange record errors (api/handler_ri_exchange.go failExchange callers and
	// the exchange package's auto-exchange reservation notes).
	// The operator's stale-exchange resolve note (api/handler_ri_exchange_resolve.go
	// note()): fixed fields in a fixed order, then the admin's own reason as a Go
	// quoted string, which is text typed on purpose and closed by its quotes.
	`operator:(?:completed|failed): account="\d{12}" region="[a-z0-9-]{1,32}" checked_at=\d{4}-\d{2}-\d{2}T[0-9:.]+(?:Z|[+-]\d{2}:\d{2})(?: ack=true)?(?: exchange_id="[A-Za-z0-9-]{1,100}")? reason="(?:[^"\\\n]|\\.)*"`,
	`config load failed`,
	`exchange record has no region; cannot execute safely`,
	`(?:daily|per-exchange) spending cap is not configured \(RIExchange(?:MaxDaily|MaxPerExchange)USD is 0\)`,
	`RI exchange daily cap exceeded: (?:spend ` + amountPattern + ` exceeds cap ` + amountPattern + `|initial ` + amountPattern + `, remaining ` + amountPattern + `)`,
	`daily spending cap check failed`,
	`reserved ceiling "` + shortValuePattern + `" is not a decimal`,
	`exchange reservation returned no record ID`,
	`exchange reservation returned invalid ceiling "` + shortValuePattern + `"`,
	// Scheduler note for a provider name it does not know.
	`unknown provider "` + shortValuePattern + `" \(valid: aws, azure, gcp\)`,
}

// categoryNotePatterns matches a collection failure stored as "<provider>:
// <category message>" per failed provider.
func categoryNotePatterns() []string {
	out := make([]string, 0, 4)
	for _, c := range []string{ErrCategoryAccessDenied, ErrCategoryUnavailable, ErrCategoryLimit, ErrCategoryUnknown} {
		out = append(out, regexp.QuoteMeta(ErrCategoryMessage(c)))
	}
	return out
}

const (
	// amountPattern is a decimal amount; shortValuePattern is a short value with no
	// whitespace, quotes or newline. Anything wider falls back to the fixed head.
	amountPattern      = `\d+(?:\.\d+)?`
	shortValuePattern  = `[A-Za-z0-9._+-]{1,32}`
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

// exchangeNoteHeads are the fixed heads of the exchange notes whose tail is not closed.
var exchangeNoteHeads = []string{
	`RI exchange daily cap exceeded`,
	`reserved ceiling`,
	`exchange reservation returned invalid ceiling`,
	`unknown provider`,
}

var redriveReasonPattern = regexp.QuoteMeta(redriveReasonAWSNoGuard) + `|` + regexp.QuoteMeta(redriveReasonAzureSavingsPlan) +
	`|provider "[^"]*" is not known to reject a duplicate purchase, so re-driving this could buy a second commitment`

// resourceTypePattern is the "<resourceType>: " head the executor's per-rec join
// adds (execution.go: fmt.Sprintf("%s: %v", rec.ResourceType, err)). The head is
// closed: a Savings Plan type from a fixed list, or an instance, node or machine
// type shape whose family carries a digit, so an account number, IPv4 address,
// hostname, DB user or ARN never fits:
//   - Savings Plan types: Compute, EC2Instance, SageMaker (aws-sdk SavingsPlanType)
//     and Database, plus Azure's "Compute"
//   - AWS: [db.|cache.]<family>.<size>[.search] (m5.large, u-6tb1.metal,
//     mac2-m2pro.metal, ultrawarm1.medium.search, m7i.metal-24xl) and db.serverless
//   - Azure SQL: GP_Gen5_8 vCore SKUs and the tier names GENERAL_PURPOSE, BUSINESS_CRITICAL, HYPERSCALE
//   - Azure: Standard_|Basic_ sizes (Standard_D2, Standard_NV24ads_A10_v5) and the
//     unprefixed forms the provider sends (D2, D2s_v3, NC6s_v3)
//   - GCP: <family>-<series word>[-<digits or suffix>]... with a closed series word
//     (n2-standard-2, n2d-custom-2-8192-ext, a2-ultragpu-8g, c3d-highmem-360-lssd, e2-micro)
const resourceTypePattern = `(?:` +
	`Compute|EC2Instance|SageMaker|Database|db\.serverless` +
	`|(?:db\.|cache\.)?(?:[a-z]{1,4}[0-9][a-z0-9]{0,5}(?:-[a-z][a-z0-9]{1,7})?|u-[0-9]{1,2}tb[0-9]|ultrawarm1)\.(?:nano|micro|small|medium|large|[0-9]{0,3}xlarge|xlplus|metal(?:-[0-9]{1,2}xl)?)(?:\.search)?` +
	`|(?:Standard|Basic)_[A-Za-z]{1,4}[0-9][A-Za-z0-9_-]{0,24}` +
	`|[A-Z]{1,3}[0-9]{1,3}[a-z]{0,4}(?:_[A-Za-z0-9]{1,4}){0,2}|[A-Z]{1,3}_Gen[0-9]_[0-9]{1,3}|GENERAL_PURPOSE|BUSINESS_CRITICAL|HYPERSCALE` +
	`|[a-z][0-9][a-z]{0,2}-(?:standard|highmem|highcpu|highgpu|ultragpu|megagpu|megamem|ultramem|hypermem|custom|micro|small|medium)(?:-[a-z0-9]{1,8}){0,3}` +
	`)`

// providerHeadPattern is the collection writer's "<provider>: " head.
const providerHeadPattern = `(?:aws|azure|gcp)`

const (
	resourceHead = `(?:` + resourceTypePattern + `: )?`
	categoryHead = `(?:(?:` + providerHeadPattern + `|` + resourceTypePattern + `): )?`
)

func joinAlt(parts []string) string { return strings.Join(parts, "|") }

var serverNoteRE = regexp.MustCompile(`^(?:` +
	resourceHead + `(?:` + joinAlt(serverNotePatterns) + `)` +
	`|` + `(?:` + joinAlt(bareNotePatterns) + `)` +
	`|` + categoryHead + `(?:` + joinAlt(categoryNotePatterns()) + `)` +
	`)(?:; |$)`)

var serverNoteHeadRE = regexp.MustCompile(`^(` + resourceHead + `(?:` + joinAlt(serverNoteHeads) + `)|(?:` + joinAlt(exchangeNoteHeads) + `))(?:(?:: | ")[^;]*)?(?:; |$)`)

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
