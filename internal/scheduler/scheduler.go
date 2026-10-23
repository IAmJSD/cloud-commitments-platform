// Package scheduler handles scheduled recommendation collection.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/concurrency"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	awsrecommendations "github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	azureprovider "github.com/LeanerCloud/cloud-commitments-go/providers/azure"
	gcpprovider "github.com/LeanerCloud/cloud-commitments-go/providers/gcp"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/execution"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/oidc"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

// STSClient is the minimal AWS STS surface the scheduler uses to discover
// the Lambda's own AWS account ID for the ambient-path account-tagging fix
// (issue #604). Mirrors purchase.STSClient deliberately — both packages
// need the same single call, and cross-package imports would tangle the
// purchase ↔ scheduler dependency.
type STSClient interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// SchedulerConfig holds configuration for the scheduler.
type SchedulerConfig struct { //nolint:revive // exported: doc comment style intentional
	ConfigStore     config.StoreInterface
	PurchaseManager ManagerInterface
	EmailSender     email.SenderInterface
	ProviderFactory provider.FactoryInterface
	CredentialStore credentials.CredentialStore
	OIDCSigner      oidc.Signer
	AssumeRoleSTS   credentials.STSClient
	STSClient       STSClient
	DashboardURL    string
	OIDCIssuerURL   string
	IsLambda        bool
}

// CollectResult holds the result of collecting recommendations.
//
// SuccessfulProviders + FailedProviders track per-provider outcomes so the
// persistence layer (UpsertRecommendations) can scope the delete-stale
// clause to providers that actually ran, and the frontend banner can
// surface the specific failures.
type CollectResult struct {
	FailedProviders     map[string]string `json:"failed_providers,omitempty"`
	SuccessfulProviders []string          `json:"successful_providers,omitempty"`
	Recommendations     int               `json:"recommendations"`
	TotalSavings        float64           `json:"total_savings"`
}

// ManagerInterface defines the purchase manager methods used by scheduler.
type ManagerInterface interface {
	ProcessScheduledPurchases(ctx context.Context) (*purchase.ProcessResult, error)
	SendUpcomingPurchaseNotifications(ctx context.Context) (*purchase.NotificationResult, error)
	// FireScheduledDelayedPurchases fires purchase_executions in status=scheduled
	// whose scheduled_execution_at is in the past (Gmail-style pre-fire delay,
	// issue #291 wave-2). Called on the "fire_scheduled_purchases" scheduler tick.
	FireScheduledDelayedPurchases(ctx context.Context) (*purchase.FireResult, error)
}

// Scheduler handles scheduled tasks.
type Scheduler struct {
	stsClient       STSClient
	purchase        ManagerInterface
	email           email.SenderInterface
	providerFactory provider.FactoryInterface
	credStore       credentials.CredentialStore
	oidcSigner      oidc.Signer
	assumeRoleSTS   credentials.STSClient
	config          config.StoreInterface
	dashboardURL    string
	oidcIssuerURL   string
	cacheTTL        time.Duration
	collecting      atomic.Bool
	isLambda        bool
}

// defaultCacheTTL is the fallback when CUDLY_RECOMMENDATION_CACHE_TTL is
// unset or invalid. 6h is shorter than the default 1-day cron so
// opportunistic refresh closes the gap when users are active.
const defaultCacheTTL = 6 * time.Hour

// NewScheduler creates a new scheduler.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	factory := cfg.ProviderFactory
	if factory == nil {
		factory = &provider.DefaultFactory{}
	}

	return &Scheduler{
		config:          cfg.ConfigStore,
		purchase:        cfg.PurchaseManager,
		email:           cfg.EmailSender,
		dashboardURL:    cfg.DashboardURL,
		providerFactory: factory,
		credStore:       cfg.CredentialStore,
		oidcSigner:      cfg.OIDCSigner,
		oidcIssuerURL:   cfg.OIDCIssuerURL,
		assumeRoleSTS:   cfg.AssumeRoleSTS,
		stsClient:       cfg.STSClient,
		isLambda:        cfg.IsLambda,
		cacheTTL:        cacheTTLFromEnv(),
	}
}

// IsLambda reports the Lambda mode this scheduler was constructed with
// (SchedulerConfig.IsLambda). It never re-reads the environment: the value is
// owned by whoever built the scheduler, so callers can assert that the mode
// they threaded in is the one that gates background goroutines (issue #110).
func (s *Scheduler) IsLambda() bool {
	return s.isLambda
}

// cacheTTLFromEnv parses CUDLY_RECOMMENDATION_CACHE_TTL using
// time.ParseDuration. Falls back to defaultCacheTTL on parse error (with
// a warning) so a mis-set env doesn't take the feature offline.
func cacheTTLFromEnv() time.Duration {
	v := os.Getenv("CUDLY_RECOMMENDATION_CACHE_TTL")
	if v == "" {
		return defaultCacheTTL
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		logging.Warnf("CUDLY_RECOMMENDATION_CACHE_TTL invalid (%q); using default %s", v, defaultCacheTTL)
		return defaultCacheTTL
	}
	return d
}

// CollectRecommendations fetches recommendations from all configured cloud providers.
// Persists results to the recommendations cache so read handlers can serve
// from SQL instead of re-fetching live.
//
// Bookkeeping: clears last_collection_started_at on exit (success or
// failure) so the frontend polling loop can detect completion, but only if
// ownerToken is non-empty and still matches last_collection_owner_id
// (issue #261 compare-and-clear guard). The scheduler is invoked either by
// the cron EventBridge rule or by an async self-invoke from the POST
// /api/recommendations/refresh handler. In the async case,
// MarkCollectionStarted has already set last_collection_started_at and
// returned the token that ownerToken carries here; kickColdStartCollection's
// background collect does too (issue #106), for the same reason. The cron
// case calls with an empty ownerToken since it never wins a marker of its
// own (it coordinates via the advisory lock instead, see
// HandleScheduledTask), and the clear is skipped entirely rather than
// clearing unconditionally,
// which previously let a cron run wipe a concurrent user-triggered run's
// marker. This deferred clear is the ONLY place that touches started_at:
// persistCollection's SetRecommendationsCollectionError call (below, on a
// provider failure) intentionally leaves started_at alone, so a tokenless
// run hitting a routine provider error cannot wipe another run's marker
// either.
func (s *Scheduler) CollectRecommendations(ctx context.Context, ownerToken string) (*CollectResult, error) {
	logging.Info("Collecting recommendations from cloud providers...")

	// Clear last_collection_started_at on exit so the frontend knows the
	// collection has finished. Use a fresh background context with a short
	// timeout: the request ctx may already be canceled by the time the
	// defer runs (e.g. caller deadline expired during a slow collect),
	// which would cause ClearCollectionStarted to fail and leave the
	// "in flight" marker until the 5-min auto-recovery window kicks in.
	defer func() {
		clearCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.clearCollectionStartedBestEffort(clearCtx, ownerToken)
	}()

	// Get global config
	globalCfg, err := s.config.GetGlobalConfig(ctx)
	if err != nil {
		return nil, err
	}

	// Attach a shared semaphore to ctx so every leaf goroutine in the
	// recommendations-collection fan-out tree (AWS service, Azure service,
	// GCP region×service) acquires one slot before issuing its cloud-API
	// call and releases it after. This bounds aggregate concurrent IO across
	// the whole tree at CUDLY_MAX_PARALLELISM (default 20) regardless of how
	// nested the dispatch is — without it, peak concurrency multiplies
	// through the nested fan-outs and can exhaust Lambda memory before work
	// completes (observed with a 512 MB function in dev). Intermediate
	// dispatchers (provider, account, GCP region) do NOT acquire — they only
	// launch sub-goroutines — so no goroutine can deadlock by holding a
	// permit while waiting for sub-permits.
	maxParallelism := concurrency.MaxParallelismFromEnv()
	sem := semaphore.NewWeighted(int64(maxParallelism))
	ctx = concurrency.WithSharedSemaphore(ctx, sem)
	logging.Infof("Recommendations collection: aggregate parallelism cap = %d", maxParallelism)

	// Collect recommendations from each enabled provider concurrently, tracking
	// per-provider outcomes so persistence can scope stale-row eviction to
	// (provider, account) pairs that actually ran. A partial collection (e.g.
	// one of three Azure subscriptions failed) preserves the failed pairs'
	// previous-cycle rows instead of wiping the whole provider's slice.
	//
	// Provider-level fan-out under errgroup. Each goroutine returns nil to the
	// group so a single provider's failure does not cancel siblings — matches
	// the previous loop's `continue`-on-error behavior. Per-provider results
	// are written into a map under a single mutex; the merge then walks
	// EnabledProviders in config order so successfulProviders ordering is
	// deterministic regardless of goroutine completion order. After Wait, ctx
	// cancellation is propagated. No concurrency cap — the universe is at
	// most 3 providers.
	allRecommendations, totalSavings, successfulProviders, successfulCollects, failedProviders, partial, err := s.collectAllProviders(ctx, globalCfg)
	if err != nil {
		return nil, err
	}

	logging.Infof("Collected %d recommendations with $%.2f/month potential savings",
		len(allRecommendations), totalSavings)

	s.persistCollection(ctx, allRecommendations, successfulCollects, failedProviders, partial)

	// Send notification if we have recommendations
	if len(allRecommendations) > 0 && totalSavings > 0 {
		// Sort by savings descending to show top recommendations in email
		sort.Slice(allRecommendations, func(i, j int) bool {
			return allRecommendations[i].Savings > allRecommendations[j].Savings
		})

		data := email.NotificationData{
			DashboardURL: s.dashboardURL,
			TotalSavings: totalSavings,
		}
		for _rvc := range allRecommendations {
			rec := allRecommendations[_rvc]
			if len(data.Recommendations) >= 10 { // Limit to top 10 in email
				break
			}
			data.Recommendations = append(data.Recommendations, email.RecommendationSummary{
				Service:        rec.Service,
				ResourceType:   rec.ResourceType,
				Engine:         rec.Engine,
				Region:         rec.Region,
				Count:          rec.Count,
				MonthlySavings: rec.Savings,
			})
		}

		if err := s.email.SendNewRecommendationsNotification(ctx, data); err != nil {
			logging.Errorf("Failed to send notification: %v", err)
		}
	}

	return &CollectResult{
		Recommendations:     len(allRecommendations),
		TotalSavings:        totalSavings,
		SuccessfulProviders: successfulProviders,
		FailedProviders:     failedProviders,
	}, nil
}

