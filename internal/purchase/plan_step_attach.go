package purchase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// errNoRecommendationSource marks a Manager that was never given a
// recommendation source; a step that needs recommendations fails loud.
var errNoRecommendationSource = errors.New("purchase manager has no recommendation source")

// errPlanStepSkipped is the quiet skip a tick returns when the plan changed
// under it or no match exists yet; it sends nothing and writes nothing.
var errPlanStepSkipped = fmt.Errorf("%w: step skipped", errExecutionNotNotifiable)

// stepResolver resolves the recommendations a plan's next step would buy.
type stepResolver interface {
	Resolve(ctx context.Context, plan *config.PurchasePlan, accountIDs []string, step int,
		baseline config.PlanRampBaseline, committed map[string]config.PlanRampQuantity) (planstep.Result, error)
}

type planstepResolver struct {
	src   planstep.Source
	store planstep.Store
}

func (r planstepResolver) Resolve(ctx context.Context, plan *config.PurchasePlan, accountIDs []string, step int,
	baseline config.PlanRampBaseline, committed map[string]config.PlanRampQuantity) (planstep.Result, error) {
	return planstep.Resolve(ctx, r.src, r.store, plan, accountIDs, step, baseline, committed)
}

// SetRecommendationSource wires the strict recommendation read the plan-step
// attach resolves through. Late-bound because the scheduler is built after the
// manager and imports this package.
func (m *Manager) SetRecommendationSource(src planstep.Source) {
	m.resolver = planstepResolver{src: src, store: m.config}
}

// claimPlanStep runs inside the tick's transaction. Lock order is the
// per-provider advisory lock, then the plan row lock; nothing else takes the
// advisory lock, so it cannot deadlock. Under both locks it decides from the
// LOCKED plan whether the step may attach, and either hands back the row to
// notify, writes the one row the step gets (attached or failed), or returns a
// quiet skip.
func (m *Manager) claimPlanStep(ctx context.Context, tx pgx.Tx, plan *config.PurchasePlan) (stepClaim, error) {
	providers := planProviders(plan)
	if len(providers) > 0 {
		if err := m.config.LockPlanProvidersTx(ctx, tx, providers); err != nil {
			return stepClaim{}, err
		}
	}
	locked, lockErr := m.config.LockPurchasePlanTx(ctx, tx, plan.ID)
	if lockErr != nil {
		return stepClaim{}, fmt.Errorf("failed to lock plan %s: %w", plan.ID, lockErr)
	}
	if locked == nil {
		return stepClaim{}, fmt.Errorf("plan not found: %s", plan.ID)
	}
	if !planUnchanged(plan, locked, providers) {
		return stepClaim{}, fmt.Errorf("%w: plan %s changed since it was listed", errPlanStepSkipped, plan.ID)
	}
	// step_number names the step this row will COMPLETE, not the count
	// already completed, matching api.createPurchaseExecutionsTx
	// (CurrentStep + 1). The ramp advance is keyed on it since #1669.
	claim := stepClaim{number: locked.RampSchedule.CurrentStep + 1}
	rows, listErr := m.config.ListExecutionsForPlanStepTx(ctx, tx, plan.ID, claim.number)
	if listErr != nil {
		return stepClaim{}, fmt.Errorf("failed to check for existing execution: %w", listErr)
	}
	return m.claimFromRows(ctx, tx, locked, claim, rows)
}

// claimFromRows decides, from the step's existing rows, whether to adopt one,
// skip, or attach recommendations (to the empty root row or to a new row).
func (m *Manager) claimFromRows(ctx context.Context, tx pgx.Tx, locked *config.PurchasePlan, claim stepClaim, rows []config.PurchaseExecution) (stepClaim, error) {
	if len(rows) > 0 {
		root, blocked := adoptableRootRow(rows)
		if blocked != nil {
			claim.blocked = blocked
			return claim, nil
		}
		if len(root.Recommendations) > 0 {
			if locked.NextExecutionDate.Before(time.Now()) {
				return stepClaim{}, fmt.Errorf("%w: step %d of plan %s already has a row and the plan date has passed", errExecutionNotNotifiable, claim.number, locked.ID)
			}
			claim.existing = root
			return claim, nil
		}
		if root.ScheduledDate.Before(vetoWindowEnd(locked, time.Now())) {
			// The row keeps its own date. One due, or due before the veto window
			// ends, would be bought before anyone could cancel; it is left to
			// the scheduler, which fails a due empty row (S10).
			return stepClaim{}, fmt.Errorf("%w: empty execution %s is due inside the veto window", errPlanStepSkipped, root.ExecutionID)
		}
		return m.attachStep(ctx, tx, locked, claim, root)
	}
	if !locked.AutoPurchase {
		return stepClaim{}, fmt.Errorf("%w: manual plan has no scheduled root", errPlanStepSkipped)
	}
	return m.attachStep(ctx, tx, locked, claim, nil)
}

