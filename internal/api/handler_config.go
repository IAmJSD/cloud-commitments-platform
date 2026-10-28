// Package api provides the HTTP API handlers for the CUDly dashboard.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/commitmentopts"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

// sourceCloud returns the cloud where CUDly is currently running, read from
// the CUDLY_SOURCE_CLOUD env var (set by Terraform). Falls back to "aws".
func sourceCloud() string {
	if v := os.Getenv("CUDLY_SOURCE_CLOUD"); v != "" {
		return v
	}
	return "aws"
}

// Configuration handlers.
func (h *Handler) getConfig(ctx context.Context, req *events.LambdaFunctionURLRequest) (*ConfigResponse, error) {
	// Require view:config permission. Every other read handler in the package
	// pairs the route-level AuthUser gate with this explicit permission check;
	// config GET must be consistent (02-M4).
	if _, err := h.requirePermission(ctx, req, "view", "config"); err != nil {
		return nil, err
	}

	globalCfg, err := h.config.GetGlobalConfig(ctx)
	if err != nil {
		return nil, err
	}

	services, err := h.config.ListServiceConfigs(ctx)
	if err != nil {
		return nil, err
	}

	resp := &ConfigResponse{
		Global:      globalCfg,
		Services:    services,
		SourceCloud: sourceCloud(),
	}

	// SourceIdentity contains the host cloud account ID (AWS account number,
	// Azure tenant ID, etc.). Only expose it to admin sessions so that
	// non-admin users cannot extract the cloud identity of the CUDly host
	// account (issue #407).
	if _, adminErr := h.requireAdmin(ctx, req); adminErr == nil {
		resp.SourceIdentity = h.resolveSourceIdentity(ctx)
	}

	return resp, nil
}

func (h *Handler) updateConfig(ctx context.Context, req *events.LambdaFunctionURLRequest) (*StatusResponse, error) {
	// Require update:config permission
	session, permErr := h.requirePermission(ctx, req, "update", "config")
	if permErr != nil {
		return nil, permErr
	}

	// Reject a malformed body before any DB work so a bad request fails fast
	// (and never opens a transaction). Keeps the nil-store fast-fail contract.
	if !json.Valid([]byte(req.Body)) {
		return nil, NewClientError(400, "invalid request body")
	}

	// Presence map of the top-level keys the caller actually sent, used to
	// replace (not merge) grace_period_days when present.
	var present map[string]json.RawMessage
	if err := json.Unmarshal([]byte(req.Body), &present); err != nil {
		return nil, NewClientError(400, "invalid request body")
	}
	_, gracePresent := present["grace_period_days"]
	modeSent := riExchangeModeSent(present)
	if err := h.requireGlobalConfigScope(ctx, session); err != nil {
		return nil, err
	}
	// Resolved before the transaction: the closure below runs while it holds a
	// pooled connection and must not need a second one (issue #1517).
	grants := h.resolveArmingGrants(ctx, req)

	// Serialized read-modify-write: the store loads the stored config and
	// applies this closure under an advisory-locked transaction, then upserts
	// the result in the same tx. json.Unmarshal only assigns keys present in the
	// body, so any field ABSENT is preserved (present -> updated, absent ->
	// preserved) for EVERY field, with no per-field enumeration. Doing the read
	// and write in one locked tx prevents two concurrent partial PUTs (e.g. the
	// laddering kill-switch toggle and a Settings save) from reading the same
	// stale base and losing each other's update. The closure returns a
	// ClientError(400) on a bad body or validation failure, which the store
	// propagates unchanged; DB/transport errors surface as 500. A changed
	// default reaches the service configs inside the same transaction.
	var switchBefore, ladderBefore bool
	merged, err := h.config.UpdateGlobalConfigAtomic(ctx, func(existing *config.GlobalConfig) error {
		// Snapshot before the wholesale unmarshal: this body writes onto the
		// entire GlobalConfig, so ri_exchange_mode / ri_exchange_enabled reach
		// the scheduler through this endpoint exactly as they do through the
		// dedicated PUT /api/ri-exchange/config (issue #1765). Taken inside the
		// closure so the comparison reads the same advisory-locked row the
		// write lands on.
		before := riExchangeArmStateOf(existing)
		switchBefore = existing.AutoPurchaseEnabled
		ladderBefore = existing.LadderExecutionEnabled

		// grace_period_days is a map: json.Unmarshal into a non-nil map MERGES
		// keys (an omitted key can never be deleted). When the caller sends the
		// key, nil the stored map first so the body's map REPLACES it wholesale
		// (present -> replace); when absent, leave it to preserve.
		if gracePresent {
			existing.GracePeriodDays = nil
		}
		if uErr := json.Unmarshal([]byte(req.Body), existing); uErr != nil {
			return NewClientError(400, "invalid request body")
		}
		if vErr := validateGlobalConfigWrite(existing, modeSent || existing.RIExchangeMode != before.mode); vErr != nil {
			return vErr
		}
		if gErr := requireRIExchangeAutoModeGrant(before, riExchangeArmStateOf(existing), grants.riExchange); gErr != nil {
			return gErr
		}
		return requireSwitchGrants(ctx, session, grants.purchases, switchBefore, ladderBefore, existing)
	})
	if err != nil {
		return nil, err
	}
	logAutoPurchaseSwitchChange(ctx, session, switchBefore, merged)
	logLadderExecutionSwitchChange(ctx, session, ladderBefore, merged)

	return &StatusResponse{Status: "updated"}, nil
}

