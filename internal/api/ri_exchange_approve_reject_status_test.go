package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

const (
	riStatusID       = "550e8400-e29b-41d4-a716-446655440600"
	riStatusRawToken = "raw-email-token"
)

// riStatusRecord selects the stored record: pending with a token, one with no
// token, a missing one, or one whose transition loses the race (409).
type riStatusRecord int

const (
	riRecPending riStatusRecord = iota
	riRecNoToken
	riRecMissing
	riRecAlreadyProcessed
)

// riStatusOpts tweaks the fixture for the less common cases.
type riStatusOpts struct {
	fourEyes    bool // global 4-eyes mode on; the admin session is the creator
	noCreator   bool // with fourEyes: the record has no creator, as every scheduler/ladder row does
	noApprove   bool // the session holds no approve permission
	approveOwn  bool // the session holds approve-own only (not the creator)
	dbError     bool // the record lookup fails
	rateLimited bool // the public approve/cancel rate limit rejects the request
}

func riStatusHandler(rec riStatusRecord, o riStatusOpts) *Handler {
	var record *config.RIExchangeRecord
	if rec != riRecMissing {
		record = &config.RIExchangeRecord{
			ID: riStatusID, Status: "pending", SourceRIIDs: []string{"ri-1"}, PaymentDue: "0",
			ApprovalToken: config.HashApprovalToken(riStatusRawToken),
		}
		if rec == riRecNoToken {
			record.ApprovalToken = ""
		}
		if (o.fourEyes && !o.noCreator) || o.approveOwn {
			creator := "admin-uuid"
			if o.approveOwn {
				creator = "someone-else"
			}
			record.CreatedByUserID = &creator
		}
	}
	store := new(MockConfigStore)
	if o.dbError {
		store.On("GetRIExchangeRecord", mock.Anything, riStatusID).Return(nil, errors.New("db down")).Maybe()
	} else {
		store.On("GetRIExchangeRecord", mock.Anything, riStatusID).Return(record, nil).Maybe()
	}
	var transitioned *config.RIExchangeRecord
	if rec != riRecAlreadyProcessed {
		transitioned = &config.RIExchangeRecord{ID: riStatusID, Status: "processing"}
	}
	store.On("TransitionRIExchangeStatus", mock.Anything, riStatusID, "pending", mock.Anything, mock.Anything).
		Return(transitioned, nil).Maybe()
	store.On("ReserveApprovedRIExchange", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("500.000000", nil).Maybe()
	store.On("GetGlobalConfig", mock.Anything).Return(&config.GlobalConfig{RequireDifferentApprover: o.fourEyes}, nil).Maybe()
	store.On("FailRIExchange", mock.Anything, riStatusID, mock.Anything).Return(nil).Maybe()
	store.On("StampRIExchangeApprovedBy", mock.Anything, riStatusID, mock.Anything).Return(nil).Maybe()

	a := new(MockAuthService)
	a.On("ValidateSession", mock.Anything, "admin-bearer").Return(&Session{UserID: "admin-uuid", Email: "admin@example.com"}, nil).Maybe()
	a.On("ValidateSession", mock.Anything, mock.Anything).Return(nil, errors.New("invalid session")).Maybe()
	switch {
	case o.noApprove:
		grantRINone(a, "admin-uuid")
	case o.approveOwn:
		grantRIApproveOwn(a, "admin-uuid")
	default:
		a.grantAdminPurchaser()
	}
	a.On("ValidateCSRFToken", mock.Anything, "admin-bearer", "csrf-ok").Return(nil).Maybe()
	a.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(errors.New("csrf mismatch")).Maybe()
	h := &Handler{config: store, auth: a}
	if o.rateLimited {
		rl := new(MockRateLimiter)
		rl.On("AllowWithIP", mock.Anything, mock.Anything, "approve_cancel_public").Return(false, nil)
		h.rateLimiter = rl
	}
	return h
}

type riStatusSession int

const (
	riSessNone riStatusSession = iota
	riSessInvalid
	riSessNoCSRF
	riSessCSRF
)

func riStatusRequest(action string, sess riStatusSession, token string) *events.LambdaFunctionURLRequest {
	hdr := map[string]string{}
	switch sess {
	case riSessInvalid:
		hdr["authorization"] = "Bearer bad"
	case riSessNoCSRF:
		hdr["authorization"] = "Bearer admin-bearer"
	case riSessCSRF:
		hdr["authorization"] = "Bearer admin-bearer"
		hdr["x-csrf-token"] = "csrf-ok"
	}
	return &events.LambdaFunctionURLRequest{
		Headers:               hdr,
		QueryStringParameters: map[string]string{"token": token},
		RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
			Method: "POST", Path: "/api/ri-exchange/" + action + "/" + riStatusID}},
	}
}

