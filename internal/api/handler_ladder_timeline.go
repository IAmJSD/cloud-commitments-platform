package api

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

type ladderTimelineRequest struct {
	store     config.LadderTimelineStore
	accountID string
	provider  string
	cursor    *config.LadderTimelineCursor
}

func (h *Handler) ladderTimelineScope(ctx context.Context, req *events.LambdaFunctionURLRequest) (ladderTimelineRequest, error) {
	session, err := h.requirePermission(ctx, req, string(auth.ActionView), string(auth.ResourceConfig))
	if err != nil {
		return ladderTimelineRequest{}, err
	}
	params := req.QueryStringParameters
	accountID, provider := params["account_id"], params["provider"]
	if accountID == "" || provider == "" {
		return ladderTimelineRequest{}, NewClientError(400, "account_id and provider are required")
	}
	account, err := h.requireAccountAccess(ctx, session, accountID)
	if err != nil {
		return ladderTimelineRequest{}, err
	}
	if account.Provider != provider {
		return ladderTimelineRequest{}, NewClientError(404, "account not found")
	}
	constraints := []auth.PermissionConstraints{{AccountIDs: []string{accountID}, Providers: []string{provider}, StrictScope: true}}
	err = h.requirePermissionConstraints(ctx, session, auth.ActionView, auth.ResourceConfig, constraints)
	if err != nil {
		return ladderTimelineRequest{}, err
	}
	if common.ProviderType(provider) != common.ProviderAWS {
		return ladderTimelineRequest{}, NewClientError(501, "Ladder planning is currently available only for AWS accounts")
	}
	cursor, err := ladderTimelineCursor(params)
	if err != nil {
		return ladderTimelineRequest{}, err
	}
	store, ok := h.config.(config.LadderTimelineStore)
	if !ok {
		return ladderTimelineRequest{}, NewClientError(503, "Ladder timeline storage is unavailable")
	}
	return ladderTimelineRequest{store: store, accountID: accountID, provider: provider, cursor: cursor}, nil
}

func ladderTimelineCursor(params map[string]string) (*config.LadderTimelineCursor, error) {
	created, id := params["after_created_at"], params["after_id"]
	if created == "" && id == "" {
		return nil, nil
	}
	stamp, err := time.Parse(time.RFC3339Nano, created)
	if err != nil || stamp.IsZero() {
		return nil, NewClientError(400, "after_created_at must be a valid RFC3339 timestamp")
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, NewClientError(400, "after_id must be a UUID")
	}
	return &config.LadderTimelineCursor{CreatedAt: stamp, ID: id}, nil
}

func (h *Handler) getLadderTimeline(ctx context.Context, req *events.LambdaFunctionURLRequest) (any, error) {
	scope, err := h.ladderTimelineScope(ctx, req)
	if err != nil {
		return nil, err
	}
	page, err := scope.store.ListLadderTimeline(ctx, scope.accountID, scope.provider, scope.cursor)
	if errors.Is(err, config.ErrLadderTimelineRate) {
		return nil, NewClientError(500, "Ladder timeline contains a tranche with an unreadable commitment rate")
	}
	return page, err
}

func (h *Handler) getLadderTimelineRuns(ctx context.Context, req *events.LambdaFunctionURLRequest) (any, error) {
	scope, err := h.ladderTimelineScope(ctx, req)
	if err != nil {
		return nil, err
	}
	return scope.store.ListLadderTimelineRuns(ctx, scope.accountID, scope.provider, scope.cursor)
}
