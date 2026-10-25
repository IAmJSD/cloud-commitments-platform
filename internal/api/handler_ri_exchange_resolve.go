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
	"unicode/utf8"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

// Operator resolve of a stale processing RI exchange (#692). A run that
// crashed between the provider call and its ledger write leaves the row
// processing, and a processing row holds its reserved ceiling against the
// daily cap on every later day (#657). Only a person who checked AWS can say
// whether the exchange happened, so this endpoint records their decision; the
// store refuses rows that a running writer may still settle.

const (
	riExchangeResolveReasonMin = 10
	riExchangeResolveReasonMax = 300
	riExchangeResolveBodyMax   = 4096
)

var (
	riExchangeResolveAccountRe  = regexp.MustCompile(`^\d{12}$`)
	riExchangeResolveRegionRe   = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	riExchangeResolveExchangeRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)
)

// riExchangeResolveRequest is the body of POST /api/ri-exchange/resolve/{id}.
// outcome "failed" releases the hold and needs acknowledge_unknown_outcome;
// "completed" keeps the reserved amount on today's books and may name the
// exchange id the operator found.
type riExchangeResolveRequest struct {
	Outcome     string                       `json:"outcome"`
	Reason      string                       `json:"reason"`
	ExchangeID  string                       `json:"exchange_id"`
	Acknowledge bool                         `json:"acknowledge_unknown_outcome"`
	CloudCheck  *riExchangeResolveCloudCheck `json:"cloud_check"`
}

type riExchangeResolveCloudCheck struct {
	AccountID string `json:"checked_account_id"`
	Region    string `json:"checked_region"`
	CheckedAt string `json:"checked_at"`
}

type riExchangeResolveResponse struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	PaymentDue string `json:"payment_due"`
	Note       string `json:"note"`
	Replayed   bool   `json:"replayed"`
}

func (h *Handler) resolveRIExchange(ctx context.Context, req *events.LambdaFunctionURLRequest, id string) (any, error) {
	session, err := h.requirePermission(ctx, req, "execute", "ri-exchange")
	if err != nil {
		return nil, err
	}
	// The decision is stamped on the row as transitioned_by, so it needs an
	// identified person, not the stateless admin key.
	if session.UserID == apiKeyAdminUserID {
		return nil, NewClientError(403, "resolving an RI exchange needs a signed-in user or a user API key")
	}
	if idErr := validateUUID(id); idErr != nil {
		return nil, idErr
	}
	// Same deployment-account scope as approvals: out of scope reads as missing.
	if scopeErr := h.requireReshapeAccountVisible(ctx, session); scopeErr != nil {
		return nil, scopeErr
	}
	body, check, err := decodeRIExchangeResolve(req.Body)
	if err != nil {
		return nil, err
	}
	if constraintErr := h.requireRIExchangeResolveConstraints(ctx, session, check.Region); constraintErr != nil {
		return nil, constraintErr
	}
	store, ok := h.config.(config.RIExchangeResolveStore)
	if !ok {
		return nil, NewClientError(503, "RI exchange resolve storage is unavailable")
	}
	res, err := store.ResolveStaleRIExchange(ctx, config.RIExchangeResolve{
		ID: id, Outcome: body.Outcome, ExchangeID: body.ExchangeID, Note: body.note(check),
		Check: check, Actor: resolveCreatorUserID(session), StaleAfter: config.RIExchangeResolveStaleAfter,
	})
	if mapped := mapRIExchangeResolveError(err); mapped != nil {
		return nil, mapped
	}
	if !res.Replayed {
		logging.Warnf("RI exchange resolved by operator: id=%s actor=%q via=%s processing -> %s payment_due=%s note=%q",
			id, session.UserID, credentialKind(ctx, session), res.Status, res.PaymentDue, res.Note)
	}
	return riExchangeResolveResponse{ID: res.ID, Status: res.Status, PaymentDue: res.PaymentDue, Note: res.Note, Replayed: res.Replayed}, nil
}

// requireRIExchangeResolveConstraints applies the Constraints on the granting
// execute:ri-exchange permission, as the direct execute endpoint does. The
// region is the operator's checked_region, which the store requires to equal
// the exchange's own region.
func (h *Handler) requireRIExchangeResolveConstraints(ctx context.Context, session *Session, region string) error {
	cloudAccountID, err := h.resolveReshapeCloudAccountIDOrUnattributed(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve cloud account scope: %w", err)
	}
	return h.requirePermissionConstraints(ctx, session, "execute", "ri-exchange", []auth.PermissionConstraints{{
		AccountIDs: []string{cloudAccountID},
		Providers:  []string{string(common.ProviderAWS)},
		Services:   []string{string(common.ServiceEC2)},
		Regions:    []string{region},
	}})
}

