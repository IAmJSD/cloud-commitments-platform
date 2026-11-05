package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/planstep"
	"github.com/aws/aws-lambda-go/events"
)

// Auto-purchase arming is a money-spending authorization: once the global kill
// switch is on, an armed plan buys with no approver. Arming therefore needs the
// same execute:purchases verb the direct execute handlers require, which is
// carved out of admin:* (auth.adminCarvedOuts), instead of the update:config /
// update:plans verbs any admin holds. The stateless admin API key skips every
// permission check, so it is refused outright for escalations.
// De-escalations (switch off, disarm, lower cap) stay on the ordinary verbs.

const autoPurchaseGrantDenied = "arming auto-purchase requires a user holding execute:purchases; the admin API key cannot do it"

// planArmed reports whether the plan, with the kill switch on, would buy without
// an approver: enabled, auto-purchase, and a non-NULL step cap (a NULL cap is
// refused at run time). It deliberately ignores the global switch so plan
// handlers never read global_config.
func planArmed(p *config.PurchasePlan) bool {
	return p != nil && p.Enabled && p.AutoPurchase && p.MaxStepCommitment != nil
}

// capCents returns the cap in exact cents. Plans reaching the gate have passed
// Validate, which rejects caps that are not whole cents, so the error branch is
// unreachable there; it still fails closed (treated as the maximum, so any write
// is a raise and needs the grant).
func capCents(v *float64) int64 {
	if v == nil {
		return 0
	}
	c, err := config.StepCapCents(*v)
	if err != nil {
		return math.MaxInt64
	}
	return c
}

// planArmEscalates reports whether the write takes the plan from not-armed to
// armed, or raises the cap of an already armed plan. before is nil on create.
func planArmEscalates(before, after *config.PurchasePlan) bool {
	if !planArmed(after) {
		return false
	}
	if !planArmed(before) {
		return true
	}
	if capCents(after.MaxStepCommitment) > capCents(before.MaxStepCommitment) {
		return true
	}
	// New providers or services widen what the armed plan buys, so the grant's
	// provider and service constraints must be checked again.
	// A plan whose provider cannot be derived fails closed as an escalation;
	// requirePlanArmGrant then refuses it.
	afterProviders, errAfter := config.DerivePlanProviders(after)
	beforeProviders, errBefore := config.DerivePlanProviders(before)
	if errAfter != nil || errBefore != nil {
		return true
	}
	return !subsetOf(afterProviders, beforeProviders) ||
		!subsetOf(config.DerivePlanServices(after), config.DerivePlanServices(before))
}

func subsetOf(sub, super []string) bool {
	for _, v := range sub {
		if !slices.Contains(super, v) {
			return false
		}
	}
	return true
}

// credentialKind labels the credential behind a session for logs and stamps.
func credentialKind(ctx context.Context, session *Session) string {
	if principal, ok := principalFromContext(ctx); ok {
		return string(principal.Kind)
	}
	if session != nil && session.UserID == apiKeyAdminUserID {
		return string(PrincipalAdminAPIKey)
	}
	return string(PrincipalSession)
}

// requireAutoPurchaseGrant refuses the admin API key and otherwise requires
// execute:purchases, evaluated against the credential presented. It checks the
// verb only; callers that know the target accounts use requireAutoPurchaseGrantFor.
func (h *Handler) requireAutoPurchaseGrant(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session) error {
	if err := refuseAutoPurchaseCredential(session); err != nil {
		return err
	}
	_, err := h.requirePermission(ctx, req, auth.ActionExecute, auth.ResourcePurchases)
	return err
}

// refuseAutoPurchaseCredential is the credential half of the auto-purchase
// grant: no session, or the stateless admin API key, can never arm.
func refuseAutoPurchaseCredential(session *Session) error {
	if session == nil || session.UserID == apiKeyAdminUserID {
		return NewClientError(403, autoPurchaseGrantDenied)
	}
	return nil
}

// requireAutoPurchaseGrantFor is requireAutoPurchaseGrant plus the grant's own
// account and provider constraints, checked against the accounts and providers
// the arming write targets. Regions are never sent: a plan's region filters
// are optional, so a regions-constrained grant cannot be shown to cover it and
// fails closed under StrictScope. An empty accountIDs is a request with no known
// account, which only an unconstrained grant covers.
func (h *Handler) requireAutoPurchaseGrantFor(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, accountIDs, providers, services []string) error {
	if err := h.requireAutoPurchaseGrant(ctx, req, session); err != nil {
		return err
	}
	return h.requirePermissionConstraints(ctx, session, auth.ActionExecute, auth.ResourcePurchases,
		[]auth.PermissionConstraints{{AccountIDs: accountIDs, Providers: providers, Services: services, StrictScope: true}})
}

