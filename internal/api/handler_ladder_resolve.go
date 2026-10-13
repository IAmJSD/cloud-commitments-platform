package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
)

const (
	ladderResolveReasonMin = 10
	ladderResolveReasonMax = 300
	ladderResolveNoteMax   = 200
	ladderResolveBodyMax   = 4096
)

var (
	ladderResolveAccountRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	ladderResolveRegionRe  = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

// ladderResolveRequest is the body of POST /api/ladder/tranches/{id}/resolve.
// Only outcome "failed" is implemented; "completed" needs the commitment
// visibility adapter and answers 501 until it exists.
type ladderResolveRequest struct {
	Outcome     string                   `json:"outcome"`
	Reason      string                   `json:"reason"`
	Note        string                   `json:"note"`
	Acknowledge bool                     `json:"acknowledge_unknown_outcome"`
	CloudCheck  *ladderResolveCloudCheck `json:"cloud_check"`
}

type ladderResolveCloudCheck struct {
	AccountID string `json:"checked_account_id"`
	Region    string `json:"checked_region"`
	Service   string `json:"checked_service"`
	CheckedAt string `json:"checked_at"`
}

type ladderResolveResponse struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	StatusReason string `json:"status_reason"`
	Replayed     bool   `json:"replayed"`
}

// ladderResolveStore is the part of the store the endpoint needs.
type ladderResolveStore interface {
	LadderTrancheScope(ctx context.Context, id string) (accountID, provider string, err error)
	ResolveLadderTrancheFailed(ctx context.Context, req config.LadderResolveFailed) (*config.LadderResolveResult, error)
}

func (h *Handler) resolveLadderTranche(ctx context.Context, req *events.LambdaFunctionURLRequest, id string) (any, error) {
	session, err := h.requirePermission(ctx, req, auth.ActionExecute, auth.ResourcePurchases)
	if err != nil {
		return nil, err
	}
	if grantErr := h.requireAutoPurchaseGrant(ctx, req, session); grantErr != nil {
		return nil, grantErr
	}
	if _, parseErr := uuid.Parse(id); parseErr != nil {
		return nil, errNotFound
	}
	body, err := decodeLadderResolve(req.Body)
	if err != nil {
		return nil, err
	}
	store, ok := h.config.(ladderResolveStore)
	if !ok {
		return nil, NewClientError(503, "Ladder resolve storage is unavailable")
	}
	accountID, err := h.authorizeLadderResolveScope(ctx, session, store, id)
	if err != nil {
		return nil, err
	}
	if body.Outcome == "completed" {
		return nil, NewClientError(501, "Resolving a tranche as completed is not available yet: it needs the commitment visibility check")
	}
	return h.applyLadderResolveFailed(ctx, session, store, id, accountID, body)
}

// authorizeLadderResolveScope checks the caller's account access and
// execute:purchases scope against the tranche's own account and provider, and
// returns the account id.
func (h *Handler) authorizeLadderResolveScope(ctx context.Context, session *Session, store ladderResolveStore, id string) (string, error) {
	accountID, provider, err := store.LadderTrancheScope(ctx, id)
	if errors.Is(err, config.ErrLadderAmendNotFound) {
		return "", errNotFound
	}
	if err != nil {
		return "", err
	}
	if _, err = h.requireAccountAccess(ctx, session, accountID); err != nil {
		return "", err
	}
	constraints := []auth.PermissionConstraints{{AccountIDs: []string{accountID}, Providers: []string{provider}, StrictScope: true}}
	if scopeErr := h.requirePermissionConstraints(ctx, session, auth.ActionExecute, auth.ResourcePurchases, constraints); scopeErr != nil {
		return "", scopeErr
	}
	if common.ProviderType(provider) != common.ProviderAWS {
		return "", NewClientError(501, "Ladder execution is currently available only for AWS accounts")
	}
	return accountID, nil
}

