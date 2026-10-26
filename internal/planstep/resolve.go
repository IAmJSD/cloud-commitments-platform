// Package planstep resolves a purchase plan's current step into the
// recommendations it would buy. It reads only; nothing here attaches or
// executes, and no production caller exists yet (platform#631).
package planstep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// ErrNoMatch means no stored recommendation matches the plan right now. It is
// not a failure of the plan: the cache may be cold or mid-collect, so callers
// retry on a later tick and fail only after the grace window.
var ErrNoMatch = errors.New("no current recommendation matches the plan")

// ErrTargetMet means every matched recommendation already has this plan's
// cumulative target committed, so the step has nothing left to buy. It is
// distinct from ErrNoMatch (nothing matched, retry later).
var ErrTargetMet = errors.New("the plan's cumulative target is already met")

// spHourlyStep is the precision AWS accepts for a Savings Plans hourly
// commitment (the purchase client formats it with %.2f). Sizing rounds DOWN
// to whole cents (floor(hourly*product/100)/100) and drops anything below one step.
const spHourlyStep = 0.01

// Source is the strict recommendation read (suppressions and account
// overrides applied; their read errors returned). *scheduler.Scheduler.
type Source interface {
	ListRecommendationsStrict(ctx context.Context, filter config.RecommendationFilter) ([]config.RecommendationRecord, error)
}

// Store supplies the unsuppressed stored rows used for pricing and the
// enabled-providers setting. config.StoreInterface satisfies it.
type Store interface {
	ListStoredRecommendations(ctx context.Context, filter config.RecommendationFilter) ([]config.RecommendationRecord, error)
	GetGlobalConfig(ctx context.Context) (*config.GlobalConfig, error)
}

// Result is what Resolve decided for one step.
type Result struct {
	// Recs are the recommendations the step would buy.
	Recs []config.RecommendationRecord
	// Baseline is set only when step is 1: every matched count-based identity
	// mapped to its base (the existing baseline entry, else step-1 shown),
	// including recs that sized to 0. The caller freezes it; nil at step > 1.
	Baseline config.PlanRampBaseline
	// Skipped lists identities with no baseline at step > 1 (new recs, newly
	// added accounts). They are never frozen at first sight: under-buy.
	Skipped []string
	// Target and Committed total the judged identities' cumulative target and
	// what the plan already holds. Set only with ErrTargetMet, for the no-op
	// row's note.
	Target, Committed int
}

// ramp carries the cumulative-sizing inputs and accumulates its outputs.
type ramp struct {
	step      int
	cum       float64
	mode      config.RampAllocationMode
	count     int
	baseline  config.PlanRampBaseline
	committed map[string]config.PlanRampQuantity
	out       Result
}

// Resolve returns the recommendations the plan's step would buy: for each
// enabled plan service, the recs Opportunities shows (strict read, enabled
// providers) restricted to the plan's term, payment and target accounts, sized
// to the CUMULATIVE target and priced from the unsuppressed stored row. For a
// count rec: target = floor(base * Coverage * CumulativePercent(step) / 10000)
// and buy = clamp(target - committed, 0, shown). base is the frozen baseline
// entry, or at step 1 the shown count (post-suppression, never above demand);
// at step > 1 an identity without a baseline is skipped. baseline and
// committed are caller-supplied (this function does no extra store reads).
// accountIDs are the plan's target accounts; recs without an account are
// excluded. Every failure is returned; there is no unsized or over-show
// fallback. An empty result is ErrTargetMet only when a non-empty baseline
// exists and every baselined identity has its target committed (decided
// without `shown`); anything else, including a hidden identity with a gap or an
// empty step-1 read (no baseline returned), is ErrNoMatch.
// Savings Plans keep their per-step sizing (platform#631 Q7).
func Resolve(ctx context.Context, src Source, store Store, plan *config.PurchasePlan, accountIDs []string,
	step int, baseline config.PlanRampBaseline, committed map[string]config.PlanRampQuantity) (Result, error) {
	if err := Precheck(plan); err != nil {
		return Result{}, err
	}
	if step < 1 {
		return Result{}, fmt.Errorf("plan %s: step %d must be at least 1", plan.ID, step)
	}
	if len(accountIDs) == 0 {
		return Result{}, fmt.Errorf("plan %s has no target accounts", plan.ID)
	}
	global, err := store.GetGlobalConfig(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read global config: %w", err)
	}
	allowed := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		allowed[id] = struct{}{}
	}
	r := &ramp{step: step, cum: plan.RampSchedule.CumulativePercent(step), mode: plan.RampSchedule.AllocationMode, count: plan.RampSchedule.TotalSteps, baseline: baseline, committed: committed}
	if step == 1 {
		r.out.Baseline = config.PlanRampBaseline{}
	}
	if err := r.resolveServices(ctx, src, store, plan, accountIDs, allowed, global.EnabledProviders); err != nil {
		return Result{}, err
	}
	return r.out, r.emptyErr(plan)
}

