package api

import (
	"context"
	"fmt"

	"github.com/aws/aws-lambda-go/events"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
)

// getApprovalDetails answers POST /api/purchases/approve/{id}/details, the
// read the approval email's deep link makes to show the amount before the
// user confirms (issue #529). GET /api/purchases/{id} needs view:purchases
// and account scope, but the email-link approve is authorized by the
// per-account contact_email instead, so a contact approver without
// view:purchases could approve yet not load the details the modal requires.
//
// It is authorized exactly like the approve it precedes, minus the write:
//   - a session (401) and CSRF (403) are required before the lookup
//     (requireSessionBeforeLookup, issues #435 and #597);
//   - a session holding view:purchases with the execution in scope gets the
//     same response GET /api/purchases/{id} would give it;
//   - otherwise the request needs BOTH a valid approval token for this
//     execution AND a session email that is one of its contact approvers.
//
// Every final denial answers like a missing execution (issue #597). POST
// carries the token in the body so it stays out of access logs (issue #398).
func (h *Handler) getApprovalDetails(ctx context.Context, req *events.LambdaFunctionURLRequest, execID, token string) (any, error) {
	if err := validateUUID(execID); err != nil {
		return nil, err
	}
	if err := h.requireSessionBeforeLookup(ctx, req); err != nil {
		return nil, err
	}
	execution, err := h.getExecutionOr404(ctx, execID)
	if err != nil {
		return nil, err
	}
	if err := h.authorizeApprovalDetailsRead(ctx, req, execution, token); err != nil {
		return nil, concealExecutionDenial(err)
	}
	return buildPurchaseDetailsResponse(execution, h.executionPlanName(ctx, execution)), nil
}

// authorizeApprovalDetailsRead returns nil when the caller may read the
// execution's details on the approval deep link. A session view:purchases
// denial or scope miss falls through to the token branch only when a token is
// present (fallsThroughToToken); a wrapped backend failure propagates.
func (h *Handler) authorizeApprovalDetailsRead(ctx context.Context, req *events.LambdaFunctionURLRequest, execution *config.PurchaseExecution, token string) error {
	err := h.authorizeSessionViewExecution(ctx, h.tryGetSession(ctx, req), execution)
	if err == nil || !fallsThroughToToken(err, token) {
		return err
	}
	if _, err := h.authorizeApprovalAction(ctx, req, execution); err != nil {
		return err
	}
	if err := purchase.ValidateApprovalToken(execution, token); err != nil {
		return tokenActionError(err)
	}
	return nil
}

// authorizeSessionViewExecution applies GET /api/purchases/{id}'s gate:
// view:purchases, then the account scope (errNotFound on a miss).
func (h *Handler) authorizeSessionViewExecution(ctx context.Context, session *Session, execution *config.PurchaseExecution) error {
	if _, err := h.requireSessionPermission(ctx, session, "view", "purchases"); err != nil {
		return err
	}
	scope, err := h.getAccountScope(ctx, session)
	if err != nil {
		return fmt.Errorf("failed to get allowed accounts: %w", err)
	}
	return h.requireExecutionInScope(ctx, scope, execution, nil)
}

// executionPlanName returns the execution's plan name, or "" when it has no
// plan or the plan cannot be read (the name is display-only).
func (h *Handler) executionPlanName(ctx context.Context, execution *config.PurchaseExecution) string {
	if execution.PlanID == "" {
		return ""
	}
	plan, err := h.config.GetPurchasePlan(ctx, execution.PlanID)
	if err != nil || plan == nil {
		return ""
	}
	return plan.Name
}
