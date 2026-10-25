package api

import (
	"context"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
)

// Post-claim Constraints re-checks (issue #415).
//
// Every synchronous execute entry point checks the caller's permission
// Constraints (MaxPurchaseAmount, Providers, Services, Regions, AccountIDs)
// against a row read by GetExecutionByID, then calls the purchase funnel,
// which buys the row its claim CAS returns. A repricing, an edit or a plan
// step recommendation attach landing between that read and the CAS would be
// bought unchecked. Each check below re-runs the same Constraints the entry
// point already ran, on the claimed row, before any provider call. The
// pre-CAS check stays: it refuses an over-cap request on an unchanged row
// without touching the row. A post-claim refusal ends the row "failed" with
// no_submission set (see purchase.ClaimedRowCheck).

// runNowClaimCheck is runPlannedPurchase's update-any / creator check
// (issue #1129) and execute:purchases Constraints check, against the claimed
// row.
func (h *Handler) runNowClaimCheck(session *Session) purchase.ClaimedRowCheck {
	return func(ctx context.Context, claimed *config.PurchaseExecution) error {
		if err := h.authorizeClaimedRowManagement(ctx, session, claimed); err != nil {
			return err
		}
		return h.enforcePurchaseConstraints(ctx, session, "execute", claimed.Recommendations)
	}
}

// sessionApproveClaimCheck is approvePurchaseViaSession's approve-any /
// approve-own check, against the claimed row.
func (h *Handler) sessionApproveClaimCheck(session *Session) purchase.ClaimedRowCheck {
	return func(ctx context.Context, claimed *config.PurchaseExecution) error {
		return h.authorizeSessionApprove(ctx, session, claimed)
	}
}

// tokenApproveClaimCheck is approveViaToken's Constraints check (including the
// plan step cap for an approver with no approve verb), against the claimed
// row. session may be nil, as on the pre-CAS check.
func (h *Handler) tokenApproveClaimCheck(session *Session) purchase.ClaimedRowCheck {
	return func(ctx context.Context, claimed *config.PurchaseExecution) error {
		return h.approveConstraintsForSession(ctx, session, claimed)
	}
}

// directExecuteClaimCheck is the direct-execute path's execute:purchases and
// execute-any / execute-own Constraints checks, against the claimed row. The
// creator comes from the claimed row: an execute-own session must still be
// its creator.
func (h *Handler) directExecuteClaimCheck(session *Session) purchase.ClaimedRowCheck {
	return func(ctx context.Context, claimed *config.PurchaseExecution) error {
		if err := h.enforcePurchaseConstraints(ctx, session, "execute", claimed.Recommendations); err != nil {
			return err
		}
		return h.authorizeSessionExecuteDirect(ctx, session, derefStringOrEmpty(claimed.CreatedByUserID), claimed.Recommendations)
	}
}

// claimedRowRefusalError maps a purchase.ErrClaimedRowRefused failure. A
// Constraints denial keeps its status code (403, or 400 for a zero-commitment
// batch) and says the row was not bought; a failure to evaluate the check
// (an auth backend error) stays a server error. Either way the row already
// ended failed and nothing was bought.
func claimedRowRefusalError(final *config.PurchaseExecution, err error) error {
	ce, ok := IsClientError(err)
	if !ok {
		return fmt.Errorf("purchase refused after claim, nothing was bought: %w", err)
	}
	details := map[string]any{}
	if final != nil {
		details["execution_id"] = final.ExecutionID
		details["status"] = final.Status
	}
	return NewClientErrorWithDetails(ce.code,
		"the purchase changed after it was authorized and no longer passes your permission checks, so nothing was bought and it is now marked failed: "+ce.message,
		details)
}