// riExchangeModeSent reports whether the body writes ri_exchange_mode. A JSON
// null does not: json.Unmarshal leaves the stored value in place for it.
func riExchangeModeSent(present map[string]json.RawMessage) bool {
	for key, raw := range present {
		if strings.EqualFold(key, "ri_exchange_mode") && string(raw) != "null" {
			return true
		}
	}
	return false
}

// validateGlobalConfigWrite validates the merged config as a 400.
//
// ri_exchange_mode is checked when explicitly set or changed by JSON decoding
// (including duplicate keys followed by null), with
// the same manual|auto rule PUT /api/ri-exchange/config applies. Checking the
// merged value instead would refuse every partial save, including the
// laddering kill-switch toggle, on a row stored before this check with a mode
// such as "Auto". Such a row keeps its value until a caller sends a valid one,
// and armed() keeps treating it as auto. Runs before the arming gates so a
// malformed write never reveals grant state.
func validateGlobalConfigWrite(cfg *config.GlobalConfig, modeSent bool) error {
	if modeSent && !config.IsValidRIExchangeMode(cfg.RIExchangeMode) {
		return NewClientError(400, "ri_exchange_mode must be 'manual' or 'auto'")
	}
	if err := cfg.Validate(); err != nil {
		return NewClientError(400, fmt.Sprintf("validation error: %s", err))
	}
	return nil
}

// requireSwitchGrants chains the arming gates of the two money-spending
// switches: auto-purchase, then ladder execution. purchasesGrant is the
// pre-resolved execute:purchases outcome (armingGrants.purchases).
func requireSwitchGrants(ctx context.Context, session *Session, purchasesGrant error, autoBefore, ladderBefore bool, cfg *config.GlobalConfig) error {
	if err := requireAutoPurchaseSwitchGrant(ctx, session, purchasesGrant, autoBefore, cfg); err != nil {
		return err
	}
	return requireLadderExecutionSwitchGrant(ctx, session, purchasesGrant, ladderBefore, cfg)
}

func (h *Handler) requireGlobalConfigScope(ctx context.Context, session *Session) error {
	scope, err := h.getAccountScope(ctx, session)
	if err != nil {
		return err
	}
	if !scope.AllowsAll() {
		return NewClientError(403, "permission denied: global configuration requires unrestricted account access")
	}
	return h.requirePermissionConstraints(ctx, session, auth.ActionUpdate, auth.ResourceConfig,
		[]auth.PermissionConstraints{{StrictScope: true}})
}