// planProviders returns the sorted distinct providers of the plan's enabled services.
func planProviders(plan *config.PurchasePlan) []string {
	seen := map[string]struct{}{}
	var out []string
	for k := range plan.Services {
		provider, enabled := plan.Services[k].Provider, plan.Services[k].Enabled
		if _, dup := seen[provider]; enabled && !dup {
			seen[provider] = struct{}{}
			out = append(out, provider)
		}
	}
	sort.Strings(out)
	return out
}

// planUnchanged reports whether the locked plan still describes the step the
// tick listed: enabled, auto-purchase, same next date, same lock set. Dates are
// compared at the microsecond precision PostgreSQL stores, so an in-memory plan
// with nanoseconds (Linux clocks) is not mistaken for a changed one.
func planUnchanged(listed, locked *config.PurchasePlan, providers []string) bool {
	return locked.Enabled && locked.AutoPurchase == listed.AutoPurchase && locked.NextExecutionDate != nil && listed.NextExecutionDate != nil &&
		locked.NextExecutionDate.Truncate(time.Microsecond).Equal(listed.NextExecutionDate.Truncate(time.Microsecond)) &&
		strings.Join(planProviders(locked), ",") == strings.Join(providers, ",")
}

// attachStep resolves the step's recommendations under the locks and writes
// them. root is the pre-created empty root row, or nil when the step has none.
func (m *Manager) attachStep(ctx context.Context, tx pgx.Tx, locked *config.PurchasePlan, claim stepClaim, root *config.PurchaseExecution) (stepClaim, error) {
	if m.resolver == nil {
		return stepClaim{}, errNoRecommendationSource
	}
	if err := locked.RequireVetoWindow(); err != nil {
		return stepClaim{}, fmt.Errorf("plan %s step %d not attached: %w", locked.ID, claim.number, err)
	}
	accounts, err := m.config.GetPlanAccounts(ctx, locked.ID)
	if err != nil {
		return stepClaim{}, fmt.Errorf("failed to read accounts of plan %s: %w", locked.ID, err)
	}
	ids := make([]string, 0, len(accounts))
	for i := range accounts {
		ids = append(ids, accounts[i].ID)
	}
	res, err := m.resolveCumulative(ctx, tx, locked, claim.number, ids)
	if errors.Is(err, planstep.ErrNoMatch) {
		return m.noMatch(ctx, tx, locked, claim, root, res.Skipped)
	}
	if errors.Is(err, planstep.ErrTargetMet) {
		return m.targetMet(ctx, tx, locked, claim, root, res)
	}
	if errors.Is(err, planstep.ErrUnsupportedPlan) {
		return m.failStep(ctx, tx, locked, claim, root, err.Error())
	}
	if err != nil {
		return stepClaim{}, fmt.Errorf("plan %s step %d: %w", locked.ID, claim.number, err)
	}
	if len(res.Skipped) > 0 {
		logging.Warnf("plan %s step %d: %s", locked.ID, claim.number, skippedNote(res.Skipped))
	}
	recs := res.Recs
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	return m.attachWithinCap(ctx, tx, locked, claim, root, recs)
}

func (m *Manager) attachWithinCap(ctx context.Context, tx pgx.Tx, locked *config.PurchasePlan, claim stepClaim, root *config.PurchaseExecution, recs []config.RecommendationRecord) (stepClaim, error) {
	if locked.AutoPurchase || locked.MaxStepCommitment != nil {
		if capErr := CheckStepCommitmentCap(locked, recs); capErr != nil {
			return m.failStep(ctx, tx, locked, claim, root, fmt.Sprintf("%v (recommendations: %s)", capErr, recIDs(recs)))
		}
	}
	return m.writeAttached(ctx, tx, locked, claim, root, recs)
}

