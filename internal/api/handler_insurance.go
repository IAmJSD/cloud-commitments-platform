package api

import (
	"context"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/aws/aws-lambda-go/events"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/archera"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
)

// InsuranceProvider is the Archera comparison surface. nil means not wired.
type InsuranceProvider interface {
	Status() archera.Status
	PlanID() string
	Client(ctx context.Context) (insurance.QuoteClient, error)
}

// getInsuranceStatus reports whether the Archera comparison settings are
// present (names only, no values, no outbound call, no secret read).
//
// Gate: view:recommendations plus an unrestricted account scope. The Archera
// plan is deployment-global and cannot be attributed to one cloud account, so
// a scoped session gets 404 (same non-leaky shape as requireAccountAccess).
func (h *Handler) getInsuranceStatus(ctx context.Context, req *events.LambdaFunctionURLRequest) (*archera.Status, error) {
	if err := h.requireInsuranceAccess(ctx, req); err != nil {
		return nil, err
	}
	status := archera.Settings{}.Status()
	if h.insurance != nil {
		status = h.insurance.Status()
	}
	return &status, nil
}

func (h *Handler) requireInsuranceAccess(ctx context.Context, req *events.LambdaFunctionURLRequest) error {
	session, err := h.requirePermission(ctx, req, "view", "recommendations")
	if err != nil {
		return err
	}
	scope, err := h.getAccountScope(ctx, session)
	if err != nil {
		return err
	}
	if !scope.AllowsAll() {
		return errNotFound
	}
	if gErr := h.requirePermissionConstraints(ctx, session, auth.ActionView, auth.ResourceRecommendations,
		[]auth.PermissionConstraints{auth.GlobalConstraints()}); gErr != nil {
		if ce, ok := IsClientError(gErr); ok && ce.code == 403 {
			return errNotFound
		}
		return gErr
	}
	return nil
}