// checkCommitmentOptionCombo rejects saves that carry a (term, payment)
// combination we've dynamically confirmed the cloud doesn't sell. Returns
// nil when: no probe service is wired, the service hasn't persisted data
// yet (absent data → fall through to the frontend's hardcoded rules),
// the save isn't AWS, or the combo is valid.
//
// Any other Validate error (a store read failing, a timeout) fails the save
// with a 503 instead of letting it through: the check exists for API
// callers that bypass the frontend's rules, so waving the save through on an
// error would persist an unsupported combo that fails only at purchase time
// (issue #243, audit finding A02-022). The caller can retry.
func (h *Handler) checkCommitmentOptionCombo(ctx context.Context, cfg config.ServiceConfig) error {
	if h.commitmentOpts == nil || cfg.Provider != "aws" || cfg.Term <= 0 || cfg.Payment == "" {
		return nil
	}
	ok, err := h.commitmentOpts.Validate(ctx, cfg.Provider, cfg.Service, cfg.Term, cfg.Payment)
	if errors.Is(err, commitmentopts.ErrNoData) {
		return nil
	}
	if err != nil {
		return commitmentComboUnverifiable(err)
	}
	if !ok {
		return commitmentComboUnsupported(cfg)
	}
	return nil
}

// commitmentComboUnverifiable is the 503 a save gets when the commitment
// options could not be read, so the combo could not be checked.
func commitmentComboUnverifiable(err error) error {
	logging.Errorf("commitment-option validation error (refusing save): %v", err)
	return NewClientError(503, "could not validate the commitment term and payment option; try again")
}

// commitmentComboUnsupported is the 400 a save gets for a (term, payment)
// combination the probe data says the service does not sell.
func commitmentComboUnsupported(cfg config.ServiceConfig) error {
	return NewClientError(400, fmt.Sprintf(
		"%s does not support %dyr %s commitments",
		cfg.Service, cfg.Term, cfg.Payment,
	))
}

// commitmentOptsSnapshot is the commitment-option data a service-config PUT
// checks its merged row against. It is fetched before the save opens its
// transaction (issue #1488): Get reads the store through pooled connections
// and can probe AWS on a cold cache, and doing that while the transaction
// holds a pooled connection deadlocks the pool once as many saves run at
// once as the pool has connections (the Settings page saves its rows in
// parallel). check is pure, so it is safe inside the transaction.
type commitmentOptsSnapshot struct {
	opts commitmentopts.Options
	err  error
	// loaded is false when there is nothing to check against: no service is
	// wired, the save is not AWS, or no probe data exists (ErrNoData).
	loaded bool
}

// prefetchCommitmentOptions fetches the snapshot for a save of provider. A
// read error is kept rather than returned, so it fails only a save whose
// merged row actually needs the check, as checkCommitmentOptionCombo does.
func (h *Handler) prefetchCommitmentOptions(ctx context.Context, provider string) commitmentOptsSnapshot {
	if h.commitmentOpts == nil || provider != "aws" {
		return commitmentOptsSnapshot{}
	}
	opts, err := h.commitmentOpts.Get(ctx)
	if errors.Is(err, commitmentopts.ErrNoData) {
		return commitmentOptsSnapshot{}
	}
	return commitmentOptsSnapshot{opts: opts, err: err, loaded: true}
}

// check applies checkCommitmentOptionCombo's rules to cfg against the
// snapshot, without touching the database or the network.
func (s commitmentOptsSnapshot) check(cfg config.ServiceConfig) error {
	if !s.loaded || cfg.Provider != "aws" || cfg.Term <= 0 || cfg.Payment == "" {
		return nil
	}
	if s.err != nil {
		return commitmentComboUnverifiable(s.err)
	}
	if !commitmentopts.ValidateCombo(s.opts, cfg.Provider, cfg.Service, cfg.Term, cfg.Payment) {
		return commitmentComboUnsupported(cfg)
	}
	return nil
}