// providerOutcome bundles a single provider's collection outcome for the
// deterministic merge in collectAllProviders. Only one of recs/err is
// meaningful per outcome — err != nil means the provider failed entirely
// (mirrors the pre-fan-out `continue` branch); err == nil means the provider
// succeeded (possibly partially — partial-account-failure semantics live in
// fanOutPerAccount, not here).
//
// gap is non-nil when at least one of the provider's sweeps was incomplete;
// it is surfaced in recommendations_state.last_collection_partial (#87).
type providerOutcome struct {
	err                 error
	gap                 *sweepGap
	recs                []config.RecommendationRecord
	succeededAccountIDs []string
}

// collectAllProviders fans out provider collection (AWS / Azure / GCP) under
// errgroup. Each goroutine returns nil so a single provider's error does not
// cancel siblings; per-provider results are written into a map under a single
// mutex. After Wait, ctx cancellation is propagated. The merge then walks
// EnabledProviders in config order so successfulProviders / successfulCollects
// / allRecommendations ordering is deterministic regardless of goroutine
// completion order — keeps existing tests stable. No concurrency cap; the
// universe is at most 3 providers.
//
// Extracted from CollectRecommendations to keep that function under the
// project's gocyclo gate (.golangci.yml min-complexity: 15) after the
// errgroup + post-Wait ctx.Err() block was added.
func (s *Scheduler) collectAllProviders(ctx context.Context, globalCfg *config.GlobalConfig) ( //nolint:gocritic // tooManyResultsChecker: multiple returns intentional
	allRecommendations []config.RecommendationRecord,
	totalSavings float64,
	successfulProviders []string,
	successfulCollects []config.SuccessfulCollect,
	failedProviders map[string]string,
	partial *config.RecommendationsCollectionPartial,
	err error,
) {
	failedProviders = map[string]string{}

	var (
		mu       sync.Mutex
		outcomes = make(map[string]providerOutcome, len(globalCfg.EnabledProviders))
	)

	g, gctx := errgroup.WithContext(ctx)

	for _, providerName := range globalCfg.EnabledProviders {
		providerName := providerName // capture per-iteration
		g.Go(func() error {
			logging.Infof("Collecting recommendations from %s...", providerName)
			recs, succeededAccountIDs, gap, perr := s.collectProviderRecommendations(gctx, providerName, globalCfg)
			mu.Lock()
			outcomes[providerName] = providerOutcome{
				recs:                recs,
				succeededAccountIDs: succeededAccountIDs,
				gap:                 gap,
				err:                 perr,
			}
			mu.Unlock()
			return nil // error isolation: per-provider failures don't cancel siblings
		})
	}

	// Wait for all goroutines. g.Wait() always returns nil because every
	// goroutine returns nil. After Wait, propagate ctx cancellation so
	// callers can distinguish "all providers completed" from "the parent
	// ctx was canceled mid-fan-out".
	if waitErr := g.Wait(); waitErr != nil {
		// Goroutines return nil to isolate per-provider failures; non-nil is unexpected.
		logging.Warnf("collectAllProviders: errgroup.Wait returned unexpected error: %v", waitErr)
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, 0, nil, nil, nil, nil, cerr
	}

	// Deterministic merge: walk EnabledProviders in config order so
	// successfulProviders ordering is independent of goroutine completion
	// order — keeps existing tests stable.
	for _, providerName := range globalCfg.EnabledProviders {
		out, ok := outcomes[providerName]
		if !ok {
			continue
		}
		if out.err != nil {
			logging.Errorf("Failed to collect %s recommendations: %v", providerName, out.err)
			failedProviders[providerName] = out.err.Error()
			continue
		}
		successfulProviders = append(successfulProviders, providerName)
		successfulCollects = append(successfulCollects, expandSuccessfulCollects(providerName, out.succeededAccountIDs)...)
		for _rvc := range out.recs {
			rec := out.recs[_rvc]
			totalSavings += rec.Savings
		}
		allRecommendations = append(allRecommendations, out.recs...)
		partial = appendPartialProvider(partial, providerName, out.gap)
	}
	return allRecommendations, totalSavings, successfulProviders, successfulCollects, failedProviders, partial, nil
}

// appendPartialProvider adds providerName's incomplete-sweep summary to
// partial, allocating it on first use. A complete provider (nil gap) leaves
// partial unchanged, so a collection whose every sweep was complete yields
// nil and persistCollection leaves the column cleared.
func appendPartialProvider(partial *config.RecommendationsCollectionPartial, providerName string, gap *sweepGap) *config.RecommendationsCollectionPartial {
	if gap == nil {
		return partial
	}
	if partial == nil {
		partial = &config.RecommendationsCollectionPartial{}
	}
	partial.Providers = append(partial.Providers, gap.toProviderSweep(providerName))
	return partial
}

// clearCollectionStartedBestEffort clears last_collection_started_at on the
// scheduler's exit path. Best-effort — a failure here is logged but does not
// prevent returning the collection result. Extracted so CollectRecommendations
// stays under the cyclomatic-complexity gate.
//
// A caller that never won MarkCollectionStarted (the cron) passes an empty
// ownerToken and by design owns no marker to clear; this is an explicit,
// logged skip, not a silent fallback.
func (s *Scheduler) clearCollectionStartedBestEffort(ctx context.Context, ownerToken string) {
	if ownerToken == "" {
		logging.Debugf("skipping collection-started clear: caller holds no owner token (cron run)")
		return
	}
	if err := s.config.ClearCollectionStarted(ctx, ownerToken); err != nil {
		logging.Errorf("failed to clear collection started: %v", err)
	}
}

// persistCollection writes the collection results to the recommendations
// cache via UpsertRecommendations, which:
//
//   - Upserts each row by natural key so re-collected recommendations
//     reuse the same PK across runs (stable IDs for frontend).
//   - Evicts stale rows (collected_at < $now) but ONLY for the
//     (provider, account) pairs that successfully collected in this run.
//     Failed pairs' rows stay visible so the dashboard isn't blanked out
//     by transient cloud-API failures, even when one of several accounts
//     under the same provider succeeded.
//
// On any failure (including partial), the collection error is additionally
// recorded in recommendations_state so the frontend banner renders while
// the user still sees the valid rows we managed to upsert.
//
// UpsertRecommendations also resets last_collection_partial, so a complete
// collection clears the previous run's partial marker; a non-nil partial
// re-records it straight after (issue #87). The two writes mirror the
// existing error-column pattern and are not atomic: if the second write
// fails, the run is logged and reads as complete until the next collection.
func (s *Scheduler) persistCollection(ctx context.Context, recs []config.RecommendationRecord, successfulCollects []config.SuccessfulCollect, failedProviders map[string]string, partial *config.RecommendationsCollectionPartial) {
	if err := s.config.UpsertRecommendations(ctx, time.Now().UTC(), recs, successfulCollects); err != nil {
		logging.Errorf("Failed to persist recommendations: %v", err)
		if setErr := s.config.SetRecommendationsCollectionError(ctx, err.Error()); setErr != nil {
			logging.Errorf("Failed to record write error: %v", setErr)
		}
		return
	}

	if len(failedProviders) > 0 {
		if err := s.config.SetRecommendationsCollectionError(ctx, joinProviderErrors(failedProviders)); err != nil {
			logging.Errorf("Failed to record collection error: %v", err)
		}
	}

	if partial != nil {
		if err := s.config.SetRecommendationsCollectionPartial(ctx, partial); err != nil {
			logging.Errorf("Failed to record partial collection: %v", err)
		}
	}
}

// joinProviderErrors formats a "provider: err; provider: err" string for
// the state table's last_collection_error column.
func joinProviderErrors(failed map[string]string) string {
	parts := make([]string, 0, len(failed))
	for p, msg := range failed {
		parts = append(parts, fmt.Sprintf("%s: %s", p, msg))
	}
	sort.Strings(parts) // deterministic ordering for test stability
	return strings.Join(parts, "; ")
}