// resolveCumulative reads the frozen baseline and the plan's own ledger in the
// claim's transaction (both locks are held, so no concurrent tick can change
// them) and resolves the step against them. At step 1 it freezes the baseline
// from what the step saw, including when nothing sized to a purchase: a base
// whose target floors to 0 still has to be frozen or step 2 would find no
// baseline and strand. In that case the step is judged again against the
// frozen baseline, so "target met" and "hidden" are told apart exactly as at
// any later step.
func (m *Manager) resolveCumulative(ctx context.Context, tx pgx.Tx, locked *config.PurchasePlan, step int, ids []string) (planstep.Result, error) {
	baseline, err := m.config.GetPlanRampBaselineTx(ctx, tx, locked.ID)
	if err != nil {
		return planstep.Result{}, err
	}
	committed, err := m.config.ListPlanCommittedTx(ctx, tx, locked.ID)
	if err != nil {
		return planstep.Result{}, err
	}
	res, err := m.resolver.Resolve(ctx, locked, ids, step, baseline, committed)
	if step != 1 || baseline != nil || len(res.Baseline) == 0 {
		return res, err
	}
	if _, setErr := m.config.SetPlanRampBaselineTx(ctx, tx, locked.ID, res.Baseline); setErr != nil {
		return planstep.Result{}, setErr
	}
	if errors.Is(err, planstep.ErrNoMatch) {
		return m.resolver.Resolve(ctx, locked, ids, step, res.Baseline, committed)
	}
	return res, err
}

// skippedNote names the identities left out for lack of a baseline.
func skippedNote(skipped []string) string {
	shown := skipped
	if len(shown) > 5 {
		shown = shown[:5]
	}
	names := make([]string, len(shown))
	for i, k := range shown {
		names[i] = strings.ReplaceAll(k, "\x1f", "/")
	}
	return fmt.Sprintf("%d recommendation(s) without a frozen baseline skipped (under-buy): %s", len(skipped), strings.Join(names, ", "))
}

// targetMet completes a step whose cumulative target is already held as a
// no-op row and advances the ramp (platform#631 Q1). No email, no
// suppression, no purchase. The claim hands back nothing to notify.
func (m *Manager) targetMet(ctx context.Context, tx pgx.Tx, locked *config.PurchasePlan, claim stepClaim, root *config.PurchaseExecution, res planstep.Result) (stepClaim, error) {
	note := fmt.Sprintf("target already met: target %d, committed %d", res.Target, res.Committed)
	if len(res.Skipped) > 0 {
		note += "; " + skippedNote(res.Skipped)
	}
	rootID := ""
	if root != nil {
		rootID = root.ExecutionID
	}
	err := m.config.CompleteTargetMetStepTx(ctx, tx, locked.ID, claim.number, rootID, note)
	if errors.Is(err, config.ErrExecutionNotInExpectedStatus) {
		return stepClaim{}, fmt.Errorf("%w: execution %s changed before the step could be completed", errPlanStepSkipped, rootID)
	}
	if err != nil {
		return stepClaim{}, fmt.Errorf("plan %s step %d: %w", locked.ID, claim.number, err)
	}
	claim.targetMet = true
	return claim, nil
}

func recIDs(recs []config.RecommendationRecord) string {
	ids := make([]string, len(recs))
	for i := range recs {
		ids[i] = recs[i].ID
	}
	return strings.Join(ids, ", ")
}

// noMatch handles ErrNoMatch: no row until the grace window after the plan's
// date has passed (a cold or lagging recommendation cache is not a failure),
// then one failed row.
func (m *Manager) noMatch(ctx context.Context, tx pgx.Tx, locked *config.PurchasePlan, claim stepClaim, root *config.PurchaseExecution, skipped []string) (stepClaim, error) {
	global, err := m.config.GetGlobalConfig(ctx)
	if err != nil {
		return stepClaim{}, fmt.Errorf("failed to read grace period: %w", err)
	}
	grace := planGraceDays(locked, global)
	deadline := locked.NextExecutionDate.AddDate(0, 0, grace)
	if time.Now().Before(deadline) {
		return stepClaim{}, fmt.Errorf("%w: no recommendation matches plan %s yet, retrying until %s", errPlanStepSkipped, locked.ID, deadline.Format(time.RFC3339))
	}
	reason := fmt.Sprintf("no current recommendation matched plan %s step %d by %s (%d-day grace after the plan date)", locked.ID, claim.number, deadline.Format("2006-01-02"), grace)
	if len(skipped) > 0 {
		reason += "; " + skippedNote(skipped)
	}
	return m.failStep(ctx, tx, locked, claim, root, reason)
}

