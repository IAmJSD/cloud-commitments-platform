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
	"sort"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// ErrNoMatch means no stored recommendation matches the plan right now. It is
// not a failure of the plan: the cache may be cold or mid-collect, so callers
// retry on a later tick and fail only after the grace window.
var ErrNoMatch = errors.New("no current recommendation matches the plan")

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

// Resolve returns the recommendations the plan's next step would buy: for each
// enabled plan service, the recs Opportunities shows (strict read, enabled
// providers) restricted to the plan's term, payment and target accounts, sized
// to Coverage * PercentPerStep and priced from the unsuppressed stored row.
// accountIDs are the plan's target accounts; recs without an account are
// excluded. Every failure is returned; there is no unsized or over-show
// fallback. An empty result is ErrNoMatch.
func Resolve(ctx context.Context, src Source, store Store, plan *config.PurchasePlan, accountIDs []string) ([]config.RecommendationRecord, error) {
	if len(accountIDs) == 0 {
		return nil, fmt.Errorf("plan %s has no target accounts", plan.ID)
	}
	global, err := store.GetGlobalConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("read global config: %w", err)
	}
	allowed := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		allowed[id] = struct{}{}
	}
	var out []config.RecommendationRecord
	seen := map[string]struct{}{}
	for _, k := range sortedKeys(plan.Services) {
		svc := plan.Services[k]
		if !svc.Enabled {
			continue
		}
		recs, err := resolveService(ctx, src, store, &svc, plan.RampSchedule.PercentPerStep, accountIDs, allowed, global.EnabledProviders)
		if err == nil {
			err = claimIdentities(seen, recs)
		}
		if err != nil {
			return nil, fmt.Errorf("plan service %q: %w", k, err)
		}
		out = append(out, recs...)
	}
	if len(out) == 0 {
		return nil, ErrNoMatch
	}
	return out, nil
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

func resolveService(ctx context.Context, src Source, store Store, svc *config.ServiceConfig, percentPerStep float64,
	accountIDs []string, allowed map[string]struct{}, enabledProviders []string) ([]config.RecommendationRecord, error) {
	wantPayment, err := validateInputs(svc, percentPerStep)
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
		sized, keep, err := sizeFromStored(&rec, stored, svc.Coverage*percentPerStep)
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
func validateInputs(svc *config.ServiceConfig, percentPerStep float64) (string, error) {
	if svc.Coverage <= 0 || svc.Coverage > 100 || percentPerStep <= 0 || percentPerStep > 100 || svc.Term <= 0 {
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
func sizeFromStored(shown *config.RecommendationRecord, stored map[string]*config.RecommendationRecord, product float64) (config.RecommendationRecord, bool, error) {
	key, err := config.RecIdentityKey(shown)
	if err != nil {
		return config.RecommendationRecord{}, false, err
	}
	base := stored[key]
	if base == nil {
		return config.RecommendationRecord{}, false, errors.New("no stored row to price from (cache changed mid-read)")
	}
	return size(shown, base, product)
}

// size returns shown sized to product/10000 of itself (product =
// coverage*percentPerStep, multiplied before dividing so 100*10 over 10000
// stays exact) and priced from the unsuppressed stored row. keep=false means
// the rec sized to nothing.
func size(shown, stored *config.RecommendationRecord, product float64) (config.RecommendationRecord, bool, error) {
	if stored.Count <= 0 {
		return config.RecommendationRecord{}, false, fmt.Errorf("stored count %d cannot price a share", stored.Count)
	}
	out := *shown
	out.SuppressedCount, out.SuppressionExpiresAt, out.PrimarySuppressionExecutionID = 0, nil, nil
	out.Selected = true
	out.RecommendedCount = shown.Count
	var ratio float64
	var err error
	if common.IsSavingsPlan(common.ServiceType(stored.Service)) {
		ratio, err = sizeSP(&out, stored, product)
	} else {
		ratio = sizeCount(&out, stored, shown.Count, product)
	}
	if err != nil || ratio == 0 {
		return out, false, err
	}
	out.UpfrontCost = stored.UpfrontCost * ratio
	out.Savings = stored.Savings * ratio
	out.MonthlyCost = scaled(stored.MonthlyCost, ratio)
	out.OnDemandCost = scaled(stored.OnDemandCost, ratio)
	if out.UpfrontCost < 0 || (out.MonthlyCost != nil && *out.MonthlyCost < 0) ||
		out.UpfrontCost+monthlyTotal(&out) <= 0 {
		return out, false, errors.New("stored row carries no usable price")
	}
	return out, true, nil
}

// sizeCount sets out.Count to floor(shown*product/10000) and returns the
// discrete ratio to stored.Count; 0 means it sized to nothing.
func sizeCount(out, stored *config.RecommendationRecord, shown int, product float64) float64 {
	n := int(math.Floor(float64(shown) * product / 10000))
	if n <= 0 {
		return 0
	}
	out.Count = n
	return float64(n) / float64(stored.Count)
}

// sizeSP rewrites out's hourly_commitment (rounded down to whole cents, never
// up) and returns the ratio to the stored commitment; 0 means below one cent.
func sizeSP(out, stored *config.RecommendationRecord, product float64) (float64, error) {
	keys, hourly, err := decodeHourly(stored.Details)
	if err != nil {
		return 0, err
	}
	sized := math.Floor(hourly*product/100) / 100
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