// collectProviderRecommendations collects recommendations from a specific
// provider. The succeededAccountIDs return value is the per-account roster
// that completed in this run; the scheduler threads it into
// []config.SuccessfulCollect for account-scoped eviction. For the AWS
// ambient path (no registered accounts) the slice contains a single empty
// string sentinel that expandSuccessfulCollects translates into a nil
// CloudAccountID. The *sweepGap is non-nil when some account's sweep was
// incomplete (its rows are kept; see tolerateIncompleteSweep). Provider-level
// errors propagate unchanged.
func (s *Scheduler) collectProviderRecommendations(ctx context.Context, providerName string, globalCfg *config.GlobalConfig) ([]config.RecommendationRecord, []string, *sweepGap, error) {
	switch providerName {
	case "aws":
		return s.collectAWSRecommendations(ctx, globalCfg)
	case "azure":
		return s.collectAzureRecommendations(ctx, globalCfg)
	case "gcp":
		return s.collectGCPRecommendations(ctx, globalCfg)
	default:
		logging.Warnf("Unknown provider: %s", providerName)
		return nil, nil, nil, nil
	}
}

// ambientEvictionRoster builds the succeeded-account roster for an ambient
// (unregistered-credential) collection. An incomplete sweep yields an empty
// roster, so UpsertRecommendations evicts nothing for it and the
// subscriptions the sweep never queried keep their previous rows -- see
// tolerateIncompleteSweep and issue #1652.
//
// accountID is "" when the host identity did not resolve to a registered
// cloud_accounts row; that empty-string sentinel is what
// expandSuccessfulCollects maps to a nil CloudAccountID.
func ambientEvictionRoster(complete bool, accountID string) []string {
	if !complete {
		return nil
	}
	return []string{accountID}
}

// expandSuccessfulCollects converts the per-account ID slice returned by
// each provider collect into the typed []SuccessfulCollect the persist
// layer expects. The empty-string sentinel is translated into a nil
// CloudAccountID so the eviction's account_key join uses uuid.Nil
// (matching the AWS ambient-credential path).
func expandSuccessfulCollects(providerName string, accountIDs []string) []config.SuccessfulCollect {
	out := make([]config.SuccessfulCollect, 0, len(accountIDs))
	for _, id := range accountIDs {
		sc := config.SuccessfulCollect{Provider: providerName}
		if id != "" {
			id := id // pin
			sc.CloudAccountID = &id
		}
		out = append(out, sc)
	}
	return out
}

// collectAWSRecommendations fans out across all enabled AWS accounts.
// If no accounts are registered and CUDly runs on AWS, it falls back to
// ambient credentials (backward compatibility with single-account setups).
// Returns the merged recommendations + the IDs of accounts that succeeded
// (or [""] for the ambient path so the caller can synthesize a nil
// CloudAccountID for eviction).
func (s *Scheduler) collectAWSRecommendations(ctx context.Context, globalCfg *config.GlobalConfig) ([]config.RecommendationRecord, []string, *sweepGap, error) {
	accounts, err := s.enabledAccounts(ctx, "aws")
	if err != nil {
		return nil, nil, nil, err
	}

	// Backward-compatible fallback: no registered accounts → ambient credentials
	if len(accounts) == 0 {
		recs, gap, err := s.collectAWSAmbient(ctx, globalCfg)
		if err != nil {
			return nil, nil, nil, err
		}
		// Issue #604: if the Lambda's STS identity matches a registered
		// (possibly disabled) cloud_accounts row, tag every rec with that
		// account's UUID so the approve modal renders the account name
		// instead of `(ambient)`. Best-effort — STS or store errors must
		// NOT fail the collection; we just keep the pre-fix nil tagging
		// in those cases.
		if acctID := s.resolveAmbientHostAccountID(ctx); acctID != "" {
			recs = s.tagAccount(recs, acctID)
			return recs, ambientEvictionRoster(gap == nil, acctID), gap.forAccount(), nil
		}
		return recs, ambientEvictionRoster(gap == nil, ""), gap.forAccount(), nil
	}

	recs, outcome := fanOutPerAccount(ctx, "AWS", accounts, func(ctx context.Context, acct config.CloudAccount) ([]config.RecommendationRecord, *sweepGap, error) {
		return s.collectAWSForAccount(ctx, globalCfg, acct)
	})
	if outcome.FailedCount == len(accounts) && len(accounts) > 0 {
		return nil, nil, nil, errAllAccountsFailed("AWS", outcome)
	}
	return recs, outcome.SucceededAccountIDs, outcome.Gap, nil
}

// accountOutcome is the per-provider summary returned by
// fanOutPerAccount alongside the merged recommendations slice. The
// caller uses this to decide whether the provider's collection
// succeeded overall: if every registered account failed
// (FailedCount == len(accounts) > 0) the caller should surface an
// error so the provider lands in failedProviders + the freshness
// banner shows the failure. Partial success (some accounts succeeded)
// stays a success at the provider level — the per-account [ERROR]
// logs are the operator signal for the failed minority.
//
// SucceededAccountIDs lists the IDs of accounts whose collection
// completed IN FULL; the scheduler threads these into the
// []SuccessfulCollect slice that drives UpsertRecommendations'
// account-scoped eviction. Order is not preserved — the eviction
// query treats it as a set.
//
// IncompleteAccountIDs lists accounts that returned usable rows from a
// sweep that did not cover every subscription it was asked to. They are
// counted in SucceededCount (their rows are kept and upserted, and they
// must not trip the all-accounts-failed guard) but are deliberately
// absent from SucceededAccountIDs, because eviction is keyed on the
// account while a sweep fails per subscription; evicting on a partial
// sweep deletes the un-queried subscriptions' previous rows. See
// tolerateIncompleteSweep and issue #1652.
//
// Gap is the merged incomplete-sweep summary across IncompleteAccountIDs
// (nil when every succeeded account swept completely), surfaced in
// recommendations_state.last_collection_partial (issue #87).
type accountOutcome struct {
	Gap                  *sweepGap
	LastErr              string
	SucceededAccountIDs  []string
	IncompleteAccountIDs []string
	SucceededCount       int
	FailedCount          int
}

// fanOutPerAccount runs fn concurrently across accounts, bounded by the
// shared CUDLY_MAX_ACCOUNT_PARALLELISM env var. Per-account errors are
// logged and the run continues — the stale-row preservation in the
// UpsertRecommendations write path (see commit "parallelise collection")
// keeps those accounts' older rows visible. Order is not preserved; the
// SQL read in ListStoredRecommendations re-sorts anyway.
//
// Returns the merged recommendations + an accountOutcome the caller
// uses to detect "all accounts failed" → return error → provider
// flagged as failed in CollectRecommendations.
func fanOutPerAccount(
	ctx context.Context,
	providerLabel string,
	accounts []config.CloudAccount,
	fn func(ctx context.Context, acct config.CloudAccount) ([]config.RecommendationRecord, *sweepGap, error),
) ([]config.RecommendationRecord, accountOutcome) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(execution.ConcurrencyFromEnv())

	// One mutex guards both the recs accumulator AND the outcome
	// counters — the same critical section, no second mutex needed.
	var mu sync.Mutex
	var all []config.RecommendationRecord
	var outcome accountOutcome

	for _rvc := range accounts {
		acct := accounts[_rvc]
		g.Go(func() error {
			recs, gap, err := fn(gctx, acct)
			if err != nil {
				if isAccountPermissionError(providerLabel, err) {
					// Operator-fixable misconfiguration (missing IAM role
					// on the deploy SA): log at WARN so a single
					// misconfigured account doesn't drown out other log
					// signals. The collection still counts as a per-account
					// failure — provider-success semantics are unchanged.
					logging.Warnf("%s account %s (%s) permission gap (operator action needed: grant required IAM role): %v",
						providerLabel, acct.Name, acct.ExternalID, err)
				} else {
					logging.Errorf("%s account %s (%s): %v", providerLabel, acct.Name, acct.ExternalID, err)
				}
				mu.Lock()
				outcome.FailedCount++
				outcome.LastErr = fmt.Sprintf("account %s (%s): %v", acct.Name, acct.ExternalID, err)
				mu.Unlock()
				return nil // never fail the whole group — partial is fine
			}
			mu.Lock()
			all = append(all, recs...)
			// A partially-swept account counts as succeeded -- its rows are
			// kept and upserted, and it must not trip the all-accounts-failed
			// guard -- but it is deliberately withheld from
			// SucceededAccountIDs, the roster that authorizes stale-row
			// eviction. See tolerateIncompleteSweep for why (issue #1652).
			outcome.SucceededCount++
			if gap == nil {
				outcome.SucceededAccountIDs = append(outcome.SucceededAccountIDs, acct.ID)
			} else {
				outcome.IncompleteAccountIDs = append(outcome.IncompleteAccountIDs, acct.ID)
				outcome.Gap = mergeSweepGaps(outcome.Gap, gap.forAccount())
			}
			mu.Unlock()
			return nil
		})
	}
	if waitErr := g.Wait(); waitErr != nil {
		// Goroutines return nil to isolate per-account failures; non-nil is unexpected.
		logging.Warnf("fanOutPerAccount: errgroup.Wait returned unexpected error: %v", waitErr)
	}
	return all, outcome
}

// isAccountPermissionError reports whether err from a per-account
// collection call represents an operator-fixable IAM permission gap
// (rather than a genuine collector error). Used by fanOutPerAccount to
// downgrade the log severity from ERROR to WARN — see issue #57: a
// single misconfigured GCP account otherwise emits an [ERROR] line on
// every scheduler tick (~15 min) and drowns out other log signals.
//
// Currently dispatches per provider:
//   - "GCP": gcpprovider.IsPermissionError (HTTP 403 / gRPC PermissionDenied)
//   - other providers: false (existing ERROR behavior preserved until
//     analogous predicates are added for AWS/Azure)
func isAccountPermissionError(providerLabel string, err error) bool {
	if err == nil {
		return false
	}
	switch providerLabel {
	case "GCP":
		return gcpprovider.IsPermissionError(err)
	default:
		return false
	}
}