// failStep gives the step its one failed row: a new row written in tx, or the
// pre-created empty row handed back for a compare-and-set after commit.
func (m *Manager) failStep(ctx context.Context, tx pgx.Tx, locked *config.PurchasePlan, claim stepClaim, root *config.PurchaseExecution, reason string) (stepClaim, error) {
	claim.failureReason = reason
	if root != nil {
		claim.emptyRoot = root
		return claim, nil
	}
	fresh := &config.PurchaseExecution{
		PlanID: locked.ID, ExecutionID: uuid.New().String(), Status: "pending",
		StepNumber: claim.number, ScheduledDate: *locked.NextExecutionDate,
	}
	stampFailedPlanStep(fresh, reason)
	if err := m.config.SavePurchaseExecutionTx(ctx, tx, fresh); err != nil {
		return stepClaim{}, fmt.Errorf("failed to record plan step %d of plan %s as failed: %w", claim.number, locked.ID, err)
	}
	claim.failedNew = true
	return claim, nil
}

// writeAttached writes the resolved recommendations and their suppressions in
// the tick's transaction: a new pending row with NO token (the token is minted
// and rotated in only after the email goes out, so the row becomes notified
// only then), or the recommendations onto the pre-created empty root row.
func (m *Manager) writeAttached(ctx context.Context, tx pgx.Tx, locked *config.PurchasePlan, claim stepClaim, root *config.PurchaseExecution, recs []config.RecommendationRecord) (stepClaim, error) {
	var upfront, savings float64
	for i := range recs {
		upfront += recs[i].UpfrontCost
		savings += recs[i].Savings
	}
	global, err := m.config.GetGlobalConfig(ctx)
	if err != nil {
		return stepClaim{}, fmt.Errorf("failed to read grace periods for suppressions: %w", err)
	}
	exec := root
	if root == nil {
		exec = &config.PurchaseExecution{
			PlanID: locked.ID, ExecutionID: uuid.New().String(), Status: "pending", StepNumber: claim.number,
			ScheduledDate: attachScheduledDate(locked, time.Now()), IdempotencyKey: uuid.New().String(),
			Recommendations: recs, TotalUpfrontCost: upfront, EstimatedSavings: savings,
		}
		if err := m.config.SavePurchaseExecutionTx(ctx, tx, exec); err != nil {
			return stepClaim{}, fmt.Errorf("failed to record plan step %d of plan %s: %w", claim.number, locked.ID, err)
		}
	} else {
		ok, err := m.config.SetExecutionRecommendationsIfEmptyTx(ctx, tx, root.ExecutionID, recs, upfront, savings)
		if err != nil {
			return stepClaim{}, err
		}
		if !ok {
			return stepClaim{}, fmt.Errorf("%w: execution %s changed before recommendations could attach", errPlanStepSkipped, root.ExecutionID)
		}
		exec.Recommendations, exec.TotalUpfrontCost, exec.EstimatedSavings = recs, upfront, savings
	}
	sups := config.BuildSuppressions(recs, exec.ExecutionID, global, time.Now())
	for i := range sups {
		if err := m.config.CreateSuppressionTx(ctx, tx, &sups[i]); err != nil {
			return stepClaim{}, fmt.Errorf("failed to record suppressions for execution %s: %w", exec.ExecutionID, err)
		}
	}
	claim.existing = exec
	return claim, nil
}

// vetoWindowEnd is the earliest time a freshly emailed step may be bought:
// NotificationDaysBefore days from now.
func vetoWindowEnd(locked *config.PurchasePlan, now time.Time) time.Time {
	return now.AddDate(0, 0, locked.NotificationDaysBefore)
}

// attachScheduledDate is the later of the plan's next date and the end of the
// veto window, so the scheduler (which buys any notified row once its date
// passes) can never buy before the operator has had NotificationDaysBefore
// days to cancel, whether the plan date is past or only close.
func attachScheduledDate(locked *config.PurchasePlan, now time.Time) time.Time {
	if end := vetoWindowEnd(locked, now); locked.NextExecutionDate.Before(end) {
		return end
	}
	return *locked.NextExecutionDate
}