// requireAutoPurchaseGrantForPlan is requireAutoPurchaseGrantFor for the
// providers and services the plan targets. A plan whose providers cannot be
// derived fails closed: an empty provider set would read as no restriction.
func (h *Handler) requireAutoPurchaseGrantForPlan(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, accountIDs []string, plan *config.PurchasePlan) error {
	providers, err := config.DerivePlanProviders(plan)
	if err != nil {
		logging.Errorf("requireAutoPurchaseGrantForPlan: %v", err)
		return NewClientError(500, "failed to check the arming grant")
	}
	return h.requireAutoPurchaseGrantFor(ctx, req, session, accountIDs, providers, config.DerivePlanServices(plan))
}

// planArmAccountIDs returns the accounts the plan targets: the request's own
// list on create (the plan has no id yet), the stored association otherwise.
func (h *Handler) planArmAccountIDs(ctx context.Context, plan *config.PurchasePlan, targets []string) ([]string, error) {
	if len(targets) > 0 {
		return targets, nil
	}
	accounts, err := h.config.GetPlanAccounts(ctx, plan.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get plan accounts: %w", err)
	}
	ids := make([]string, 0, len(accounts))
	for i := range accounts {
		ids = append(ids, accounts[i].ID)
	}
	return ids, nil
}

// armingGrants holds the outcome (nil = granted) of the two requirePermission
// checks a global_config write may need to arm unattended spend:
// execute:purchases (auto-purchase and ladder execution switches) and
// execute:ri-exchange (RI exchange auto mode and its caps).
//
// They are resolved by resolveArmingGrants BEFORE UpdateGlobalConfigAtomic
// opens its transaction, and the gates inside the closure read them from here.
// The closure runs while the transaction holds a pooled connection; a
// requirePermission call there needs a second one from the same pool, so as
// many concurrent arming saves as the pool has connections deadlocked the
// instance (issue #1517). Whether a write escalates still depends on the
// advisory-locked row, so it is still decided inside the closure; only the
// permission lookup moved out. A lookup error is kept, not raised, so a write
// that escalates nothing is unaffected by it, exactly as when the lookup ran
// only on escalation.
type armingGrants struct {
	purchases  error
	riExchange error
}

// resolveArmingGrants evaluates both execute verbs against the credential
// presented, through requirePermission, so a user API key is held to its own
// effective permissions just as the in-transaction checks did.
func (h *Handler) resolveArmingGrants(ctx context.Context, req *events.LambdaFunctionURLRequest) armingGrants {
	return armingGrants{
		purchases:  h.resolveGrant(ctx, req, auth.ResourcePurchases, autoPurchaseSwitchScopeMsg),
		riExchange: h.resolveGrant(ctx, req, auth.ResourceRIExchange, riExchangeAutoModeScopeMsg),
	}
}

// resolveGrant returns the outcome of an unconstrained execute grant on
// resource (requireGlobalPermission): both gated writes arm unattended spend
// across every account. msg is the 403 text for a restricted scope.
func (h *Handler) resolveGrant(ctx context.Context, req *events.LambdaFunctionURLRequest, resource, msg string) error {
	return h.requireGlobalPermission(ctx, req, auth.ActionExecute, resource, msg)
}

const riExchangeAutoModeScopeMsg = "permission denied: enabling RI exchange auto mode requires unrestricted account access"
const autoPurchaseSwitchScopeMsg = "permission denied: arming a global switch requires unrestricted account access"

// requirePlanArmGrant gates a plan write. before is nil on create. targets is
// the request's target_accounts on create and nil when the plan already exists.
func (h *Handler) requirePlanArmGrant(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, before, after *config.PurchasePlan, targets []string) error {
	if !planArmEscalates(before, after) {
		return nil
	}
	accountIDs, err := h.planArmAccountIDs(ctx, after, targets)
	if err != nil {
		return err
	}
	return h.requirePlanArmGrantFor(ctx, req, session, before, after, accountIDs)
}