// emptyErr classifies an empty result; nil when there is something to buy.
func (r *ramp) emptyErr(plan *config.PurchasePlan) error {
	if len(r.out.Recs) > 0 {
		return nil
	}
	if len(r.out.Baseline) == 0 {
		r.out.Baseline = nil // an empty baseline is never frozen
	}
	met, err := r.targetMet(plan)
	if err != nil {
		return err
	}
	if met {
		return ErrTargetMet
	}
	return ErrNoMatch
}

// resolveServices sizes every enabled plan service into r.out.Recs.
func (r *ramp) resolveServices(ctx context.Context, src Source, store Store, plan *config.PurchasePlan,
	accountIDs []string, allowed map[string]struct{}, enabledProviders []string) error {
	seen := map[string]struct{}{}
	for _, k := range sortedKeys(plan.Services) {
		svc := plan.Services[k]
		if !svc.Enabled {
			continue
		}
		recs, err := resolveService(ctx, src, store, &svc, plan.RampSchedule.PercentPerStep, r, accountIDs, allowed, enabledProviders)
		if err == nil {
			err = claimIdentities(seen, recs)
		}
		if err != nil {
			return fmt.Errorf("plan service %q: %w", k, err)
		}
		r.out.Recs = append(r.out.Recs, recs...)
	}
	return nil
}

// targetMet reports whether a non-empty frozen baseline exists and every
// baselined identity of an enabled plan service has its cumulative target
// committed. It reads no `shown`: an identity that is merely hidden (still has
// a gap) is not met, so it ends as ErrNoMatch, never a silent no-op advance.
func (r *ramp) targetMet(plan *config.PurchasePlan) (bool, error) {
	if len(r.baseline) == 0 {
		return false, nil
	}
	judged, target, committed := 0, 0, 0
	for key, base := range r.baseline {
		svc, ok := planServiceOf(plan, key)
		if !ok {
			continue
		}
		judged++
		t, err := r.target(base.Count, svc.Coverage)
		if err != nil {
			return false, err
		}
		c := r.committed[key].Count
		if err := r.validateCounts(c); err != nil {
			return false, err
		}
		if t > c {
			return false, nil
		}
		target, committed = target+t, committed+c
	}
	r.out.Target, r.out.Committed = target, committed
	return judged > 0, nil
}

// planServiceOf finds the enabled plan service an identity key belongs to
// (provider, service and term are fields 0, 2 and 6 of the key).
func planServiceOf(plan *config.PurchasePlan, key string) (config.ServiceConfig, bool) {
	f := strings.Split(key, "\x1f")
	if len(f) < 7 {
		return config.ServiceConfig{}, false
	}
	for _, k := range sortedKeys(plan.Services) {
		svc := plan.Services[k]
		if svc.Enabled && strings.EqualFold(svc.Provider, f[0]) && svc.Service == f[2] && strconv.Itoa(svc.Term) == f[6] {
			return svc, true
		}
	}
	return config.ServiceConfig{}, false
}

// Legacy targets multiply coverage*cum first; marked targets use exact shares.
func (r *ramp) target(base int, coverage float64) (int, error) {
	if r.mode == config.RampAllocationEqualCount {
		if err := r.validateCounts(base); err != nil {
			return 0, err
		}
		share, err := r.equalCountShare(coverage, r.step)
		if err != nil {
			return 0, err
		}
		return floorAllocation(share.Mul(share, big.NewRat(int64(base), 1)))
	}
	return int(math.Floor(float64(base) * (coverage * r.cum) / 10000)), nil
}