// errAllAccountsFailed wraps an accountOutcome's LastErr so callers
// (the per-provider collect funcs) can surface a single error to
// CollectRecommendations when every registered account failed.
func errAllAccountsFailed(providerLabel string, outcome accountOutcome) error {
	return fmt.Errorf("%s: all %d accounts failed; last error: %s",
		providerLabel, outcome.FailedCount, outcome.LastErr)
}

func (s *Scheduler) collectAWSAmbient(ctx context.Context, globalCfg *config.GlobalConfig) ([]config.RecommendationRecord, *sweepGap, error) {
	prov, err := s.providerFactory.CreateAndValidateProvider(ctx, "aws", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create AWS provider: %w", err)
	}
	return s.fetchAndConvert(ctx, prov, "aws", nil, s.hostAWSAccountID(ctx), globalCfg)
}

// hostAWSAccountID returns the Lambda's own AWS account ID via STS
// GetCallerIdentity, or "" when STS is unavailable or fails (logged at warn).
func (s *Scheduler) hostAWSAccountID(ctx context.Context) string {
	if s.stsClient == nil {
		return ""
	}
	stsCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := s.stsClient.GetCallerIdentity(stsCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		logging.Warnf("ambient host-account lookup: STS GetCallerIdentity failed: %v", err)
		return ""
	}
	if out == nil || out.Account == nil {
		return ""
	}
	return *out.Account
}

// resolveAmbientHostAccountID looks up the Lambda's own AWS account ID
// (via STS GetCallerIdentity) and, if it matches a registered
// cloud_accounts row by (provider="aws", external_id), returns that
// row's UUID. Returns "" when STS is unavailable, the lookup fails, or
// no registered account matches — callers fall back to the pre-fix
// nil tagging in that case, preserving the truly-orphan deployment.
//
// All errors are intentionally swallowed (logged at debug/warn) — this
// is a best-effort UX improvement on the ambient path and must not
// break the collection.
func (s *Scheduler) resolveAmbientHostAccountID(ctx context.Context) string {
	hostAcctID := s.hostAWSAccountID(ctx)
	if hostAcctID == "" {
		return ""
	}

	acct, err := s.config.GetCloudAccountByExternalID(ctx, "aws", hostAcctID)
	if err != nil {
		logging.Warnf("ambient host-account lookup: GetCloudAccountByExternalID(aws,%s) failed: %v", hostAcctID, err)
		return ""
	}
	if acct == nil {
		// Truly orphan deployment — preserve pre-fix nil tagging.
		return ""
	}
	return acct.ID
}

// resolveAmbientAccountID is the provider-agnostic counterpart to
// resolveAmbientHostAccountID: given the host's external identifier (subscription
// ID for Azure, project ID for GCP) it checks whether a registered cloud_accounts
// row exists for (provider, externalID) and returns its UUID. Returns "" on any
// error or when no row matches, preserving the pre-fix nil-tagging behavior so
// truly-orphan deployments are unaffected. All errors are intentionally swallowed
// (logged at warn) — this is a best-effort UX improvement on the ambient path
// and must not break the collection.
func (s *Scheduler) resolveAmbientAccountID(ctx context.Context, providerName, externalID string) string {
	if externalID == "" {
		return ""
	}
	acct, err := s.config.GetCloudAccountByExternalID(ctx, providerName, externalID)
	if err != nil {
		logging.Warnf("ambient host-account lookup: GetCloudAccountByExternalID(%s,%s) failed: %v", providerName, externalID, err)
		return ""
	}
	if acct == nil {
		// Truly orphan deployment — preserve pre-fix nil tagging.
		return ""
	}
	return acct.ID
}

// collectAzureAmbient collects Azure recommendations using the host's managed
// identity (DefaultAzureCredential / ambient credentials). Used by the ambient
// fallback path in collectAzureRecommendations when no accounts are registered.
// subscriptionID is passed explicitly to avoid an unnecessary Azure API round-trip
// to auto-discover subscriptions — the caller already resolved it from env.
func (s *Scheduler) collectAzureAmbient(ctx context.Context, subscriptionID string) ([]config.RecommendationRecord, *sweepGap, error) {
	prov, err := s.providerFactory.CreateAndValidateProvider(ctx, "azure", &provider.ProviderConfig{
		AzureSubscriptionID: subscriptionID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create ambient Azure provider: %w", err)
	}
	recClient, err := prov.GetRecommendationsClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get Azure recommendations client: %w", err)
	}
	recs, err := recClient.GetAllRecommendations(ctx)
	gap, err := tolerateIncompleteSweep("azure", err)
	if err != nil {
		return nil, nil, fmt.Errorf("get Azure recommendations: %w", err)
	}
	return s.convertRecommendations(recs, "azure"), gap, nil
}

// collectGCPAmbient collects GCP recommendations using Application Default
// Credentials. Used by the ambient fallback path in collectGCPRecommendations
// when no accounts are registered.
func (s *Scheduler) collectGCPAmbient(ctx context.Context) ([]config.RecommendationRecord, *sweepGap, error) {
	prov, err := s.providerFactory.CreateAndValidateProvider(ctx, "gcp", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create ambient GCP provider: %w", err)
	}
	recClient, err := prov.GetRecommendationsClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get GCP recommendations client: %w", err)
	}
	recs, err := recClient.GetAllRecommendations(ctx)
	gap, err := tolerateIncompleteSweep("gcp", err)
	if err != nil {
		return nil, nil, fmt.Errorf("get GCP recommendations: %w", err)
	}
	return s.convertRecommendations(recs, "gcp"), gap, nil
}

func (s *Scheduler) collectAWSForAccount(ctx context.Context, globalCfg *config.GlobalConfig, acct config.CloudAccount) ([]config.RecommendationRecord, *sweepGap, error) {
	// Self-account: role_arn mode with no role ARN means "use the CUDly
	// Lambda's own (ambient) credentials to access this account" -- see
	// credentials.resolveRoleARNProvider, which documents the identical
	// shape. Any OTHER auth mode with an empty AWSRoleARN -- including the
	// empty string org-discovery persists on newly-discovered member
	// accounts pending operator review (handler_accounts.go,
	// persistDiscoveredMembers) -- must NOT take this branch: it must fall
	// through to ResolveAWSCredentialProvider below so its switch fails
	// loud ("unsupported aws_auth_mode") instead of silently collecting the
	// HOST account's data and tagging it with this account's UUID
	// (issue #107). A role_arn account that is not the host fails the
	// host-identity check instead (issue #402).
	if acct.AWSAuthMode == "role_arn" && acct.AWSRoleARN == "" {
		if err := credentials.VerifyHostAccount(ctx, &acct, s.stsClient); err != nil {
			return nil, nil, err
		}
		prov, err := s.providerFactory.CreateAndValidateProvider(ctx, "aws", nil)
		if err != nil {
			return nil, nil, fmt.Errorf("create ambient provider: %w", err)
		}
		return s.fetchAndConvert(ctx, prov, "aws", &acct.ID, acct.ExternalID, globalCfg)
	}
	awsCreds, err := credentials.ResolveAWSCredentialProvider(ctx, &acct, s.credStore, s.assumeRoleSTS)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve credentials: %w", err)
	}
	cfg := &provider.ProviderConfig{Name: "aws", AWSCredentialsProvider: awsCreds}
	prov, err := s.providerFactory.CreateAndValidateProvider(ctx, "aws", cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create provider: %w", err)
	}
	return s.fetchAndConvert(ctx, prov, "aws", &acct.ID, acct.ExternalID, globalCfg)
}

// collectAzureRecommendations fans out across all enabled Azure accounts,
// resolving per-account federated credentials via the KMS signer.
// When no accounts are registered but AZURE_SUBSCRIPTION_ID is set (CUDly
// running natively on Azure with managed identity), falls back to ambient
// credentials and tags recommendations with the registered subscription's
// UUID if found in cloud_accounts.
func (s *Scheduler) collectAzureRecommendations(ctx context.Context, _ *config.GlobalConfig) ([]config.RecommendationRecord, []string, *sweepGap, error) {
	accounts, err := s.enabledAccounts(ctx, "azure")
	if err != nil {
		return nil, nil, nil, err
	}
	if len(accounts) == 0 {
		// Issue #662: mirror the AWS ambient-path tagging fix for Azure.
		// When the host subscription matches a registered cloud_accounts row,
		// tag recs with that account's UUID instead of returning nil.
		// Best-effort: env lookup or store errors must not fail the collection.
		subscriptionID := os.Getenv("AZURE_SUBSCRIPTION_ID")
		if subscriptionID == "" {
			logging.Info("No enabled Azure accounts — skipping Azure recommendations")
			return nil, nil, nil, nil
		}
		recs, gap, err := s.collectAzureAmbient(ctx, subscriptionID)
		if err != nil {
			return nil, nil, nil, err
		}
		if acctID := s.resolveAmbientAccountID(ctx, "azure", subscriptionID); acctID != "" {
			recs = s.tagAccount(recs, acctID)
			return recs, ambientEvictionRoster(gap == nil, acctID), gap.forAccount(), nil
		}
		return recs, ambientEvictionRoster(gap == nil, ""), gap.forAccount(), nil
	}

	recs, outcome := fanOutPerAccount(ctx, "Azure", accounts, s.collectAzureForAccount)
	if outcome.FailedCount == len(accounts) && len(accounts) > 0 {
		return nil, nil, nil, errAllAccountsFailed("Azure", outcome)
	}
	return recs, outcome.SucceededAccountIDs, outcome.Gap, nil
}