func mapRIExchangeResolveError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, config.ErrRIExchangeResolveNotFound):
		return errNotFound
	case errors.Is(err, config.ErrRIExchangeResolveInvalid):
		return NewClientError(400, err.Error())
	case errors.Is(err, config.ErrRIExchangeResolveConflict):
		return NewClientError(409, err.Error())
	}
	return err
}

func decodeRIExchangeResolve(raw string) (riExchangeResolveRequest, config.RIExchangeCloudCheck, error) {
	var body riExchangeResolveRequest
	if len(raw) > riExchangeResolveBodyMax {
		return body, config.RIExchangeCloudCheck{}, NewClientError(400, "RI exchange resolve body is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return body, config.RIExchangeCloudCheck{}, NewClientError(400, "Invalid RI exchange resolve body")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return body, config.RIExchangeCloudCheck{}, NewClientError(400, "Expected one RI exchange resolve body")
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if err := body.validate(); err != nil {
		return body, config.RIExchangeCloudCheck{}, err
	}
	check, err := body.cloudCheck()
	return body, check, err
}

// validate checks the trimmed body's shape; the store decides the rest.
func (b riExchangeResolveRequest) validate() error {
	switch {
	case b.Outcome != config.StatusFailed && b.Outcome != config.StatusCompleted:
		return NewClientError(400, `outcome must be "failed" or "completed"`)
	case utf8.RuneCountInString(b.Reason) < riExchangeResolveReasonMin || utf8.RuneCountInString(b.Reason) > riExchangeResolveReasonMax:
		return NewClientError(400, fmt.Sprintf("reason must be %d to %d characters", riExchangeResolveReasonMin, riExchangeResolveReasonMax))
	case hasControlRune(b.Reason):
		return NewClientError(400, "reason must not contain control characters")
	case b.CloudCheck == nil:
		return NewClientError(400, "cloud_check is required: say where you verified the exchange at AWS")
	}
	return b.validateOutcomeFields()
}

func (b riExchangeResolveRequest) validateOutcomeFields() error {
	if b.Outcome == config.StatusCompleted {
		if b.ExchangeID != "" && !riExchangeResolveExchangeRe.MatchString(b.ExchangeID) {
			return NewClientError(400, "exchange_id must be a provider exchange id")
		}
		return nil
	}
	if b.ExchangeID != "" {
		return NewClientError(400, "exchange_id means the exchange happened; resolve it as completed")
	}
	if !b.Acknowledge {
		return NewClientError(400, "acknowledge_unknown_outcome is required to release a hold: CUDly cannot see whether the exchange happened")
	}
	return nil
}

// cloudCheck validates the shape of the operator's AWS-side check. Whether it
// names the exchange's own account and region, and postdates its last change,
// is decided by the store under the row lock.
func (b riExchangeResolveRequest) cloudCheck() (config.RIExchangeCloudCheck, error) {
	c := b.CloudCheck
	at, err := time.Parse(time.RFC3339, c.CheckedAt)
	if err != nil {
		return config.RIExchangeCloudCheck{}, NewClientError(400, "checked_at must be an RFC 3339 timestamp")
	}
	if !riExchangeResolveAccountRe.MatchString(c.AccountID) || !riExchangeResolveRegionRe.MatchString(c.Region) {
		return config.RIExchangeCloudCheck{}, NewClientError(400, "checked_account_id must be a 12-digit AWS account id and checked_region an AWS region")
	}
	return config.RIExchangeCloudCheck{AccountID: c.AccountID, Region: c.Region, CheckedAt: at.UTC()}, nil
}

// note is the exact text stored in ri_exchange_history.error. The fields are in
// a fixed order and quoted, so a repeat of the same request produces the same
// string and a different one cannot.
func (b riExchangeResolveRequest) note(check config.RIExchangeCloudCheck) string {
	s := fmt.Sprintf("operator:%s: account=%q region=%q checked_at=%s", b.Outcome, check.AccountID, check.Region, check.CheckedAt.Format(time.RFC3339))
	if b.Outcome == config.StatusFailed {
		s += " ack=true"
	}
	if b.ExchangeID != "" {
		s += fmt.Sprintf(" exchange_id=%q", b.ExchangeID)
	}
	return s + fmt.Sprintf(" reason=%q", b.Reason)
}
