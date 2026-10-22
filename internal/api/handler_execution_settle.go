package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

const (
	executionSettleReasonMin = 10
	executionSettleReasonMax = 300
	executionSettleBodyMax   = 2048
)

// executionSettleRequest is the body of POST /api/purchases/{id}/settle.
type executionSettleRequest struct {
	Reason      string `json:"reason"`
	Acknowledge bool   `json:"acknowledge_unknown_outcome"`
}

type executionSettleResponse struct {
	ExecutionID string `json:"execution_id"`
	Status      string `json:"status"`
	SettledAt   string `json:"outcome_settled_at"`
	SettledBy   string `json:"outcome_settled_by"`
	Settlement  string `json:"outcome_settlement"`
	Replayed    bool   `json:"replayed"`
}

// executionSettleStore is the part of the store the endpoint needs.
type executionSettleStore interface {
	SettleFailedExecution(ctx context.Context, req config.ExecutionSettleRequest) (*config.ExecutionSettlement, error)
}

// settleExecution records an operator's audited settlement of a failed plan
// execution whose outcome is unknown (issue #999), so it stops blocking the
// delete of its editor-managed plan. The row stays failed and keeps counting
// as possibly bought; it can no longer be retried. Gated like a purchase:
// execute:purchases, account access to the execution, and the grant's account
// constraints with strict scope.
func (h *Handler) settleExecution(ctx context.Context, req *events.LambdaFunctionURLRequest, id string) (any, error) {
	if err := validateUUID(id); err != nil {
		return nil, err
	}
	session, err := h.requirePermission(ctx, req, auth.ActionExecute, auth.ResourcePurchases)
	if err != nil {
		return nil, err
	}
	body, err := decodeExecutionSettle(req.Body)
	if err != nil {
		return nil, err
	}
	if authErr := h.authorizeExecutionSettle(ctx, session, id); authErr != nil {
		return nil, authErr
	}
	store, ok := h.config.(executionSettleStore)
	if !ok {
		return nil, NewClientError(503, "Execution settle storage is unavailable")
	}
	res, err := store.SettleFailedExecution(ctx, config.ExecutionSettleRequest{
		ExecutionID: id, Actor: session.UserID, Reason: body.Reason, Acknowledge: body.Acknowledge,
	})
	if err != nil {
		return nil, mapExecutionSettleError(err)
	}
	if !res.Replayed {
		logging.Infof("purchase execution settled by operator: execution=%s actor=%q via=%s acknowledged_unknown_outcome=true settlement=%q",
			id, session.UserID, credentialKind(ctx, session), res.Settlement)
	}
	return executionSettleResponse{
		ExecutionID: res.ExecutionID, Status: res.Status, SettledAt: res.SettledAt.UTC().Format(time.RFC3339),
		SettledBy: res.SettledBy, Settlement: res.Settlement, Replayed: res.Replayed,
	}, nil
}

// authorizeExecutionSettle checks account access to the execution, then the
// execute:purchases constraints against what the execution would have bought
// (its recommendations' providers, services, regions and total commitment)
// and its accounts. StrictScope makes a dimension the execution leaves
// unknown, such as the provider of a step with no recommendations, require an
// unconstrained grant.
func (h *Handler) authorizeExecutionSettle(ctx context.Context, session *Session, id string) error {
	if err := h.requireExecutionAccess(ctx, session, id); err != nil {
		return err
	}
	execution, err := h.config.GetExecutionByID(ctx, id)
	if errors.Is(err, config.ErrNotFound) || (err == nil && execution == nil) {
		return errNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to get execution: %w", err)
	}
	accounts, err := h.executionAccounts(ctx, execution, nil)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(accounts))
	for i := range accounts {
		ids = append(ids, accounts[i].ID)
	}
	sets := purchaseConstraintSets(execution.Recommendations)
	if len(sets) == 0 {
		sets = []auth.PermissionConstraints{{}}
	}
	for i := range sets {
		sets[i].AccountIDs, sets[i].StrictScope = ids, true
	}
	return h.requirePermissionConstraints(ctx, session, auth.ActionExecute, auth.ResourcePurchases, sets)
}

func mapExecutionSettleError(err error) error {
	switch {
	case errors.Is(err, config.ErrExecutionSettleNotFound):
		return errNotFound
	case errors.Is(err, config.ErrExecutionSettleInvalid):
		return NewClientError(400, err.Error())
	case errors.Is(err, config.ErrExecutionSettleConflict):
		return NewClientError(409, err.Error())
	}
	return err
}

func decodeExecutionSettle(raw string) (executionSettleRequest, error) {
	var body executionSettleRequest
	if len(raw) > executionSettleBodyMax {
		return body, NewClientError(400, "Settle body is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return body, NewClientError(400, "Invalid settle body")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return body, NewClientError(400, "Expected one settle body")
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if !body.Acknowledge {
		return body, NewClientError(400, "acknowledge_unknown_outcome must be true: this purchase may have been submitted; check the cloud account first")
	}
	if n := utf8.RuneCountInString(body.Reason); n < executionSettleReasonMin || n > executionSettleReasonMax {
		return body, NewClientError(400, fmt.Sprintf("reason must be %d to %d characters", executionSettleReasonMin, executionSettleReasonMax))
	}
	if hasControlRune(body.Reason) {
		return body, NewClientError(400, "reason must not contain control characters")
	}
	return body, nil
}
