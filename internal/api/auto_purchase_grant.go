package api

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
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

// capCents rounds a cap to the cent, as the NUMERIC(18,2) column will store it,
// so a "raise" below the stored precision is not mistaken for one.
func capCents(v *float64) int64 {
	if v == nil {
		return 0
	}
	return int64(math.Round(*v * 100))
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
	return capCents(after.MaxStepCommitment) > capCents(before.MaxStepCommitment)
}

// credentialKind labels the credential behind a session for logs and stamps.
func credentialKind(ctx context.Context, session *Session) string {
	if principal, ok := principalFromContext(ctx); ok {
		return string(principal.Kind)
	}
	if session != nil && session.UserID == apiKeyAdminUserID {
		return string(PrincipalAdminAPIKey)
	}
	return "user"
}

// requireAutoPurchaseGrant refuses the admin API key and otherwise requires
// execute:purchases, evaluated against the credential presented.
func (h *Handler) requireAutoPurchaseGrant(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session) error {
	if session == nil || session.UserID == apiKeyAdminUserID {
		return NewClientError(403, autoPurchaseGrantDenied)
	}
	_, err := h.requirePermission(ctx, req, auth.ActionExecute, auth.ResourcePurchases)
	return err
}

// requirePlanArmGrant gates a plan write. before is nil on create.
func (h *Handler) requirePlanArmGrant(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, before, after *config.PurchasePlan) error {
	if !planArmEscalates(before, after) {
		return nil
	}
	if err := h.requireAutoPurchaseGrant(ctx, req, session); err != nil {
		return err
	}
	var oldCap *float64
	if before != nil {
		oldCap = before.MaxStepCommitment
	}
	logging.Infof("auto-purchase plan armed or cap raised: plan=%s actor=%s via=%s armed %t -> true cap %s -> %s",
		after.ID, session.UserID, credentialKind(ctx, session), planArmed(before), formatCap(oldCap), formatCap(after.MaxStepCommitment))
	return nil
}

// validateAndGatePlan validates the plan (400) and then applies the arming
// gate (403), in that order so a malformed write never reveals grant state.
func (h *Handler) validateAndGatePlan(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, before, after *config.PurchasePlan) error {
	if err := after.Validate(); err != nil {
		return NewClientError(400, fmt.Sprintf("validation error: %s", err))
	}
	return h.requirePlanArmGrant(ctx, req, session, before, after)
}

func formatCap(v *float64) string {
	if v == nil {
		return "NULL"
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

// requireAutoPurchaseSwitchGrant gates and stamps a change of the global kill
// switch inside the UpdateGlobalConfigAtomic closure. Only off->on needs the
// grant; any change is stamped so the last actor is always recorded.
func (h *Handler) requireAutoPurchaseSwitchGrant(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, before bool, cfg *config.GlobalConfig) error {
	if before == cfg.AutoPurchaseEnabled {
		return nil
	}
	if cfg.AutoPurchaseEnabled {
		if err := h.requireAutoPurchaseGrant(ctx, req, session); err != nil {
			return err
		}
	}
	by, via, at := session.UserID, credentialKind(ctx, session), time.Now().UTC()
	cfg.AutoPurchaseChangedBy, cfg.AutoPurchaseChangedVia, cfg.AutoPurchaseChangedAt = &by, &via, &at
	logging.Infof("auto-purchase kill switch changed: actor=%s via=%s %t -> %t", by, via, before, cfg.AutoPurchaseEnabled)
	return nil
}
