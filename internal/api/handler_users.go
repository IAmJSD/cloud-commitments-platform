// Package api provides the HTTP API handlers for the CUDly dashboard.
package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
)

// User management handlers

// listUsers handles GET /api/users.
func (h *Handler) listUsers(ctx context.Context, req *events.LambdaFunctionURLRequest) (any, error) {
	if _, err := h.requirePermission(ctx, req, "view", "users"); err != nil {
		return nil, err
	}

	users, err := h.auth.ListUsersAPI(ctx)
	if err != nil {
		return nil, err
	}

	return map[string]any{"users": users}, nil
}

// createUser handles POST /api/users.
func (h *Handler) createUser(ctx context.Context, req *events.LambdaFunctionURLRequest) (any, error) {
	session, err := h.requirePermission(ctx, req, "create", "users")
	if err != nil {
		return nil, err
	}

	// Rate limiting: 30 admin operations per user per minute
	if h.rateLimiter != nil {
		allowed, rateLimitErr := h.rateLimiter.AllowWithUser(ctx, session.UserID, "admin")
		if rateLimitErr != nil {
			logging.Warnf("rate limiter error on admin operation (user %s): %v", session.UserID, rateLimitErr)
		} else if !allowed {
			return nil, NewClientError(429, "too many requests, please slow down")
		}
	}

	var createReq auth.APICreateUserRequest
	err = json.Unmarshal([]byte(req.Body), &createReq)
	if err != nil {
		return nil, NewClientError(400, "invalid request body")
	}

	// Authorization is group-membership-only: a user must belong to at least
	// one group (issue #907). The service layer re-validates as defense in
	// depth and the DB enforces it via a CHECK constraint.
	if len(createReq.Groups) == 0 {
		return nil, NewClientError(400, "at least one group is required")
	}

	// Decode base64-encoded password
	decoded, decodeErr := decodeBase64Password(createReq.Password)
	if decodeErr != nil {
		return nil, decodeErr
	}
	createReq.Password = decoded

	// session.UserID is the trusted actor identity (from the validated
	// session, never the request body); the service layer uses it to cap the
	// requested groups at the actor's own grant ceiling (issue #226).
	user, err := h.auth.CreateUserAPI(ctx, session.UserID, createReq)
	if err != nil {
		return nil, mapAuthError(err)
	}

	return user, nil
}

// mapAuthError maps internal/auth sentinel errors to the appropriate
// ClientError status code so validation failures surface to the user
// as a 4xx with the real message instead of a generic 500. Unrecognized
// errors are returned unchanged for handleRequestError to render as 500.
// Used by both /api/users (createUser) and the bootstrap setupAdmin
// endpoint — they share the sentinel set. Issue #349.
func mapAuthError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidEmail),
		errors.Is(err, auth.ErrNoGroups),
		errors.Is(err, auth.ErrContradictoryGroups),
		errors.Is(err, auth.ErrPasswordPolicy):
		return NewClientError(400, err.Error())
	case errors.Is(err, auth.ErrSelfEscalation),
		errors.Is(err, auth.ErrPermissionCeiling),
		errors.Is(err, auth.ErrPermissionNotGrantable):
		return NewClientError(403, err.Error())
	case errors.Is(err, auth.ErrEmailInUse),
		errors.Is(err, auth.ErrAdminExists),
		errors.Is(err, auth.ErrLastAdmin),
		errors.Is(err, auth.ErrUserChanged):
		return NewClientError(409, err.Error())
	case errors.Is(err, auth.ErrUserNotFound):
		// Fixed text: the wrapped error carries the store's "no rows" detail.
		return NewClientError(404, "user not found")
	}
	return err
}

// getUser handles GET /api/users/{id}.
func (h *Handler) getUser(ctx context.Context, req *events.LambdaFunctionURLRequest, userID string) (any, error) {
	// Validate UUID format to prevent injection attacks
	if err := validateUUID(userID); err != nil {
		return nil, err
	}

	if _, err := h.requirePermission(ctx, req, "view", "users"); err != nil {
		return nil, err
	}

	user, err := h.auth.GetUser(ctx, userID)
	if err != nil {
		return nil, mapAuthError(err)
	}

	return user, nil
}

// updateUser handles PUT /api/users/{id}.
func (h *Handler) updateUser(ctx context.Context, req *events.LambdaFunctionURLRequest, userID string) (any, error) {
	// Validate UUID format to prevent injection attacks
	if err := validateUUID(userID); err != nil {
		return nil, err
	}

	session, err := h.requirePermission(ctx, req, "update", "users")
	if err != nil {
		return nil, err
	}

	var updateReq auth.APIUpdateUserRequest
	err = json.Unmarshal([]byte(req.Body), &updateReq)
	if err != nil {
		return nil, NewClientError(400, "invalid request body")
	}

	// session.UserID is the trusted actor identity (from the validated
	// session, never the request body); the service layer uses it to enforce
	// the self-escalation guard (issue #907).
	user, err := h.auth.UpdateUserAPI(ctx, session.UserID, userID, updateReq)
	if err != nil {
		return nil, mapAuthError(err)
	}

	return user, nil
}

// deleteUser handles DELETE /api/users/{id}.
func (h *Handler) deleteUser(ctx context.Context, req *events.LambdaFunctionURLRequest, userID string) (any, error) {
	// Validate UUID format to prevent injection attacks
	if err := validateUUID(userID); err != nil {
		return nil, err
	}

	session, err := h.requirePermission(ctx, req, "delete", "users")
	if err != nil {
		return nil, err
	}

	// Prevent self-deletion
	if session.UserID == userID {
		return nil, NewClientError(400, "cannot delete your own account")
	}

	if err := h.auth.DeleteUser(ctx, userID); err != nil {
		return nil, mapAuthError(err)
	}

	return map[string]string{"status": "user deleted"}, nil
}