func (s *Scheduler) collectAzureForAccount(ctx context.Context, acct config.CloudAccount) ([]config.RecommendationRecord, *sweepGap, error) {
	// Every recommendation returned below is tagged with THIS account's UUID
	// (see tagAccount at the end of this function), so the provider must be
	// pinned to this account's subscription. An empty AzureSubscriptionID
	// leaves the provider unpinned, and an unpinned provider now fans out
	// across every subscription the credential can see -- which would file
	// other subscriptions' recommendations under this account and expose them
	// to anyone authorized for it. Fail loud instead; the row is misconfigured.
	if acct.AzureSubscriptionID == "" {
		return nil, nil, fmt.Errorf("cloud account %s has no azure_subscription_id configured", acct.ID)
	}

	azCred, err := credentials.ResolveAzureTokenCredentialWithOpts(ctx, &acct, s.credStore, credentials.AzureResolveOptions{
		Signer:    s.oidcSigner,
		IssuerURL: s.oidcIssuerURL,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("resolve credentials: %w", err)
	}
	azProv, err := azureprovider.NewAzureProvider(&provider.ProviderConfig{Profile: acct.AzureSubscriptionID})
	if err != nil {
		return nil, nil, fmt.Errorf("create provider: %w", err)
	}
	azProv.SetCredential(azCred)

	recClient, err := azProv.GetRecommendationsClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get recommendations client: %w", err)
	}
	recs, err := recClient.GetAllRecommendations(ctx)
	gap, err := tolerateIncompleteSweep("azure", err)
	if err != nil {
		return nil, nil, fmt.Errorf("get recommendations: %w", err)
	}
	return s.tagAccount(s.convertRecommendations(recs, "azure"), acct.ID), gap, nil
}

// collectGCPRecommendations fans out across all enabled GCP accounts,
// resolving per-account federated credentials via the KMS signer.
// When no accounts are registered but GCP_PROJECT_ID is set (CUDly
// running natively on GCP with ADC), falls back to ambient credentials
// and tags recommendations with the registered project's UUID if found
// in cloud_accounts.
func (s *Scheduler) collectGCPRecommendations(ctx context.Context, _ *config.GlobalConfig) ([]config.RecommendationRecord, []string, *sweepGap, error) {
	accounts, err := s.enabledAccounts(ctx, "gcp")
	if err != nil {
		return nil, nil, nil, err
	}
	if len(accounts) == 0 {
		// Issue #662: mirror the AWS ambient-path tagging fix for GCP.
		// When the host project matches a registered cloud_accounts row,
		// tag recs with that account's UUID instead of returning nil.
		// Best-effort: env lookup or store errors must not fail the collection.
		projectID := os.Getenv("GCP_PROJECT_ID")
		if projectID == "" {
			logging.Info("No enabled GCP accounts — skipping GCP recommendations")
			return nil, nil, nil, nil
		}
		recs, gap, err := s.collectGCPAmbient(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		if acctID := s.resolveAmbientAccountID(ctx, "gcp", projectID); acctID != "" {
			recs = s.tagAccount(recs, acctID)
			return recs, ambientEvictionRoster(gap == nil, acctID), gap.forAccount(), nil
		}
		return recs, ambientEvictionRoster(gap == nil, ""), gap.forAccount(), nil
	}

	recs, outcome := fanOutPerAccount(ctx, "GCP", accounts, s.collectGCPForAccount)
	if outcome.FailedCount == len(accounts) && len(accounts) > 0 {
		return nil, nil, nil, errAllAccountsFailed("GCP", outcome)
	}
	return recs, outcome.SucceededAccountIDs, outcome.Gap, nil
}

func (s *Scheduler) collectGCPForAccount(ctx context.Context, acct config.CloudAccount) ([]config.RecommendationRecord, *sweepGap, error) {
	gcpTS, err := credentials.ResolveGCPTokenSourceWithOpts(ctx, &acct, s.credStore, credentials.GCPResolveOptions{
		Signer:    s.oidcSigner,
		IssuerURL: s.oidcIssuerURL,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("resolve credentials: %w", err)
	}

	var prov provider.Provider
	if gcpTS != nil {
		prov = gcpprovider.NewProviderWithCredentials(ctx, acct.GCPProjectID, gcpTS)
	} else {
		// ADC mode (application_default): use ambient credentials
		created, createErr := s.providerFactory.CreateAndValidateProvider(ctx, "gcp", nil)
		if createErr != nil {
			return nil, nil, fmt.Errorf("create ambient GCP provider: %w", createErr)
		}
		prov = created
	}

	recClient, err := prov.GetRecommendationsClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get recommendations client: %w", err)
	}
	recs, err := recClient.GetAllRecommendations(ctx)
	gap, err := tolerateIncompleteSweep("gcp", err)
	if err != nil {
		return nil, nil, fmt.Errorf("get recommendations: %w", err)
	}
	return s.tagAccount(s.convertRecommendations(recs, "gcp"), acct.ID), gap, nil
}

// enabledAccounts returns all enabled cloud accounts for the given provider.
// A listing error is returned, never mapped to "no accounts": callers treat an
// empty list as permission to fall back to ambient credentials, which must
// not happen because of a transient store failure.
func (s *Scheduler) enabledAccounts(ctx context.Context, providerName string) ([]config.CloudAccount, error) {
	enabled := true
	accounts, err := s.config.ListCloudAccounts(ctx, config.CloudAccountFilter{
		Provider: &providerName,
		Enabled:  &enabled,
	})
	if err != nil {
		return nil, fmt.Errorf("list enabled %s accounts: %w", providerName, err)
	}
	return accounts, nil
}

// tolerateIncompleteSweep converts an incomplete recommendation collection
// into a warning and a nil error, so the caller keeps the recommendations that
// WERE collected instead of discarding them.
//
// Every recommendation-fetch site in this file has the shape
// `recs, err := ...; if err != nil { return nil, ... }`, which throws the
// results away. That is correct for a real error, but for a partial sweep it
// would turn one flaky subscription out of fifty into a total collection
// outage -- strictly worse than the silent under-collection that
// PartialSubscriptionFailureError exists to prevent. Routing every site
// through this helper keeps the policy in one place rather than relying on
// each call site to remember it.
//
// The failed subscription IDs are named in the log so an operator can tell an
// under-collected sweep from a genuinely shrinking savings opportunity. Any
// other error is returned unchanged, preserving the existing fail-loud
// behavior.
//
// The gap return value is what stops a tolerated sweep from being mistaken
// for a whole one. It is nil for a complete sweep (and for a hard error, which
// callers handle via err first); non-nil only for a partial sweep, and callers
// must keep such an account OUT of the succeeded-account roster: that roster
// drives UpsertRecommendations' stale-row eviction, which deletes with
//
//	DELETE FROM recommendations
//	 WHERE collected_at < $1 AND (provider, account_key) IN (…)
//
// keyed on the CUDly account UUID, while a sweep fails one SUBSCRIPTION at a
// time. Reporting a partial sweep as succeeded therefore deletes the
// previous-cycle rows of every subscription the sweep never queried, and
// their savings silently read as "nothing to buy" rather than "collection
// incomplete" (issue #1652). That is data loss, not a display gap.
//
// Withholding eviction is deliberately all this does. The rows that WERE
// collected are still upserted by natural key, and the account is not marked
// failed -- failing it would blank the whole account's dashboard over one
// flaky subscription, the opposite failure and the one the succeeded-account
// roster exists to prevent.
//
// The gap is also how the incompleteness leaves the logs (issue #87): it is
// threaded up through fanOutPerAccount and collectAllProviders into
// recommendations_state.last_collection_partial, which the freshness API
// serves, so the dashboard can tell 1-of-10 coverage from full coverage. It
// is sanitized (counts and reason categories); the failed subscription IDs
// and raw errors stay in this WARN log only. See sweepGap.
func tolerateIncompleteSweep(providerName string, err error) (*sweepGap, error) {
	var incomplete *awsrecommendations.IncompleteRecommendationsError
	if errors.As(err, &incomplete) {
		logging.Warnf("%s recommendations incomplete: failed_details=%d failed_scopes=%d; %v; keeping collected rows and withholding stale-row eviction",
			providerName, incomplete.FailedDetails, incomplete.FailedScopes, incomplete)
		return gapFromAWSIncomplete(incomplete), nil
	}
	partial := azureprovider.AsPartialSubscriptionFailure(err)
	if partial == nil {
		return nil, err
	}
	logging.Warnf(
		"%s recommendations incomplete: %d of %d subscriptions succeeded; not queried: %s (keeping the %d that did, "+
			"and holding back stale-row eviction so the un-queried subscriptions' previous rows survive)",
		providerName, partial.Succeeded, partial.Attempted,
		strings.Join(partial.FailedSubscriptionIDs(), ", "), partial.Succeeded)
	return gapFromPartialSubscriptions(partial), nil
}

// fetchAndConvert is a convenience for the AWS ambient path.
//
// The *sweepGap is nil when the sweep covered everything it was asked to; see
// tolerateIncompleteSweep for why an incomplete one must not authorize
// stale-row eviction.
func (s *Scheduler) fetchAndConvert(ctx context.Context, prov provider.Provider, providerName string, accountID *string, expectedAccount string, globalCfg *config.GlobalConfig) ([]config.RecommendationRecord, *sweepGap, error) {
	recClient, err := prov.GetRecommendationsClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get %s recommendations client: %w", providerName, err)
	}
	recs, err := recClient.GetAllRecommendations(ctx)
	gap, err := tolerateIncompleteSweep(providerName, err)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get %s recommendations: %w", providerName, err)
	}
	recs = dropForeignZonalEC2(recs, expectedAccount)
	if len(recs) == 0 && globalCfg != nil {
		lookbackDays := globalCfg.RecommendationsLookbackDays
		if lookbackDays == 0 {
			lookbackDays = config.DefaultRecommendationsLookbackDays
		}
		payment := awsRecommendationPayment(globalCfg.DefaultPayment)
		for _, service := range []common.ServiceType{common.ServiceEC2, common.ServiceRDS, common.ServiceElastiCache, common.ServiceOpenSearch, common.ServiceRedshift, common.ServiceSavingsPlansAll} {
			params := common.RecommendationParams{
				Service:        service,
				Term:           fmt.Sprintf("%dyr", globalCfg.DefaultTerm),
				PaymentOption:  payment,
				LookbackPeriod: fmt.Sprintf("%dd", lookbackDays),
			}
			fallbackRecs, recErr := recClient.GetRecommendations(ctx, &params)
			fallbackGap, recErr := tolerateIncompleteSweep(providerName, recErr)
			if recErr != nil {
				return nil, nil, fmt.Errorf("failed to get %s recommendations with default term/payment fallback (service=%s, term=%s, payment=%s, lookback=%s): %w",
					providerName, params.Service, params.Term, params.PaymentOption, params.LookbackPeriod, recErr)
			}
			recs = append(recs, dropForeignZonalEC2(fallbackRecs, expectedAccount)...)
			gap = mergeSweepGaps(gap, fallbackGap)
		}
	}
	result := s.convertRecommendations(recs, providerName)
	if accountID != nil {
		result = s.tagAccount(result, *accountID)
	}
	return result, gap, nil
}