// serviceConfigOverlayKeys are the JSON fields the Settings UI edits: the four
// scalars (enabled, term, payment, coverage) and the recommendation filters.
// They are overlaid onto the existing row only when the
// request body actually contains the key, so a partial PUT from a non-UI
// client (or an older UI build) never zeroes a filter the operator set
// elsewhere. RampSchedule is intentionally absent: it is still set out-of-band
// and the UI does not own it.
var serviceConfigOverlayKeys = []string{
	"enabled", "term", "payment", "coverage",
	"include_engines", "exclude_engines",
	"include_regions", "exclude_regions",
	"include_types", "exclude_types",
	"min_count",
}

// mergeServiceConfig overlays the UI-editable fields from cfg onto the stored
// row existing. Every field (Enabled, Term, Payment, Coverage and the
// include/exclude engines/regions/types and min_count filters) is overlaid
// only when the request body actually carried the corresponding key (per
// presentKeys), so a partial PUT cannot silently revert or wipe a field it
// did not mean to touch. body is the raw request JSON used to detect which
// keys are present.
//
// A nil existing means no stored row: cfg is returned unchanged. existing is
// not modified.
func mergeServiceConfig(existing *config.ServiceConfig, cfg config.ServiceConfig, body string) (config.ServiceConfig, error) {
	if existing == nil {
		return cfg, nil // new record — no existing fields to preserve
	}

	present, perr := presentKeys(body, serviceConfigOverlayKeys)
	if perr != nil {
		return cfg, perr
	}
	merged := *existing
	overlayPresentScalarFields(&merged, &cfg, present)
	overlayPresentFilterFields(&merged, &cfg, present)
	return merged, nil
}

// presentKeys returns the subset of keys that appear as top-level fields in the
// JSON body. A malformed body is reported as a client error so the caller
// returns 400 rather than silently overlaying nothing.
func presentKeys(body string, keys []string) (map[string]bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, NewClientError(400, "invalid request body")
	}
	present := make(map[string]bool, len(keys))
	for _, k := range keys {
		if _, ok := raw[k]; ok {
			present[k] = true
		}
	}
	return present, nil
}

// overlayPresentScalarFields copies enabled, term, payment and coverage from src
// onto dst only when the body carried the key, so a client that sends just the
// field the user edited (the Settings page, #539) cannot revert the others. A
// present zero value (enabled:false, coverage:0) still applies.
func overlayPresentScalarFields(dst, src *config.ServiceConfig, present map[string]bool) {
	if present["enabled"] {
		dst.Enabled = src.Enabled
	}
	if present["term"] {
		dst.Term = src.Term
	}
	if present["payment"] {
		dst.Payment = src.Payment
	}
	if present["coverage"] {
		dst.Coverage = src.Coverage
	}
}

// overlayPresentFilterFields copies each filter field from src onto dst only
// when its JSON key was present in the request body. Split out of
// mergeServiceConfig to keep that function under the cyclomatic-complexity
// gate.
func overlayPresentFilterFields(dst, src *config.ServiceConfig, present map[string]bool) {
	if present["include_engines"] {
		dst.IncludeEngines = src.IncludeEngines
	}
	if present["exclude_engines"] {
		dst.ExcludeEngines = src.ExcludeEngines
	}
	if present["include_regions"] {
		dst.IncludeRegions = src.IncludeRegions
	}
	if present["exclude_regions"] {
		dst.ExcludeRegions = src.ExcludeRegions
	}
	if present["include_types"] {
		dst.IncludeTypes = src.IncludeTypes
	}
	if present["exclude_types"] {
		dst.ExcludeTypes = src.ExcludeTypes
	}
	if present["min_count"] {
		dst.MinCount = src.MinCount
	}
}