// requirePlanArmGrantFor is requirePlanArmGrant for an escalating write whose
// target accounts are already resolved.
func (h *Handler) requirePlanArmGrantFor(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, before, after *config.PurchasePlan, accountIDs []string) error {
	// A bound plan that lost its binding must not be armed or widened
	// (#1746); disarming and disabling it never reach this gate.
	if err := planstep.CheckBoundBinding(after); err != nil {
		return NewClientError(http.StatusConflict, err.Error())
	}
	if err := h.requireAutoPurchaseGrantForPlan(ctx, req, session, accountIDs, after); err != nil {
		return err
	}
	var oldCap *float64
	if before != nil {
		oldCap = before.MaxStepCommitment
	}
	logging.Infof("auto-purchase plan arm or cap raise authorized (write follows): plan=%s actor=%s via=%s armed %t -> true cap %s -> %s",
		after.ID, session.UserID, credentialKind(ctx, session), planArmed(before), formatCap(oldCap), formatCap(after.MaxStepCommitment))
	return nil
}

// validateAndGatePlan validates the plan (400) and then applies the arming
// gate (403), in that order so a malformed write never reveals grant state.
func (h *Handler) validateAndGatePlan(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, before, after *config.PurchasePlan, targets []string) error {
	if err := after.Validate(); err != nil {
		return NewClientError(400, fmt.Sprintf("validation error: %s", err))
	}
	return h.requirePlanArmGrant(ctx, req, session, before, after, targets)
}

func formatCap(v *float64) string {
	if v == nil {
		return "NULL"
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

// requireAutoPurchaseSwitchGrant gates and stamps a change of the global kill
// switch inside the UpdateGlobalConfigAtomic closure. Only off->on needs the
// grant; any change is stamped so the last actor is always recorded. grant is
// the pre-resolved execute:purchases outcome (armingGrants.purchases): nothing
// here may read through the pool while the transaction holds a connection.
func requireAutoPurchaseSwitchGrant(ctx context.Context, session *Session, grant error, before bool, cfg *config.GlobalConfig) error {
	if before == cfg.AutoPurchaseEnabled {
		return nil
	}
	if cfg.AutoPurchaseEnabled {
		if err := prefetchedAutoPurchaseGrant(session, grant); err != nil {
			return err
		}
	}
	by, via, at := session.UserID, credentialKind(ctx, session), time.Now().UTC()
	cfg.AutoPurchaseChangedBy, cfg.AutoPurchaseChangedVia, cfg.AutoPurchaseChangedAt = &by, &via, &at
	return nil
}

// prefetchedAutoPurchaseGrant is requireAutoPurchaseGrant with the
// execute:purchases lookup already done: the credential refusal first, then the
// pre-resolved permission outcome.
func prefetchedAutoPurchaseGrant(session *Session, grant error) error {
	if err := refuseAutoPurchaseCredential(session); err != nil {
		return err
	}
	return grant
}

// logAutoPurchaseSwitchChange records a committed change of the kill switch.
func logAutoPurchaseSwitchChange(ctx context.Context, session *Session, before bool, after *config.GlobalConfig) {
	if after == nil || after.AutoPurchaseEnabled == before {
		return
	}
	logging.Infof("auto-purchase kill switch changed: actor=%s via=%s %t -> %t", session.UserID, credentialKind(ctx, session), before, after.AutoPurchaseEnabled)
}

// requireRetargetOfArmedPlan gates PUT /api/plans/{id}/accounts: moving an
// armed plan to other accounts changes where it buys without an approver, so
// the caller needs the arming grant for the new account set. Unarmed plans
// need only the ordinary update:plans check. It runs as the gate of
// SetPlanAccountsGated, so the plan and its armed state are read under the
// plan's row lock.
func (h *Handler) requireRetargetOfArmedPlan(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, planID string, accountIDs []string) (*config.PurchasePlan, error) {
	plan, err := h.getPlanForAccountProviderValidation(ctx, planID)
	if err != nil {
		return nil, err
	}
	if !planArmed(plan) {
		return plan, nil
	}
	return plan, h.requireAutoPurchaseGrantForPlan(ctx, req, session, accountIDs, plan)
}

// errPlanMovedUnderGate means the plan changed between the grant check and the
// plan row lock, so the check no longer describes what would be written.
var errPlanMovedUnderGate = errors.New("plan changed under the arming gate")

// planGateAttempts bounds the re-checks after errPlanMovedUnderGate.
const planGateAttempts = 3

var errPlanGateContended = NewClientError(409, "plan changed; refresh the plan and try again")