// tagAccount assigns the stored account identity and updates its recommendation ID.
// A rec whose ID cannot be computed is dropped loudly rather than emitted
// without its platform/tenancy pair (issue #806).
func (s *Scheduler) tagAccount(recs []config.RecommendationRecord, accountID string) []config.RecommendationRecord {
	kept := recs[:0]
	for i := range recs {
		recs[i].CloudAccountID = &accountID
		id, err := recommendationID(recs[i])
		if err != nil {
			logging.Errorf("tagAccount: dropping rec (account=%s service=%s sku=%s region=%s): %v",
				accountID, recs[i].Service, recs[i].ResourceType, recs[i].Region, err)
			continue
		}
		recs[i].ID = id
		kept = append(kept, recs[i])
	}
	return kept
}

// recommendationID uses the same identity dimensions as the stored natural key.
func recommendationID(rec config.RecommendationRecord) (string, error) {
	ci, err := rec.ComputeIdentity()
	if err != nil {
		return "", err
	}
	accountID := ""
	if rec.CloudAccountID != nil {
		accountID = *rec.CloudAccountID
	}
	id := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d|%s",
		rec.Provider, accountID, rec.Service, rec.Region,
		rec.ResourceType, rec.Engine, rec.Term, rec.Payment)
	if rec.AvailabilityZone != "" {
		// Appended only for zonal EC2 recs so every other rec keeps its
		// existing ID (issue #792).
		id += "|" + rec.AvailabilityZone
	}
	if !ci.IsZero() {
		// The platform/tenancy pair is appended together, only for recs whose
		// Details carry them, so every other rec keeps its ID (issue #806).
		// AWS platform is the raw CE string, so the ID can contain "/" and
		// spaces.
		id += "|" + ci.Platform + "|" + ci.Tenancy
	}
	return id, nil
}

// ListRecommendations reads cached recommendations from the store, applying
// the given filter as SQL WHERE clauses. Previously this did live cloud
// API calls on every request (2–10s per provider); now it's a pure DB
// read, typically under 100ms.
//
// Order of operations:
//  1. Read freshness.
//  2. Cold-start (LastCollectedAt==nil): single-flight a background collect
//     via kickColdStartCollection rather than blocking this read on it (issue
//     #106) -- the frontend already drives a coordinated first collection on
//     page load (triggerAutoRefreshIfStale -> POST /recommendations/refresh),
//     so this is a best-effort kick for non-browser API callers only.
//  3. Read from the cache (empty on a genuine cold start, until the kicked
//     collection or the frontend's refresh populates it).
//  4. Stale-while-revalidate: if the cache is older than cacheTTL AND we
//     aren't on Lambda, kick off a background CollectRecommendations so
//     the NEXT read sees fresh data. Lambda skips this (goroutines freeze
//     between invocations); the scheduled cron is Lambda's refresh path.
//
// Steps 2 and 4 run only when ctx carries WithBackgroundCollection, which the
// API sets for callers with unrestricted account scope. Both collect across
// every account, so a scoped reader gets a pure cache read (issue #627).
func (s *Scheduler) ListRecommendations(ctx context.Context, filter config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	logging.Info("Reading recommendations from cache...")

	freshness, err := s.config.GetRecommendationsFreshness(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read recommendations freshness: %w", err)
	}

	mayCollect := BackgroundCollectionAllowed(ctx)
	if mayCollect && freshness.LastCollectedAt == nil {
		s.kickColdStartCollection(ctx)
	}

	recs, err := s.config.ListStoredRecommendations(ctx, filter)
	if err != nil {
		return nil, err
	}

	recs, err = s.applySuppressions(ctx, recs)
	if err != nil {
		// A suppression read failure shouldn't block the recs list —
		// the user-facing cost of that is an empty page; we'd rather
		// over-show than under-show. Log and continue.
		logging.Errorf("failed to apply suppressions; returning un-suppressed recs: %v", err)
	}

	recs, err = s.applyAccountOverrides(ctx, recs)
	if err != nil {
		// Same over-show-vs-under-show trade-off as applySuppressions.
		// A missing global ServiceConfig or override-table read failure
		// should leave the user looking at un-overridden recs, not a
		// blank page. Issue #196.
		logging.Errorf("failed to apply account overrides; returning un-filtered recs: %v", err)
	}

	// Background refresh is only attempted on non-Lambda runtimes; skip
	// the DB config read entirely on Lambda so GetGlobalConfig is not called
	// on the hot read path (Lambda has no persistent goroutines).
	if mayCollect && !s.isLambda {
		ttl, disabled := s.resolveEffectiveCacheTTL(ctx)
		if disabled {
			return recs, nil
		}
		s.maybeKickBackgroundRefresh(freshness, ttl)
	}
	return recs, nil
}

// ListRecommendationsStrict is ListRecommendations for the money path: the
// same stored rows, suppressions and account overrides, but a suppression or
// override read failure is returned instead of logged. Over-showing is fine
// for a page; on a purchase it would re-buy what a prior step already bought.
// It has no cold-start or background-refresh side effects: an empty cache
// reads as empty and the caller retries on a later tick.
func (s *Scheduler) ListRecommendationsStrict(ctx context.Context, filter config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	recs, err := s.config.ListStoredRecommendations(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("list stored recommendations: %w", err)
	}
	recs, err = s.applySuppressions(ctx, recs)
	if err != nil {
		return nil, fmt.Errorf("apply suppressions: %w", err)
	}
	recs, err = s.applyAccountOverrides(ctx, recs)
	if err != nil {
		return nil, fmt.Errorf("apply account overrides: %w", err)
	}
	return recs, nil
}

// GetRecommendationByID fetches a single recommendation by its application-
// level ID (the id field in the stored payload), bypassing the account-override
// filter so deep-linked URLs to override-hidden recs still resolve.
//
// Suppressions are still applied: a rec whose remaining count has been
// suppressed to zero is treated as gone (returns nil, nil, nil) so the caller
// can render a 404 rather than a "hidden" banner for actively-dismissed recs.
//
// hiddenBy is non-nil (one or more reason strings) when the rec exists and
// passes the suppression check, but would be dropped by the account-override
// filter. Possible reasons: "enabled=false", "engine", "region",
// "resource_type", "min_count" (overrideDimensions). The caller renders a
// "hidden by your override" banner.
//
// Returns (nil, nil, nil) when the rec is genuinely absent or fully suppressed.
func (s *Scheduler) GetRecommendationByID(ctx context.Context, id string) (rec *config.RecommendationRecord, hiddenBy []string, err error) {
	recs, err := s.config.ListStoredRecommendations(ctx, config.RecommendationFilter{ID: id})
	if err != nil {
		return nil, nil, fmt.Errorf("GetRecommendationByID: store lookup: %w", err)
	}
	if len(recs) == 0 {
		return nil, nil, nil
	}

	// Apply suppressions. A fully-suppressed rec becomes genuinely absent from
	// the caller's perspective (the suppression is intentional user action).
	recs, err = s.applySuppressions(ctx, recs)
	if err != nil {
		// Over-show: on suppression read failure treat the rec as un-suppressed.
		logging.Errorf("GetRecommendationByID: suppression check failed; treating rec as un-suppressed: %v", err)
	}
	if len(recs) == 0 {
		return nil, nil, nil
	}
	found := &recs[0]

	// Check whether the account-override filter would drop this rec. This is
	// a read-only call — we never drop it here, only report the reasons.
	resolved, resolveErr := config.ResolveAccountConfigsForRecs(ctx, s.config, recs)
	if resolveErr != nil {
		// Non-fatal: if the override check fails we surface the rec without
		// a hidden_by marker (over-show is the safer default).
		logging.Errorf("GetRecommendationByID: override resolution failed; returning rec without hidden_by: %v", resolveErr)
		return found, nil, nil
	}
	accountID := ""
	if found.CloudAccountID != nil {
		accountID = *found.CloudAccountID
	}
	cfg := resolved[config.AccountConfigKey(accountID, found.Provider, found.Service)]
	if cfg != nil {
		hiddenBy = overrideHiddenReasons(found, cfg)
	}

	return found, hiddenBy, nil
}