func (h *Handler) getServiceConfig(ctx context.Context, req *events.LambdaFunctionURLRequest, service string) (any, error) {
	// Require view:config permission. Consistent with getConfig (02-M4).
	if _, err := h.requirePermission(ctx, req, "view", "config"); err != nil {
		return nil, err
	}

	// Validate for path traversal attacks
	if err := validateServicePath(service); err != nil {
		return nil, err
	}

	parts := strings.SplitN(service, "/", 2)
	if len(parts) != 2 {
		return nil, NewClientError(400, "invalid service format, expected: provider/service")
	}

	// Validate provider
	if err := validateProvider(parts[0]); err != nil {
		return nil, err
	}

	cfg, err := h.config.GetServiceConfig(ctx, parts[0], parts[1])
	if err != nil {
		return nil, err
	}

	if cfg == nil {
		return &EmptyServiceConfigResponse{}, nil
	}

	return cfg, nil
}

func (h *Handler) updateServiceConfig(ctx context.Context, req *events.LambdaFunctionURLRequest, service string) (*StatusResponse, error) {
	// Require update:config permission
	session, permErr := h.requirePermission(ctx, req, "update", "config")
	if permErr != nil {
		return nil, permErr
	}
	// Service configs are deployment-wide (provider+service, no account), so
	// they need the same unrestricted-account gate as the global config.
	if err := h.requireGlobalConfigScope(ctx, session); err != nil {
		return nil, err
	}

	// Validate for path traversal attacks
	if err := validateServicePath(service); err != nil {
		return nil, err
	}

	var cfg config.ServiceConfig
	if err := json.Unmarshal([]byte(req.Body), &cfg); err != nil {
		return nil, NewClientError(400, "invalid request body")
	}

	parts := strings.SplitN(service, "/", 2)
	if len(parts) == 2 {
		cfg.Provider = parts[0]
		cfg.Service = parts[1]
	}

	// Fetch the commitment options before the transaction opens: the merge
	// callback below runs while the transaction holds a pooled connection,
	// so nothing in it may take another one (see commitmentOptsSnapshot).
	combos := h.prefetchCommitmentOptions(ctx, cfg.Provider)

	// Serialized read-merge-write (issue #1488): the store reads the stored
	// row and writes the merged result in one locked transaction, so two
	// admins saving different fields of this row at once cannot both merge
	// onto the same stale base and lose one change. ClientErrors from the
	// merge or validation propagate unchanged.
	_, err := h.config.UpdateServiceConfigAtomic(ctx, cfg.Provider, cfg.Service, func(existing *config.ServiceConfig) (*config.ServiceConfig, error) {
		return mergeAndValidateServiceConfig(existing, cfg, req.Body, combos)
	})
	if err != nil {
		return nil, err
	}

	return &StatusResponse{Status: "updated"}, nil
}

// mergeAndValidateServiceConfig is the read-merge step of a service-config
// PUT, run inside UpdateServiceConfigAtomic's transaction. It overlays each
// field (the scalars and the recommendation filters) only when the body
// carried its key, so a partial PUT never reverts or zeroes a field
// (ramp_schedule, a filter set out-of-band) it didn't mean to touch, then
// validates the merged row, not the partial body, including its commitment
// combo against the prefetched combos. It touches neither the database nor
// the network: the transaction already holds a pooled connection.
func mergeAndValidateServiceConfig(existing *config.ServiceConfig, cfg config.ServiceConfig, body string, combos commitmentOptsSnapshot) (*config.ServiceConfig, error) {
	merged, err := mergeServiceConfig(existing, cfg, body)
	if err != nil {
		return nil, err
	}
	if err := merged.Validate(); err != nil {
		return nil, NewClientError(400, fmt.Sprintf("validation error: %s", err))
	}
	if err := combos.check(merged); err != nil {
		return nil, err
	}
	return &merged, nil
}