type riStatusCase struct {
	name      string
	rec       riStatusRecord
	sess      riStatusSession
	token     string
	opts      riStatusOpts
	id        string // overrides riStatusID
	body      string // JSON request body
	want      int
	wantError string // exact "error" body text; empty for 200 cases
	wantBody  string // substring of the body for 200 cases
}

func runRIStatusCases(t *testing.T, action string, cases []riStatusCase) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := riStatusHandler(tc.rec, tc.opts)
			req := riStatusRequest(action, tc.sess, tc.token)
			if tc.id != "" {
				req.RequestContext.HTTP.Path = "/api/ri-exchange/" + action + "/" + tc.id
			}
			if tc.body != "" {
				req.Body = tc.body
				req.Headers["content-type"] = "application/json"
			}
			resp, err := h.HandleRequest(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.StatusCode, resp.Body)
			if tc.wantError != "" {
				var body map[string]string
				require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
				assert.Equal(t, tc.wantError, body["error"])
			}
			if tc.wantBody != "" {
				assert.Contains(t, resp.Body, tc.wantBody)
			}
		})
	}
}

// Observed behavior of POST /api/ri-exchange/approve/{id}; documented in
// openapi.yaml. There is no 401: a request without a usable session fails the
// CSRF check first (403) unless it carries a token.
func TestHandleRequest_ApproveRIExchange_StatusCodes(t *testing.T) {
	runRIStatusCases(t, "approve", []riStatusCase{
		{name: "200 token without session", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, want: 200, wantBody: `"status"`},
		{name: "200 session with CSRF and no token", rec: riRecPending, sess: riSessCSRF, want: 200, wantBody: `"status"`},
		{name: "200 session wins over a wrong token", rec: riRecPending, sess: riSessCSRF, token: "wrong", want: 200, wantBody: `"status"`},
		{name: "403 no session and no token", rec: riRecPending, sess: riSessNone, want: 403, wantError: "CSRF validation failed"},
		{name: "403 invalid session and no token", rec: riRecPending, sess: riSessInvalid, want: 403, wantError: "CSRF validation failed"},
		{name: "403 session without CSRF and no token", rec: riRecPending, sess: riSessNoCSRF, want: 403, wantError: "CSRF validation failed"},
		{name: "403 session without CSRF does not fall through to a valid token", rec: riRecPending, sess: riSessNoCSRF, token: riStatusRawToken, want: 403, wantError: "CSRF validation failed"},
		{name: "403 wrong token without session", rec: riRecPending, sess: riSessNone, token: "wrong", want: 403, wantError: "invalid approval token"},
		{name: "403 record without approval token", rec: riRecNoToken, sess: riSessNone, token: riStatusRawToken, want: 403, wantError: "this exchange record does not support approval"},
		{name: "400 invalid UUID", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, id: "not-a-uuid", want: 400, wantError: "invalid ID format: must be a valid UUID"},
		{name: "403 session without approve right and no token", rec: riRecPending, sess: riSessCSRF, opts: riStatusOpts{noApprove: true}, want: 403, wantError: "permission denied: requires approve-any or approve-own on purchases"},
		{name: "403 4-eyes creator cannot approve own exchange", rec: riRecPending, sess: riSessCSRF, opts: riStatusOpts{fourEyes: true}, want: 403, wantError: "approval declined: 4-eyes mode requires a different approver than the requester"},
		{name: "500 4-eyes on and token without session", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, opts: riStatusOpts{fourEyes: true}, want: 500, wantError: "4-eyes approval mode is enabled but no session could be resolved; sign in before approving"},
		{name: "500 4-eyes on and token without session on a creator-less exchange", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, opts: riStatusOpts{fourEyes: true, noCreator: true}, want: 500, wantError: "4-eyes approval mode is enabled but no session could be resolved; sign in before approving"},
		{name: "200 4-eyes on and session approves a creator-less exchange", rec: riRecPending, sess: riSessCSRF, opts: riStatusOpts{fourEyes: true, noCreator: true}, want: 200, wantBody: `"status"`},
		{name: "200 approve-own session that is not the creator falls through to the token", rec: riRecPending, sess: riSessCSRF, token: riStatusRawToken, opts: riStatusOpts{approveOwn: true}, want: 200, wantBody: `"status"`},
		{name: "403 body token is not read", rec: riRecPending, sess: riSessNone, body: `{"token":"` + riStatusRawToken + `"}`, want: 403, wantError: "CSRF validation failed"},
		{name: "429 rate limited", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, opts: riStatusOpts{rateLimited: true}, want: 429, wantError: "too many requests, please try again later"},
		{name: "500 record lookup fails", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, opts: riStatusOpts{dbError: true}, want: 500},
		{name: "404 missing record with token", rec: riRecMissing, sess: riSessNone, token: riStatusRawToken, want: 404, wantError: "exchange record not found"},
		{name: "404 missing record with wrong token", rec: riRecMissing, sess: riSessNone, token: "wrong", want: 404, wantError: "exchange record not found"},
		{name: "404 missing record with session", rec: riRecMissing, sess: riSessCSRF, want: 404, wantError: "exchange record not found"},
		{name: "409 token approve of an exchange no longer pending", rec: riRecAlreadyProcessed, sess: riSessNone, token: riStatusRawToken, want: 409, wantError: "exchange already processed, expired, or was canceled by a newer analysis run"},
	})
}

