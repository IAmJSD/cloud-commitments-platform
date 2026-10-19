// Package api provides the HTTP API handlers for the CUDly dashboard.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

// Group management handlers

// listGroups handles GET /api/groups.
func (h *Handler) listGroups(ctx context.Context, req *events.LambdaFunctionURLRequest) (any, error) {
	if _, err := h.requirePermission(ctx, req, "view", "groups"); err != nil {
		return nil, err
	}

	groups, err := h.auth.ListGroupsAPI(ctx)
	if err != nil {
		return nil, err
	}

	return map[string]any{"groups": groups}, nil
}

// createGroup handles POST /api/groups.
func (h *Handler) createGroup(ctx context.Context, req *events.LambdaFunctionURLRequest) (any, error) {
	session, err := h.requirePermission(ctx, req, "create", "groups")
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

	var createReq auth.APICreateGroupRequest
	err = json.Unmarshal([]byte(req.Body), &createReq)
	if err != nil {
		return nil, NewClientError(400, "invalid request body")
	}

	createReq.AllowedAccounts, err = h.bindGroupAccounts(ctx, session, createReq.AllowedAccounts)
	if err != nil {
		return nil, err
	}

	group, err := h.auth.CreateGroupAPI(ctx, session.UserID, createReq)
	if err != nil {
		return nil, mapGroupAuthError(err)
	}

	return group, nil
}

// mapGroupAuthError maps the group-write sentinels to 403 ClientErrors so a
// refused grant surfaces the specific permission that was refused instead of
// a generic 500. Kept separate from mapAuthError (handler_users.go) because
// that switch is already at the cyclomatic limit and the two sentinel sets
// are disjoint. Unrecognized errors pass through for handleRequestError.
func mapGroupAuthError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidPermission), errors.Is(err, auth.ErrEmptyPermissions):
		// Malformed input, not an authorization failure.
		return NewClientError(400, err.Error())
	case errors.Is(err, auth.ErrPermissionCeiling),
		errors.Is(err, auth.ErrPermissionNotGrantable),
		errors.Is(err, auth.ErrSystemManagedGroup):
		return NewClientError(403, err.Error())
	}
	return err
}

// getGroup handles GET /api/groups/{id}.
func (h *Handler) getGroup(ctx context.Context, req *events.LambdaFunctionURLRequest, groupID string) (any, error) {
	// Validate UUID format to prevent injection attacks
	if err := validateUUID(groupID); err != nil {
		return nil, err
	}

	if _, err := h.requirePermission(ctx, req, "view", "groups"); err != nil {
		return nil, err
	}

	group, err := h.auth.GetGroupAPI(ctx, groupID)
	if err != nil {
		return nil, err
	}

	return group, nil
}

// updateGroup handles PUT /api/groups/{id}.
func (h *Handler) updateGroup(ctx context.Context, req *events.LambdaFunctionURLRequest, groupID string) (any, error) {
	// Validate UUID format to prevent injection attacks
	if err := validateUUID(groupID); err != nil {
		return nil, err
	}

	session, err := h.requirePermission(ctx, req, "update", "groups")
	if err != nil {
		return nil, err
	}

	var updateReq auth.APIUpdateGroupRequest
	if unmarshalErr := json.Unmarshal([]byte(req.Body), &updateReq); unmarshalErr != nil {
		return nil, NewClientError(400, "invalid request body")
	}

	updateReq.AllowedAccounts, err = h.bindGroupAccounts(ctx, session, updateReq.AllowedAccounts)
	if err != nil {
		return nil, err
	}

	group, err := h.auth.UpdateGroupAPI(ctx, session.UserID, groupID, updateReq)
	if err != nil {
		return nil, mapGroupAuthError(err)
	}

	return group, nil
}

// deleteGroup handles DELETE /api/groups/{id}.
func (h *Handler) deleteGroup(ctx context.Context, req *events.LambdaFunctionURLRequest, groupID string) (any, error) {
	// Validate UUID format to prevent injection attacks
	if err := validateUUID(groupID); err != nil {
		return nil, err
	}

	if _, err := h.requirePermission(ctx, req, "delete", "groups"); err != nil {
		return nil, err
	}

	if err := h.auth.DeleteGroup(ctx, groupID); err != nil {
		return nil, mapGroupAuthError(err)
	}

	return map[string]string{"status": "group deleted"}, nil
}