func sortedKeys(m map[string]config.ServiceConfig) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// claimIdentities refuses a rec already produced by another plan service,
// which would otherwise be bought twice.
func claimIdentities(seen map[string]struct{}, recs []config.RecommendationRecord) error {
	for i := range recs {
		key, err := config.RecIdentityKey(&recs[i])
		if err != nil {
			return err
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("recommendation %s matches more than one plan service", recs[i].ID)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func resolveService(ctx context.Context, src Source, store Store, svc *config.ServiceConfig, percentPerStep float64, r *ramp,
	accountIDs []string, allowed map[string]struct{}, enabledProviders []string) ([]config.RecommendationRecord, error) {
	wantPayment, err := r.validateInputs(svc, percentPerStep)
	if err != nil {
		return nil, err
	}
	filter := config.RecommendationFilter{Provider: svc.Provider, Service: svc.Service, AccountIDs: accountIDs}
	shown, err := src.ListRecommendationsStrict(ctx, filter)
	if err != nil {
		return nil, err
	}
	shown = config.FilterRecommendationsByEnabledProviders(shown, enabledProviders)
	stored, err := storedByIdentity(ctx, store, filter)
	if err != nil {
		return nil, err
	}
	var out []config.RecommendationRecord
	for i := range shown {
		rec := shown[i]
		match, err := matchesPlan(&rec, svc.Term, wantPayment, allowed)
		if err != nil {
			return nil, err
		}
		if !match {
			continue
		}
		sized, keep, err := r.sizeFromStored(&rec, stored, svc.Coverage, percentPerStep)
		if err != nil {
			return nil, fmt.Errorf("recommendation %s: %w", rec.ID, err)
		}
		if keep {
			out = append(out, sized)
		}
	}
	return out, nil
}

// validateInputs rejects sizing inputs outside (0,100] (custom ramps too) and
// returns the plan payment normalized for the provider.
func (r *ramp) validateInputs(svc *config.ServiceConfig, percentPerStep float64) (string, error) {
	if svc.Coverage <= 0 || svc.Coverage > 100 || svc.Term <= 0 || (r.mode == "" && (percentPerStep <= 0 || percentPerStep > 100)) {
		return "", fmt.Errorf("invalid sizing inputs (coverage %v, percent per step %v, term %d)", svc.Coverage, percentPerStep, svc.Term)
	}
	payment, ok := config.NormalizePaymentOption(svc.Provider, svc.Payment)
	if !ok || payment == "" {
		return "", fmt.Errorf("payment %q is not valid for provider %q", svc.Payment, svc.Provider)
	}
	return payment, nil
}

// matchesPlan reports whether rec has the plan service's term, payment and a
// target account. An unrecognized payment is an error, never a loosened match.
func matchesPlan(rec *config.RecommendationRecord, term int, wantPayment string, allowed map[string]struct{}) (bool, error) {
	if rec.Term != term || rec.CloudAccountID == nil {
		return false, nil
	}
	if _, ok := allowed[*rec.CloudAccountID]; !ok {
		return false, nil
	}
	got, ok := config.NormalizePaymentOption(rec.Provider, rec.Payment)
	if !ok {
		return false, fmt.Errorf("recommendation %s has unrecognized payment %q", rec.ID, rec.Payment)
	}
	return got == wantPayment, nil
}

// storedByIdentity indexes the unsuppressed stored rows by the full identity
// key; a duplicate key is refused because the join would be ambiguous.
func storedByIdentity(ctx context.Context, store Store, filter config.RecommendationFilter) (map[string]*config.RecommendationRecord, error) {
	rows, err := store.ListStoredRecommendations(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("list stored recommendations: %w", err)
	}
	stored := make(map[string]*config.RecommendationRecord, len(rows))
	for i := range rows {
		key, err := config.RecIdentityKey(&rows[i])
		if err != nil {
			return nil, err
		}
		if _, dup := stored[key]; dup {
			return nil, fmt.Errorf("stored recommendations contain more than one row for identity %q", key)
		}
		stored[key] = &rows[i]
	}
	return stored, nil
}

// sizeFromStored joins shown to its unsuppressed stored row on the full
// identity key, then sizes and prices it.
func (r *ramp) sizeFromStored(shown *config.RecommendationRecord, stored map[string]*config.RecommendationRecord, coverage, percentPerStep float64) (config.RecommendationRecord, bool, error) {
	key, err := config.RecIdentityKey(shown)
	if err != nil {
		return config.RecommendationRecord{}, false, err
	}
	base := stored[key]
	if base == nil {
		return config.RecommendationRecord{}, false, errors.New("no stored row to price from (cache changed mid-read)")
	}
	if common.IsSavingsPlan(common.ServiceType(base.Service)) {
		var share *big.Rat
		if r.mode == config.RampAllocationEqualCount {
			share, err = r.equalCountShare(coverage, 1)
			if err != nil {
				return config.RecommendationRecord{}, false, err
			}
		}
		return size(shown, base, coverage*percentPerStep, 0, share)
	}
	n, ok, err := r.countToBuy(key, shown.Count, coverage)
	if err != nil || !ok {
		return config.RecommendationRecord{}, false, err
	}
	return size(shown, base, 0, n, nil)
}

// countToBuy applies the cumulative formula to one count identity and records
// the baseline entry (step 1, shown > 0). ok=false means skip: no baseline at
// step > 1.
func (r *ramp) countToBuy(key string, shown int, coverage float64) (buy int, ok bool, err error) {
	if validationErr := r.validateCounts(shown, r.committed[key].Count); validationErr != nil {
		return 0, false, validationErr
	}
	base, frozen := r.baseline[key]
	switch {
	case !frozen && r.step == 1:
		base = config.PlanRampQuantity{Count: shown}
	case !frozen:
		r.out.Skipped = append(r.out.Skipped, key)
		return 0, false, nil
	}
	if r.step == 1 && base.Count > 0 {
		r.out.Baseline[key] = base
	}
	target, err := r.target(base.Count, coverage)
	if err != nil {
		return 0, false, err
	}
	gap := target - r.committed[key].Count
	if gap <= 0 {
		return 0, true, nil
	}
	return min(gap, shown), true, nil
}

// size returns shown sized to n units (count recs) or to product/10000 of its
// hourly commitment (Savings Plans; product = coverage*percentPerStep,
// multiplied before dividing) and priced from the unsuppressed stored row.
// keep=false means the rec sized to nothing.
func size(shown, stored *config.RecommendationRecord, product float64, n int, share *big.Rat) (config.RecommendationRecord, bool, error) {
	if stored.Count <= 0 {
		return config.RecommendationRecord{}, false, fmt.Errorf("stored count %d cannot price a share", stored.Count)
	}
	if err := checkStoredSPCount(stored); err != nil {
		return config.RecommendationRecord{}, false, err
	}
	out := *shown
	out.SuppressedCount, out.SuppressionExpiresAt, out.PrimarySuppressionExecutionID = 0, nil, nil
	out.Selected = true
	out.RecommendedCount = shown.Count
	var ratio float64
	var err error
	if common.IsSavingsPlan(common.ServiceType(stored.Service)) {
		ratio, err = sizeSP(&out, stored, product, share)
	} else {
		ratio = sizeCount(&out, stored, n)
	}
	if err != nil || ratio == 0 {
		return out, false, err
	}
	out.UpfrontCost = stored.UpfrontCost * ratio
	out.Savings = stored.Savings * ratio
	out.MonthlyCost = scaled(stored.MonthlyCost, ratio)
	out.OnDemandCost = scaled(stored.OnDemandCost, ratio)
	// The costs are stored's, so their currency is too (issue #361).
	out.CommitmentCurrency = stored.CommitmentCurrency
	if out.UpfrontCost < 0 || (out.MonthlyCost != nil && *out.MonthlyCost < 0) ||
		out.UpfrontCost+monthlyTotal(&out) <= 0 {
		return out, false, errors.New("stored row carries no usable price")
	}
	return out, true, nil
}

// sizeCount sets out.Count to n and returns the ratio to stored.Count; 0
// means it sized to nothing.
func sizeCount(out, stored *config.RecommendationRecord, n int) float64 {
	if n <= 0 {
		return 0
	}
	out.Count = n
	return float64(n) / float64(stored.Count)
}

// sizeSP rewrites out's hourly_commitment (rounded down to whole cents, never
// up) and returns the ratio to the stored commitment; 0 means below one cent.
func sizeSP(out, stored *config.RecommendationRecord, product float64, share *big.Rat) (float64, error) {
	keys, hourly, err := decodeHourly(stored.Details)
	if err != nil {
		return 0, err
	}
	sized := math.Floor(hourly*product/100) / 100
	if share != nil {
		sized, err = exactSPHourly(hourly, share)
		if err != nil {
			return 0, err
		}
	}
	if sized < spHourlyStep {
		return 0, nil
	}
	if out.Details, err = encodeHourly(keys, sized); err != nil {
		return 0, err
	}
	out.Count = stored.Count
	return sized / hourly, nil
}

func monthlyTotal(r *config.RecommendationRecord) float64 {
	if r.MonthlyCost == nil {
		return 0
	}
	return *r.MonthlyCost * float64(r.Term) * 12
}

func scaled(v *float64, ratio float64) *float64 {
	if v == nil {
		return nil
	}
	s := *v * ratio
	return &s
}

// decodeHourly splits Details into raw keys and its hourly_commitment so the
// rewrite can put every other key (platform, tenancy, ...) back untouched.
func decodeHourly(raw json.RawMessage) (keys map[string]json.RawMessage, hourly float64, err error) {
	if err = json.Unmarshal(raw, &keys); err != nil || keys == nil {
		return nil, 0, fmt.Errorf("savings plan details unreadable: %w", errors.Join(err, errors.New("not a JSON object")))
	}
	if err = json.Unmarshal(keys["hourly_commitment"], &hourly); err != nil || hourly <= 0 {
		return nil, 0, errors.New("savings plan details carry no positive hourly_commitment")
	}
	return keys, hourly, nil
}

func encodeHourly(keys map[string]json.RawMessage, hourly float64) (json.RawMessage, error) {
	v, err := json.Marshal(hourly)
	if err != nil {
		return nil, err
	}
	keys["hourly_commitment"] = v
	return json.Marshal(keys)
}