func (h *Handler) applyLadderResolveFailed(ctx context.Context, session *Session, store ladderResolveStore, id, accountID string, body ladderResolveRequest) (any, error) {
	check, err := body.cloudCheck()
	if err != nil {
		return nil, err
	}
	res, err := store.ResolveLadderTrancheFailed(ctx, config.LadderResolveFailed{
		TrancheID: id, AccountID: accountID, StatusReason: body.statusReason(check),
		Acknowledge: body.Acknowledge, Check: check, Now: time.Now().UTC(), StaleAfter: purchase.LadderFiredStaleAfter,
	})
	switch {
	case errors.Is(err, config.ErrLadderResolveNotFound):
		return nil, errNotFound
	case errors.Is(err, config.ErrLadderResolveInvalid):
		return nil, NewClientError(400, err.Error())
	case errors.Is(err, config.ErrLadderResolveConflict):
		return nil, NewClientError(409, err.Error())
	case err != nil:
		return nil, err
	}
	if !res.Replayed {
		logging.Infof("ladder tranche resolved by operator: tranche=%s actor=%q via=%s fired -> failed acknowledged_unknown_outcome=%t status_reason=%q",
			id, session.UserID, credentialKind(ctx, session), body.Acknowledge, res.StatusReason)
	}
	return ladderResolveResponse{ID: res.ID, Status: res.Status, StatusReason: res.StatusReason, Replayed: res.Replayed}, nil
}

func decodeLadderResolve(raw string) (ladderResolveRequest, error) {
	var body ladderResolveRequest
	if len(raw) > ladderResolveBodyMax {
		return body, NewClientError(400, "Ladder resolve body is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return body, NewClientError(400, "Invalid ladder resolve body")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return body, NewClientError(400, "Expected one ladder resolve body")
	}
	body.Reason, body.Note = strings.TrimSpace(body.Reason), strings.TrimSpace(body.Note)
	if err := body.validate(); err != nil {
		return body, err
	}
	return body, nil
}

// validate checks the trimmed body's shape; the store decides the rest.
func (body ladderResolveRequest) validate() error {
	if body.Outcome != "failed" && body.Outcome != "completed" {
		return NewClientError(400, `outcome must be "failed" or "completed"`)
	}
	if n := utf8.RuneCountInString(body.Reason); n < ladderResolveReasonMin || n > ladderResolveReasonMax {
		return NewClientError(400, fmt.Sprintf("reason must be %d to %d characters", ladderResolveReasonMin, ladderResolveReasonMax))
	}
	if utf8.RuneCountInString(body.Note) > ladderResolveNoteMax {
		return NewClientError(400, fmt.Sprintf("note must be at most %d characters", ladderResolveNoteMax))
	}
	if hasControlRune(body.Reason) || hasControlRune(body.Note) {
		return NewClientError(400, "reason and note must not contain control characters")
	}
	if (body.CloudCheck != nil) != body.Acknowledge {
		return NewClientError(400, "acknowledge_unknown_outcome and cloud_check must be sent together")
	}
	return nil
}

func hasControlRune(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == unicode.ReplacementChar }) >= 0
}

// cloudCheck validates the shape of the operator's cloud-side check. Whether it
// names the tranche's own account and service, and postdates the chain, is
// decided by the store under the chain lock.
func (b ladderResolveRequest) cloudCheck() (*config.LadderCloudCheck, error) {
	c := b.CloudCheck
	if c == nil {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, c.CheckedAt)
	if err != nil {
		return nil, NewClientError(400, "checked_at must be an RFC 3339 timestamp")
	}
	if !ladderResolveAccountRe.MatchString(c.AccountID) || !ladderResolveRegionRe.MatchString(c.Region) || !ladderResolveRegionRe.MatchString(c.Service) {
		return nil, NewClientError(400, "checked_account_id, checked_region and checked_service must be short identifiers")
	}
	return &config.LadderCloudCheck{AccountID: c.AccountID, Region: c.Region, Service: c.Service, CheckedAt: at.UTC()}, nil
}

// statusReason is the exact string stored in ladder_tranches.status_reason. The
// fields are in a fixed order and quoted, so a repeat of the same request
// produces the same string and a different one cannot.
func (b ladderResolveRequest) statusReason(check *config.LadderCloudCheck) string {
	var s string
	if check == nil {
		s = fmt.Sprintf("operator:failed:proven: reason=%q", b.Reason)
	} else {
		s = fmt.Sprintf("operator:failed:unverified: ack=true account=%q region=%q service=%q checked_at=%s reason=%q",
			check.AccountID, check.Region, check.Service, check.CheckedAt.Format(time.RFC3339), b.Reason)
	}
	if b.Note != "" {
		s += fmt.Sprintf(" note=%q", b.Note)
	}
	return s
}