// groupAccountEntryMsg is the refusal for an allowed_accounts entry that a
// group write cannot bind to an account. It reads the same whether the entry
// is unknown, ambiguous or names an account outside the actor's own scope, so
// the response does not reveal which accounts exist.
const groupAccountEntryMsg = "allowed_accounts entry %q does not identify exactly one cloud account you can access; use the account ID"

// allAccountsEntry is the allowed_accounts wildcard (auth.IsUnrestrictedAccess).
const allAccountsEntry = "*"

// bindGroupAccounts binds the allowed_accounts of a group write to stable
// account identifiers before anything is stored (issue #1034).
//
// AccountScope matches entries by account ID only, because a display name is
// not bound to an account: once the account is deleted or renamed, the name
// would grant whichever account takes it next. Clients still send names (the
// group duplicate modal does), so a name is resolved HERE, at grant time, to
// the ID of the one registered account that carries it, and that ID is what
// the group stores. Later renames and reuse of the name then change nothing.
//
// Each entry must be "*", a registered account ID, a registered account's
// external ID (kept verbatim, as before), or a name exactly one registered
// account carries and the acting principal can access. Anything else is
// refused with 400 rather than stored: an unresolvable entry must not be kept
// as a name, and dropping it could turn a restricted list into an empty,
// unrestricted one. nil ("not sent") and an empty list pass through unchanged
// for the ceiling checks in the auth service to judge.
//
// Fails closed: if the account list or the actor's scope cannot be loaded,
// the write is refused.
func (h *Handler) bindGroupAccounts(ctx context.Context, session *Session, entries []string) ([]string, error) {
	if !needsAccountBinding(entries) {
		return entries, nil
	}
	if h.config == nil {
		return nil, fmt.Errorf("cannot bind allowed_accounts: account store not configured")
	}
	accounts, err := h.config.ListCloudAccounts(ctx, config.CloudAccountFilter{})
	if err != nil {
		return nil, fmt.Errorf("cannot bind allowed_accounts: %w", err)
	}
	actor, err := h.getAccountScope(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("cannot bind allowed_accounts: %w", err)
	}
	dir := newGroupAccountDirectory(accounts)
	bound := make([]string, len(entries))
	for i, entry := range entries {
		id, ok := dir.bind(entry, actor)
		if !ok {
			return nil, NewClientError(400, fmt.Sprintf(groupAccountEntryMsg, entry))
		}
		bound[i] = id
	}
	return bound, nil
}

// needsAccountBinding reports whether entries holds anything besides "*".
func needsAccountBinding(entries []string) bool {
	for _, e := range entries {
		if e != allAccountsEntry {
			return true
		}
	}
	return false
}

// groupAccountDirectory indexes the registered accounts for bindGroupAccounts.
type groupAccountDirectory struct {
	ids          map[string]bool
	externalIDs  map[string]bool
	uniqueByName map[string]config.CloudAccount
}

func newGroupAccountDirectory(accounts []config.CloudAccount) groupAccountDirectory {
	d := groupAccountDirectory{
		ids:          make(map[string]bool, len(accounts)),
		externalIDs:  make(map[string]bool, len(accounts)),
		uniqueByName: make(map[string]config.CloudAccount, len(accounts)),
	}
	shared := make(map[string]bool)
	for i := range accounts {
		a := accounts[i]
		d.ids[a.ID] = true
		if a.ExternalID != "" {
			d.externalIDs[a.ExternalID] = true
		}
		if a.Name == "" {
			continue
		}
		if _, seen := d.uniqueByName[a.Name]; seen || shared[a.Name] {
			shared[a.Name] = true
			delete(d.uniqueByName, a.Name)
			continue
		}
		d.uniqueByName[a.Name] = a
	}
	return d
}

// bind returns the identifier to store for entry, or false when entry does
// not identify one account the actor can access. Names are compared exactly,
// as AccountScope compares IDs.
func (d groupAccountDirectory) bind(entry string, actor auth.AccountScope) (string, bool) {
	if entry == allAccountsEntry || d.ids[entry] || d.externalIDs[entry] {
		return entry, true
	}
	acct, ok := d.uniqueByName[entry]
	if !ok || !actor.Allows(acct.ID, acct.Name) {
		return "", false
	}
	return acct.ID, true
}