// resolveEffectiveCacheTTL returns the effective stale-while-revalidate TTL
// and whether background auto-refresh has been explicitly disabled (value 0).
// It prefers the DB-configured RecommendationsCacheStaleHours; falls back to
// s.cacheTTL (env-var / compile-time default) when the DB read fails.
func (s *Scheduler) resolveEffectiveCacheTTL(ctx context.Context) (ttl time.Duration, disabled bool) {
	ttl = s.cacheTTL
	globalCfg, gcErr := s.config.GetGlobalConfig(ctx)
	if gcErr != nil || globalCfg == nil {
		// DB read failed OR returned (nil, nil) — fall back to the env-var /
		// default TTL with refresh enabled rather than panicking on the
		// nil deref below. (nil, nil) is a defensive case; the Postgres
		// store currently never returns it, but a future store impl or a
		// test mock might.
		return ttl, false
	}
	if globalCfg.RecommendationsCacheStaleHours == 0 {
		// Explicit 0 = operator opted out of automatic background refresh.
		return 0, true
	}
	return time.Duration(globalCfg.RecommendationsCacheStaleHours) * time.Hour, false
}

// suppressionKey is the 6-tuple used to group suppressions. Within a group,
// each row's variant (availability zone, platform, tenancy; issue #816)
// decides whether it applies to a given rec (PurchaseSuppression.AppliesTo).
type suppressionKey struct {
	accountID, provider, service, region, resourceType, engine string
}

// suppressionAgg aggregates the active suppression rows that apply to one
// rec: cumulative count, earliest expiry (drives the badge's
// "Xd remaining"), and the execution whose suppression contributed
// the most (drives the badge deep-link).
type suppressionAgg struct {
	earliestExpiresAt       time.Time
	primaryExecutionCreated time.Time
	primaryExecutionID      string
	suppressedCount         int
	primaryExecutionContrib int
}

// add folds one suppression row into the aggregate.
func (a *suppressionAgg) add(sup *config.PurchaseSuppression) {
	a.suppressedCount += sup.SuppressedCount
	if sup.ExpiresAt.Before(a.earliestExpiresAt) {
		a.earliestExpiresAt = sup.ExpiresAt
	}
	// "Most contribution" selects the biggest single suppression
	// row; ties go to the most-recently-created row so a fresh
	// cancel of an older peer doesn't leave the badge pointing at
	// a stale execution.
	if sup.SuppressedCount > a.primaryExecutionContrib ||
		(sup.SuppressedCount == a.primaryExecutionContrib && sup.CreatedAt.After(a.primaryExecutionCreated)) {
		a.primaryExecutionID = sup.ExecutionID
		a.primaryExecutionCreated = sup.CreatedAt
		a.primaryExecutionContrib = sup.SuppressedCount
	}
}

// applySuppressions subtracts active purchase_suppressions from each
// rec's count and annotates the rec with the SuppressedCount /
// SuppressionExpiresAt / PrimarySuppressionExecutionID fields that
// drive the "recently purchased" badge on the frontend. Recs whose
// remaining count hits 0 are dropped entirely.
func (s *Scheduler) applySuppressions(ctx context.Context, recs []config.RecommendationRecord) ([]config.RecommendationRecord, error) {
	sups, err := s.config.ListActiveSuppressions(ctx)
	if err != nil {
		return recs, err
	}
	if len(sups) == 0 {
		return recs, nil
	}
	index := aggregateSuppressions(sups)
	return applySuppressionIndex(recs, index), nil
}

// aggregateSuppressions groups a raw suppression list by 6-tuple. The
// per-rec aggregate is built in applySuppressionIndex, because which rows
// of a group apply depends on the rec's variant.
func aggregateSuppressions(sups []config.PurchaseSuppression) map[suppressionKey][]config.PurchaseSuppression {
	index := make(map[suppressionKey][]config.PurchaseSuppression, len(sups))
	for i := range sups {
		sup := sups[i]
		k := suppressionKey{
			accountID: sup.AccountID, provider: sup.Provider, service: sup.Service,
			region: sup.Region, resourceType: sup.ResourceType, engine: sup.Engine,
		}
		index[k] = append(index[k], sup)
	}
	return index
}

// matchingSuppressions aggregates the rows of rec's 6-tuple group that apply
// to rec's variant: rows for the same availability zone, platform and
// tenancy, plus legacy rows with no variant (a wildcard over the 6-tuple).
// A rec whose Details cannot be read matches every row of its group, the
// pre-#816 behavior. Returns nil when no row applies.
func matchingSuppressions(rec *config.RecommendationRecord, group []config.PurchaseSuppression) *suppressionAgg {
	var recVariant *config.SuppressionVariant
	if v, err := rec.SuppressionVariant(); err == nil {
		recVariant = &v
	}
	var a *suppressionAgg
	for i := range group {
		if !group[i].AppliesTo(recVariant) {
			continue
		}
		if a == nil {
			a = &suppressionAgg{earliestExpiresAt: group[i].ExpiresAt}
		}
		a.add(&group[i])
	}
	return a
}

// applySuppressionIndex walks recs, subtracts the count of the
// suppressions that apply to each rec, drops recs that go to 0 or below,
// and annotates the survivors with the badge fields.
func applySuppressionIndex(recs []config.RecommendationRecord, index map[suppressionKey][]config.PurchaseSuppression) []config.RecommendationRecord {
	// Allocate a fresh backing array so callers that hold a reference to
	// the original recs slice do not see mutations (05-M1).
	out := make([]config.RecommendationRecord, 0, len(recs))
	for _rvc := range recs {
		rec := recs[_rvc]
		accountID := ""
		if rec.CloudAccountID != nil {
			accountID = *rec.CloudAccountID
		}
		k := suppressionKey{
			accountID: accountID, provider: rec.Provider, service: rec.Service,
			region: rec.Region, resourceType: rec.ResourceType, engine: rec.Engine,
		}
		a := matchingSuppressions(&rec, index[k])
		if a == nil {
			out = append(out, rec)
			continue
		}
		rec.Count -= a.suppressedCount
		if rec.Count <= 0 {
			continue // fully covered by recent purchases; hide
		}
		rec.SuppressedCount = a.suppressedCount
		expiry := a.earliestExpiresAt
		rec.SuppressionExpiresAt = &expiry
		primary := a.primaryExecutionID
		rec.PrimarySuppressionExecutionID = &primary
		out = append(out, rec)
	}
	return out
}

// kickColdStartCollection single-flights the FIRST-EVER collection
// (freshness.LastCollectedAt == nil) cluster-wide via MarkCollectionStarted
// -- the same marker the explicit refresh endpoint coordinates through --
// instead of every concurrent cold reader racing its
// own uncoordinated CollectRecommendations call on its own short request
// context (issue #106). The previous synchronous call ran on ctx, which the
// HTTP layer caps at 30s (internal/server/http.go); a real sweep routinely
// takes longer, so collectAllProviders returned ctx.Err() before
// persistCollection ever ran, last_collected_at never advanced, and every
// subsequent reader repeated the same doomed collect -- a self-sustaining
// storm hammering Cost Explorer with duplicated fan-outs.
//
// The frontend already drives a properly-coordinated first collection on
// page load (triggerAutoRefreshIfStale -> POST /api/recommendations/refresh,
// which uses this same marker and, on Lambda, a real async self-invoke
// dispatched through HandleScheduledTask's advisory lock). This function is
// a best-effort kick for non-browser API callers that read the cache
// directly without ever hitting that endpoint.
//
// On non-Lambda runtimes the winning caller runs the collection on a
// detached background context sized for a real sweep, mirroring
// maybeKickBackgroundRefresh's stale-cache refresh below, gated by the same
// per-process s.collecting guard so the two background paths can never
// double-run concurrently. On Lambda a goroutine started inside a request
// handler freezes the instant this invocation's response is sent (see
// maybeKickBackgroundRefresh), so there is no way to make progress from
// inside this invocation; this function returns immediately WITHOUT ever
// calling MarkCollectionStarted, rather than winning the marker only to
// release it straight back -- that mark-then-clear round trip does no work
// and briefly opens a false-409 window on the refresh endpoint for nothing.
// The cache is populated by whichever runs first: the scheduled cron
// (already advisory-locked) or the frontend's auto-refresh-on-open flow.
//
// The cron never calls MarkCollectionStarted (it single-flights through the
// advisory lock instead), so a kick here can overlap at most one in-flight
// cron collect. That overlap is bounded: the marker still limits cold
// readers to one kick cluster-wide, and s.collecting to one per process.
func (s *Scheduler) kickColdStartCollection(ctx context.Context) {
	if s.isLambda {
		return
	}
	token, ok, err := s.config.MarkCollectionStarted(ctx)
	if err != nil {
		logging.Warnf("cold-start collection: failed to mark started: %v", err)
		return
	}
	if !ok {
		// Another cold reader or an explicit refresh already has a
		// collection in flight.
		return
	}
	if !s.collecting.CompareAndSwap(false, true) {
		// A stale-cache background refresh already claimed the per-process
		// slot; release the DB marker so the refresh endpoint (or the next
		// reader) is not blocked by a marker nothing is actively working.
		s.clearCollectionStartedBestEffort(ctx, token)
		return
	}

	logging.Info("Recommendations cache is empty; triggering single-flighted background cold-start collect")
	bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	go func() {
		defer cancel()
		defer s.collecting.Store(false)
		defer func() {
			if r := recover(); r != nil {
				logging.Errorf("cold-start collection panic: %v", r)
			}
		}()
		if _, err := s.CollectRecommendations(bgCtx, token); err != nil {
			// CollectRecommendations already surfaces errors via
			// recommendations_state.last_collection_error, so just log
			// locally here for operator visibility.
			logging.Errorf("cold-start collection: %v", err)
		}
	}()
}

