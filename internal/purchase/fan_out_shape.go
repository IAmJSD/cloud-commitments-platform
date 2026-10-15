package purchase

import (
	"context"
	"fmt"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// The single-account path and the per-account fan-out derive their provider
// idempotency tokens from different keys: the lineage key itself, and
// "<lineage key>:<account id>" per account. Choosing the path from the plan's
// CURRENT accounts let a plan that gained its first account, or lost its last
// one, between an attempt and its Retry derive tokens the provider had never
// seen, so a commitment the first attempt bought was bought again (issue #97).
//
// So the first attempt of a lineage that gets as far as submitting pins the
// shape it submitted under (pinLineageShape), and every later attempt follows
// the pin (lineageFansOut). An attempt refused before submission pins nothing:
// it derived no token that needs protecting, and pinning it would bind every
// retry to a path the operator may have just reconfigured the plan to avoid.

// fanOutPinKey returns the lineage key a root plan execution pins its shape
// under, and false for rows that take no part in pinning:
//
//   - direct purchases (no plan) and rows bound to an account: they always run
//     the single-account path, whatever the plan's accounts are;
//   - an empty key, which cannot identify a lineage;
//   - a key with a colon: a per-account lineage ("<root key>:<account id>")
//     whose row reached the executor without its scope. reattachAccountScope
//     (issue #1537) owns that case, and pinning it would write a per-account
//     key into a table of root lineages.
func fanOutPinKey(exec *config.PurchaseExecution) (string, bool) {
	if exec.PlanID == "" || exec.CloudAccountID != nil {
		return "", false
	}
	key := idempotencyLineageKey(exec)
	if key == "" || strings.Contains(key, ":") {
		return "", false
	}
	return key, true
}

// lineageFansOut decides whether a plan execution with no cloud_account_id fans
// out across the plan's accounts or runs the single-account path:
//
//   - nothing pinned yet, or the pin matches the live shape: the live shape, so
//     a lineage whose plan never crossed zero-to-N accounts derives exactly the
//     tokens it derived before issue #97.
//   - pinned single, plan now has accounts: the single-account path again, so
//     the first submission's tokens are re-derived and the provider dedupes.
//   - pinned multi, plan now has no accounts: refused. The submitted tokens are
//     per account and there is no account left to fan out to; the
//     single-account path would derive tokens the provider has never seen.
func (m *Manager) lineageFansOut(ctx context.Context, exec *config.PurchaseExecution, accounts []config.CloudAccount) (bool, error) {
	live := len(accounts) > 0
	key, ok := fanOutPinKey(exec)
	if !ok {
		return live, nil
	}
	pinned, err := m.config.GetExecutionFanOutShape(ctx, key)
	if err != nil {
		return false, beforeSubmission(fmt.Errorf("execution %s: %w", exec.ExecutionID, err))
	}
	switch {
	case pinned == "" || pinned == liveShape(live):
		return live, nil
	case pinned == config.FanOutShapeSingle:
		logging.Warnf("purchase[%s]: plan %s now has %d account(s), but this execution's lineage first submitted on the "+
			"single-account path; re-driving that path so the provider idempotency tokens match (issue #97)",
			exec.ExecutionID, exec.PlanID, len(accounts))
		return false, nil
	case pinned == config.FanOutShapeMulti:
		return false, beforeSubmission(fmt.Errorf("refusing execution %s: its lineage first submitted across plan %s's "+
			"accounts, which now has none, so the per-account idempotency tokens cannot be re-derived and the "+
			"single-account path would buy again under tokens the provider has never seen (issue #97). "+
			"Re-attach the accounts to the plan to let the retry complete, or cancel this execution",
			exec.ExecutionID, exec.PlanID))
	default:
		return false, beforeSubmission(fmt.Errorf("execution %s: unknown pinned fan-out shape %q", exec.ExecutionID, pinned))
	}
}

// pinLineageShape pins shape for exec's lineage. Callers invoke it after the
// attempt's last lineage-level pre-submission check and before any provider
// call. A pin already holding the other shape means a concurrent attempt of the
// same lineage submitted under it first, so this attempt must not submit.
func (m *Manager) pinLineageShape(ctx context.Context, exec *config.PurchaseExecution, shape string) error {
	key, ok := fanOutPinKey(exec)
	if !ok {
		return nil
	}
	pinned, err := m.config.PinExecutionFanOutShape(ctx, key, shape)
	if err != nil {
		return beforeSubmission(fmt.Errorf("execution %s: %w", exec.ExecutionID, err))
	}
	if pinned != shape {
		return beforeSubmission(fmt.Errorf("refusing execution %s: another attempt of its lineage submitted on the %s "+
			"fan-out shape first, and submitting on the %s shape would derive idempotency tokens the provider has "+
			"never seen (issue #97); retry it", exec.ExecutionID, pinned, shape))
	}
	return nil
}

func liveShape(fansOut bool) string {
	if fansOut {
		return config.FanOutShapeMulti
	}
	return config.FanOutShapeSingle
}
