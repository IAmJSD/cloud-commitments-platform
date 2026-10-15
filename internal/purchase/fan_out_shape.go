package purchase

import (
	"context"
	"fmt"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// lineageFansOut decides whether a plan execution with no cloud_account_id fans
// out across the plan's accounts or runs the single-account path (issue #97).
//
// The two paths derive their provider idempotency tokens from different keys:
// the lineage key itself on the single-account path, "<lineage key>:<account
// id>" on the fan-out path. Choosing the path from the plan's CURRENT accounts
// let a plan that gained its first account, or lost its last one, between an
// attempt and its Retry derive tokens the provider had never seen, so a
// commitment the first attempt bought was bought again. Instead, the first
// attempt of a lineage pins the shape it runs with, and every later attempt
// follows the pin:
//
//   - pinned shape matches the live one: unchanged behavior, so tokens of a
//     lineage whose plan never crossed zero-to-N accounts are byte-identical.
//   - pinned single, plan now has accounts: run the single-account path again,
//     re-deriving the first attempt's tokens so the provider dedupes.
//   - pinned multi, plan now has no accounts: refuse. The first attempt's tokens
//     are per account and there is no account left to fan out to; the
//     single-account path would derive tokens the provider has never seen.
//
// A key with a colon is a per-account lineage that reached the executor without
// its scope; reattachAccountScope (issue #1537) owns that case, so it is left to
// the live decision. An empty key cannot be pinned meaningfully and is left to
// the live decision too.
func (m *Manager) lineageFansOut(ctx context.Context, exec *config.PurchaseExecution, accounts []config.CloudAccount) (bool, error) {
	live := len(accounts) > 0
	key := idempotencyLineageKey(exec)
	if key == "" || strings.Contains(key, ":") {
		return live, nil
	}
	want := config.FanOutShapeSingle
	if live {
		want = config.FanOutShapeMulti
	}
	pinned, err := m.config.PinExecutionFanOutShape(ctx, key, want)
	if err != nil {
		return false, fmt.Errorf("execution %s: %w", exec.ExecutionID, err)
	}
	switch pinned {
	case want:
		return live, nil
	case config.FanOutShapeSingle:
		logging.Warnf("purchase[%s]: plan %s now has %d account(s), but this execution's lineage first ran on the "+
			"single-account path; re-driving that path so the provider idempotency tokens match (issue #97)",
			exec.ExecutionID, exec.PlanID, len(accounts))
		return false, nil
	case config.FanOutShapeMulti:
		return false, fmt.Errorf("refusing execution %s: its lineage first fanned out across plan %s's accounts, "+
			"which now has none, so the per-account idempotency tokens cannot be re-derived and the single-account "+
			"path would buy again under tokens the provider has never seen (issue #97). "+
			"Re-attach the accounts to the plan to let the retry complete, or cancel this execution",
			exec.ExecutionID, exec.PlanID)
	default:
		return false, fmt.Errorf("execution %s: unknown pinned fan-out shape %q", exec.ExecutionID, pinned)
	}
}