// maybeKickBackgroundRefresh spawns a detached CollectRecommendations
// goroutine when the cache is stale AND the runtime can safely run
// background goroutines (i.e. not Lambda). The atomic.Bool guard
// single-flights concurrent callers so only one refresh fires per TTL
// window. Recovers from panics so one bad collect can't crash the
// long-lived process.
//
// effectiveTTL is the caller-resolved stale threshold (from DB config
// RecommendationsCacheStaleHours, or the env-var default if unconfigured).
func (s *Scheduler) maybeKickBackgroundRefresh(freshness *config.RecommendationsFreshness, effectiveTTL time.Duration) {
	if s.isLambda {
		return
	}
	if freshness.LastCollectedAt == nil {
		// kickColdStartCollection already handles this case.
		return
	}
	if time.Since(*freshness.LastCollectedAt) < effectiveTTL {
		return
	}
	if !s.collecting.CompareAndSwap(false, true) {
		// Another refresh is already in flight.
		return
	}

	logging.Infof("Recommendations cache is stale (age > %s); triggering background refresh", effectiveTTL)
	bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	go func() {
		defer cancel()
		defer s.collecting.Store(false)
		defer func() {
			if r := recover(); r != nil {
				logging.Errorf("background recommendations refresh panic: %v", r)
			}
		}()
		if _, err := s.CollectRecommendations(bgCtx, ""); err != nil {
			// CollectRecommendations already surfaces errors via
			// recommendations_state.last_collection_error, so just log
			// locally here for operator visibility.
			logging.Errorf("background refresh: %v", err)
		}
	}()
}

// nonZeroPtr returns &v when v > 0, else nil. Used by convertRecommendations
// to pass the provider's on-demand baseline (rec.OnDemandCost) through as a
// nullable pointer — a literal 0 from the provider means "not populated"
// because every real on-demand baseline is non-zero. Always returning a
// pointer would poison the frontend's "is this populated?" branch
// (recommendations.ts::effectiveSavingsPct, see #274). Extracted to keep
// convertRecommendations under the gocyclo gate (.golangci.yml
// min-complexity: 15; pre-commit hook flags > 10).
func nonZeroPtr(v float64) *float64 {
	if v > 0 {
		return &v
	}
	return nil
}

// extractEngine pulls the engine string out of a polymorphic
// common.ServiceDetails value when one is present. DatabaseDetails/
// CacheDetails are always pointers (every producer constructs them that
// way; see pkg/common/service_details_codec.go's package doc for the
// invariant). Returns "" for any other Details type (or nil). The
// nil-pointer guards are not dead code: the `details == nil` check above
// only catches an untyped nil, so a typed nil reaches the switch and would
// panic on the field read. Extracted from convertRecommendations to keep
// that function under the gocyclo budget (min-complexity: 10 in
// .pre-commit-config.yaml).
func extractEngine(details common.ServiceDetails) string {
	if details == nil {
		return ""
	}
	switch d := details.(type) {
	case *common.DatabaseDetails:
		if d == nil {
			return ""
		}
		return d.Engine
	case *common.CacheDetails:
		if d == nil {
			return ""
		}
		return d.Engine
	}
	return ""
}

// marshalRecDetails encodes the full ServiceDetails payload into a raw
// JSON blob so the purchase manager can reconstruct the correct typed
// *Details pointer later (issue #453). Without this, every persisted
// rec round-trips through the DB as "service + engine" only — losing
// EC2 platform / tenancy / scope, RDS AZ-config, SP plan-type / hourly
// commitment, etc. — and findOfferingID either picks the wrong offering
// (Windows recs purchased as Linux/UNIX) or fails the type-assertion
// outright. Marshal errors are non-fatal: the row still gets persisted
// without Details and falls through to the graceful-degradation path in
// common.DecodeServiceDetailsFor. Extracted from convertRecommendations
// to keep that function under the gocyclo budget.
func marshalRecDetails(rec common.Recommendation, providerName string) []byte {
	blob, err := common.MarshalServiceDetails(rec.Details)
	if err != nil {
		logging.Warnf("Failed to marshal service details for %s/%s rec (%s): %v — persisting without Details",
			providerName, rec.Service, rec.ResourceType, err)
		return nil
	}
	return blob
}

// convertRecommendations converts common.Recommendation slice to config.RecommendationRecord slice.
func (s *Scheduler) convertRecommendations(recs []common.Recommendation, providerName string) []config.RecommendationRecord {
	records := make([]config.RecommendationRecord, 0, len(recs))

	for _rvc := range recs {
		rec := recs[_rvc]
		engine := extractEngine(rec.Details)
		detailsBlob := marshalRecDetails(rec, providerName)

		// Canonicalize PaymentOption at the emission boundary so a downstream
		// plan-validator round-trip never sees a cross-provider/AWS-style
		// token on a non-AWS rec (issue #698). The provider service clients
		// historically aliased "all-upfront"/"no-upfront" to the canonical
		// "upfront"/"monthly" inside their pricing switches; the validator
		// no longer accepts those aliases. Normalize once here so persisted
		// recs always carry the provider-canonical token.
		if canon, ok := config.NormalizePaymentOption(providerName, rec.PaymentOption); ok {
			if canon != rec.PaymentOption {
				logging.Warnf("convertRecommendations: coerced %s payment_option %q to canonical %q (account=%s service=%s sku=%s)",
					providerName, rec.PaymentOption, canon, rec.Account, rec.Service, rec.ResourceType)
				rec.PaymentOption = canon
			}
		} else if rec.PaymentOption != "" {
			// Unknown provider or unmapped token: leave as-is. The next
			// validator boundary will surface the issue with a clear error.
			logging.Warnf("convertRecommendations: cannot canonicalize %s payment_option %q (account=%s service=%s sku=%s) — leaving as-is",
				providerName, rec.PaymentOption, rec.Account, rec.Service, rec.ResourceType)
		}

		// Parse term to integer (e.g., "3yr" -> 3). An empty or unparseable
		// term must NOT be coerced to a valid default: rec.Term round-trips
		// through this int on the purchase path (internal/purchase/execution.go
		// formats it back as "%dyr"), so silently defaulting here would let a
		// malformed term launder into a valid, purchasable commitment length
		// downstream, bypassing the ARCH-04 whitelist checks in the provider
		// clients entirely, since "3yr" is itself a whitelisted value (issue
		// #1207 follow-up). Drop the recommendation instead of persisting a
		// fabricated term.
		termStr := strings.TrimSuffix(rec.Term, "yr")
		term, err := strconv.Atoi(termStr)
		if err != nil || term <= 0 {
			logging.Errorf("convertRecommendations: dropping %s rec (account=%s service=%s sku=%s region=%s): invalid term %q",
				providerName, rec.Account, rec.Service, rec.ResourceType, rec.Region, rec.Term)
			continue
		}

		var sourceAccountID *string
		if rec.Account != "" {
			sourceAccountID = new(rec.Account)
		}
		record := config.RecommendationRecord{
			SourceAccountID:   sourceAccountID,
			Provider:          providerName,
			Service:           string(rec.Service),
			Region:            rec.Region,
			ResourceType:      rec.ResourceType,
			Engine:            engine,
			Details:           detailsBlob, // full ServiceDetails payload (issue #453)
			AvailabilityZone:  zonalAvailabilityZone(rec.Details),
			Count:             rec.Count,
			Term:              term,
			Payment:           rec.PaymentOption,
			UpfrontCost:       rec.CommitmentCost,
			MonthlyCost:       rec.RecurringMonthlyCost, // nil when provider API didn't return a monthly breakdown
			Savings:           rec.EstimatedSavings,
			OnDemandCost:      nonZeroPtr(rec.OnDemandCost),      // nil when provider API didn't return a baseline; frontend falls back to reconstruction (#274)
			SavingsPercentage: nonZeroPtr(rec.SavingsPercentage), // provider-authoritative %; nil when not reported, frontend falls back to effectiveSavingsPct
			UsageHistory:      rec.UsageHistory,                  // daily coverage pcts (nil when provider not yet wired; see #239)
			Selected:          true,                              // Default to selected
			Purchased:         false,
		}
		id, err := recommendationID(record)
		if err != nil {
			logging.Errorf("convertRecommendations: dropping %s rec (account=%s service=%s sku=%s region=%s): %v",
				providerName, rec.Account, rec.Service, rec.ResourceType, rec.Region, err)
			continue
		}
		record.ID = id
		records = append(records, record)
	}

	return records
}