// Observed behavior of POST /api/ri-exchange/reject/{id}: token only; a
// session, valid or not, is ignored and no CSRF check runs.
func TestHandleRequest_RejectRIExchange_StatusCodes(t *testing.T) {
	runRIStatusCases(t, "reject", []riStatusCase{
		{name: "200 token without session", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, want: 200, wantBody: `"status":"canceled"`},
		{name: "200 token with session without CSRF", rec: riRecPending, sess: riSessNoCSRF, token: riStatusRawToken, want: 200, wantBody: `"status":"canceled"`},
		{name: "400 no token", rec: riRecPending, sess: riSessNone, want: 400, wantError: "rejection token is required"},
		{name: "400 no token even with a valid session", rec: riRecPending, sess: riSessCSRF, want: 400, wantError: "rejection token is required"},
		{name: "400 invalid UUID", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, id: "not-a-uuid", want: 400, wantError: "invalid ID format: must be a valid UUID"},
		{name: "400 body token is not read", rec: riRecPending, sess: riSessNone, body: `{"token":"` + riStatusRawToken + `"}`, want: 400, wantError: "rejection token is required"},
		{name: "429 rate limited", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, opts: riStatusOpts{rateLimited: true}, want: 429, wantError: "too many requests, please try again later"},
		{name: "500 record lookup fails", rec: riRecPending, sess: riSessNone, token: riStatusRawToken, opts: riStatusOpts{dbError: true}, want: 500},
		{name: "403 wrong token", rec: riRecPending, sess: riSessNone, token: "wrong", want: 403, wantError: "invalid rejection token"},
		{name: "403 record without approval token", rec: riRecNoToken, sess: riSessNone, token: riStatusRawToken, want: 403, wantError: "this exchange record does not support rejection"},
		{name: "404 missing record with token", rec: riRecMissing, sess: riSessNone, token: riStatusRawToken, want: 404, wantError: "exchange record not found"},
		{name: "404 missing record with wrong token", rec: riRecMissing, sess: riSessNone, token: "wrong", want: 404, wantError: "exchange record not found"},
		{name: "409 exchange no longer pending", rec: riRecAlreadyProcessed, sess: riSessNone, token: riStatusRawToken, want: 409, wantError: "exchange already processed, expired, or was canceled"},
	})
}
