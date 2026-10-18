package api

// handler_purchases_revoke.go implements POST /api/purchases/{purchaseId}/revoke
// which lets a session-authenticated user revoke a completed purchase while it
// is still within the provider's free-cancel window (issue #290).
//
// Per-provider support:
//
//   - Azure reservations: return via armreservations.ReturnClient within a 7-day
//     window. The button is shown in the History UI for Azure rows inside the
//     window. Requires CalculateRefund first (to get the session ID) then Return.
//
//   - AWS EC2 RIs / Savings Plans: AWS does not expose a direct cancel API for
//     purchased RIs. Revocation requires an AWS Support case
//     (support:CreateCase). That flow is deferred to Phase 2 (#291). For now the
//     endpoint returns 422 and the frontend hides the button for AWS rows, per
//     the constraint: "if a provider has no cancel API, the button must be hidden".
//
//   - GCP commitments: no free-cancel window. Button hidden for GCP rows.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	armreservations "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/reservations/armreservations"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/jackc/pgx/v5"
)

// revokeQuoteEpsilon is the tolerance (in currency units) for the
// TOCTOU-divergence check: if Azure's actual refund on Return diverges from
// the user-consented expected_refund_amount by more than this, the revoke is
// rejected with 422 so the user can re-quote and confirm.
const revokeQuoteEpsilon = 0.01

// azureRefundSafetyMargin is subtracted from the local window-close time before
// presenting the revoke button or accepting a revoke request. Azure's 7-day
// window has a hard edge: a reservation returned in the last few minutes of the
// window occasionally fails with RefundPolicyViolated due to clock skew between
// CUDly's clock and Azure's. Shrinking the local window by 1 hour eliminates
// the tail of clock-skew failures at the edge (issue #290 Finding #3).
//
// The safety margin applies only to the local pre-flight check -- the value
// stored in purchase_history.revocation_window_closes_at is the unmodified
// Azure deadline so operators can see the true expiry.
const azureRefundSafetyMargin = 1 * time.Hour

// revokeQuoteResult is the JSON body returned by
// GET /api/purchases/revoke/calculate/{id}.
type revokeQuoteResult struct {
	RefundCurrency string  `json:"refund_currency"`
	QuotedAt       string  `json:"quoted_at"`
	RefundAmount   float64 `json:"refund_amount"`
}

// revokeConfirmBody is the JSON body expected on
// POST /api/purchases/{purchaseId}/revoke.
// ExpectedRefundAmount and ExpectedRefundCurrency are the quote the user
// consented to (from GET /revoke/calculate), used for TOCTOU-divergence
// detection. Both are required for Azure revocations.
type revokeConfirmBody struct {
	ExpectedRefundAmount   *float64 `json:"expected_refund_amount"`
	ExpectedRefundCurrency string   `json:"expected_refund_currency"`
}

// AzureRevocationWindowDays is the number of days after purchase within which
// Azure reservations are eligible for a return (refund). Per Azure docs:
// https://learn.microsoft.com/azure/cost-management-billing/reservations/exchange-and-refund-azure-reservations
// Aliases config.AzureRevocationWindowDays so the purchase-write path and this
// endpoint share a single source of truth for the window length.
const AzureRevocationWindowDays = config.AzureRevocationWindowDays

// azureReturnClient is the narrow interface over armreservations.ReturnClient
// used by the revoke handler. Extracted for test injection.
type azureReturnClient interface {
	Post(ctx context.Context, reservationOrderID string, body armreservations.RefundRequest, options *armreservations.ReturnClientPostOptions) (armreservations.ReturnClientPostResponse, error)
}

// azureCalculateRefundClient is the narrow interface over
// armreservations.CalculateRefundClient used to obtain the session ID required
// before calling ReturnClient.Post.
type azureCalculateRefundClient interface {
	Post(ctx context.Context, reservationOrderID string, body armreservations.CalculateRefundRequest, options *armreservations.CalculateRefundClientPostOptions) (armreservations.CalculateRefundClientPostResponse, error)
}

type azureRevokeClientFactory struct {
	newCredential            func() (azcore.TokenCredential, error)
	newCalculateRefundClient func(azcore.TokenCredential) (azureCalculateRefundClient, error)
	newReturnClient          func(azcore.TokenCredential) (azureReturnClient, error)
}

// revokePurchaseResult is the JSON body returned on a successful revocation.
type revokePurchaseResult struct {
	Status     string `json:"status"`
	RevokedAt  string `json:"revoked_at"`
	RevokedVia string `json:"revoked_via"`
}

// revokeReconcilePendingResult is the JSON body returned with HTTP 207
// Multi-Status when the Azure refund succeeded but the subsequent DB write
// failed after all retries. The frontend reads the "code" field and shows a
// non-retryable toast ("Refund issued. We will reconcile your audit shortly.")
// with no retry button (issue #290 Finding #6).
type revokeReconcilePendingResult struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	AzureReturned bool   `json:"azure_returned"`
}

// revokeMarkRetryBackoffs are the sleep durations between consecutive
// MarkPurchaseRevoked attempts after the first failure (1s, 3s, 9s).
var revokeMarkRetryBackoffs = []time.Duration{
	1 * time.Second,
	3 * time.Second,
	9 * time.Second,
}

// revokePurchase handles POST /api/purchases/{purchaseId}/revoke.
//
// Authorization: session required + revoke-own:purchases (or revoke-any for
// admins). The handler is fail-closed: if the auth service is nil the request
// is rejected with 403.
//
// Gmail-style pre-fire delay (issue #291 wave-2): when the ID resolves to a
// purchase_execution in status="scheduled" (cloud SDK not yet called), the
// execution is canceled at zero cost and control returns immediately — no
// provider SDK call is made. This path handles AWS, GCP, and Azure uniformly
// since nothing has been committed to any cloud yet.
func (h *Handler) revokePurchase(ctx context.Context, req *events.LambdaFunctionURLRequest, purchaseID string) (any, error) {
	if purchaseID == "" {
		return nil, NewClientError(400, "purchase_id is required")
	}

	// Fail-closed: auth service nil means we cannot verify permissions.
	// Check before requireSession so this always surfaces as a 403 ClientError.
	if h.auth == nil {
		return nil, NewClientError(403, "authentication service not configured")
	}

	session, err := h.requireSession(ctx, req)
	if err != nil {
		return nil, err
	}

	// Gmail-style pre-fire delay: if the ID resolves to a purchase_execution
	// (cloud SDK not yet called), attempt to cancel it for free at the execution
	// layer. The status=="scheduled" check is intentionally omitted here: reading
	// "scheduled" then checking the status is a TOCTOU race. Instead we pass the
	// row straight to revokeScheduledExecution which calls CancelScheduledExecutionAtomic
	// (WHERE status='scheduled' CAS) and returns 410 if the scheduler already fired.
	//
	// A genuine DB error from GetExecutionByID surfaces as 500; ErrNotFound means
	// the ID is not an execution (or is not yet visible) and we fall through to
	// the purchase_history lookup below.
	execution, execErr := h.config.GetExecutionByID(ctx, purchaseID)
	if execErr != nil && !errors.Is(execErr, config.ErrNotFound) {
		return nil, fmt.Errorf("revoke: GetExecutionByID %s: %w", purchaseID, execErr)
	}
	if execErr == nil {
		return h.revokeScheduledExecution(ctx, session, execution)
	}

	return h.loadAndRevokePurchaseHistory(ctx, req, session, purchaseID)
}

// loadAndRevokePurchaseHistory pulls the auth + idempotency-check + provider-dispatch
// logic for the completed-purchase path out of revokePurchase to keep that function
// under the cyclomatic limit.
func (h *Handler) loadAndRevokePurchaseHistory(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, purchaseID string) (any, error) {
	record, err := h.config.GetPurchaseHistoryByPurchaseID(ctx, purchaseID)
	if err != nil {
		return nil, fmt.Errorf("revoke: load purchase %s: %w", purchaseID, err)
	}
	if record == nil {
		return nil, NewClientError(404, "purchase not found")
	}

	if err := h.authorizeSessionRevoke(ctx, session, record); err != nil {
		return nil, err
	}

	// Idempotency: already revoked.
	if record.RevokedAt != nil {
		return &revokePurchaseResult{
			Status:     "already_revoked",
			RevokedAt:  record.RevokedAt.Format(time.RFC3339),
			RevokedVia: record.RevokedVia,
		}, nil
	}

	// Partial-success reconciliation (issue #290 Finding #6): if the
	// revocation_in_flight flag is set but revoked_at is still NULL, Azure
	// already issued the refund but our DB write failed. Return 207 so the
	// frontend does not retry the Azure call (which would fail with "already
	// returned"). The finalize_revocations sweep will reconcile the row.
	if record.RevocationInFlight {
		return &revokeReconcilePendingResult{
			Code:          "RECONCILE_PENDING",
			AzureReturned: true,
			Message:       "Refund already issued. We will reconcile your audit record shortly. Do not retry.",
		}, nil
	}

	// Parse the confirmed quote from the request body. Azure revocations
	// require it for TOCTOU-divergence detection (the two-step
	// quote-then-confirm flow, issue #290 Finding #4); callAzureReturn enforces it.
	var body revokeConfirmBody
	if req.Body != "" {
		if jsonErr := json.Unmarshal([]byte(req.Body), &body); jsonErr != nil {
			return nil, NewClientError(400, fmt.Sprintf("invalid request body: %v", jsonErr))
		}
	}

	return h.dispatchProviderRevoke(ctx, record, body)
}

// revokeScheduledExecution cancels a Gmail-style pre-fire delayed execution
// that is still in the "scheduled" state (i.e. the cloud SDK has not been
// called yet). This is a free cancel: no provider SDK call is made.
//
// The method enforces revoke-any/revoke-own RBAC (same permissions as the
// completed-purchase revoke path), then atomically transitions the execution
// to "canceled" and removes its purchase_suppressions.
//
// Returns 410 Gone only when the CAS observes the row already transitioned out
// of "scheduled" (the scheduler fired the SDK call between our SELECT and the
// CAS UPDATE). We do NOT pre-reject on a past ScheduledExecutionAt: a row still
// in "scheduled" is cancellable for free no matter how stale the timestamp,
// which keeps free-cancel working during scheduler lag/backpressure.
func (h *Handler) revokeScheduledExecution(ctx context.Context, session *Session, execution *config.PurchaseExecution) (any, error) {
	// No early window-expiry check on ScheduledExecutionAt: a row that is still
	// status=="scheduled" has NOT been transitioned by the scheduler, so the SDK
	// call has not fired regardless of how far the timestamp is in the past
	// (scheduler lag / backpressure). Returning 410 purely on a past timestamp
	// would break free-cancel during lag even though the CAS below can still
	// cancel it before any cloud call. Let CancelScheduledExecutionAtomic be the
	// sole arbiter: it returns canceled=false (-> 410) only when the row has
	// actually moved out of "scheduled".
	// Account-scope gate (issue #92), ahead of RBAC so an out-of-scope caller
	// gets the enumeration-safe 404 rather than a 403 that confirms the row.
	if err := h.requireExecutionAccess(ctx, session, execution.ExecutionID); err != nil {
		return nil, err
	}
	if err := h.authorizeSessionRevokeExecution(ctx, session, execution); err != nil {
		return nil, err
	}

	// Atomically transition from scheduled -> canceled and remove suppressions.
	var cancelledBy *string
	if session.Email != "" {
		e := session.Email
		cancelledBy = &e
	}
	var canceled bool
	var currentStatus string
	if err := h.config.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		// The scheduled-revoke path uses its own CAS variant that flips ONLY
		// status='scheduled' -> 'canceled'. CancelExecutionAtomic accepts
		// only ('pending','notified') and would always return zero rows on
		// a scheduled row, miscoded as "race lost" -> a misleading 410 even
		// during the happy path. Issue #290 wave-2: keep the two CAS contracts
		// distinct so 410 unambiguously means "scheduler already fired".
		canceled, currentStatus, err = h.config.CancelScheduledExecutionAtomic(ctx, tx, execution.ExecutionID, cancelledBy)
		if err != nil {
			return err
		}
		if !canceled {
			return nil
		}
		return h.config.DeleteSuppressionsByExecutionTx(ctx, tx, execution.ExecutionID)
	}); err != nil {
		return nil, fmt.Errorf("cancel scheduled execution %s: %w", execution.ExecutionID, err)
	}
	if !canceled {
		// A concurrent scheduler tick transitioned the row away from "scheduled"
		// between our SELECT and the CAS UPDATE — the window closed. Return 410
		// so the client knows to switch to the completed-purchase revoke path.
		return nil, NewClientError(410, fmt.Sprintf(
			"revocation window has closed: execution %s was already transitioned to %q", execution.ExecutionID, currentStatus,
		))
	}

	logging.Infof("revokeScheduledExecution: execution_id=%s canceled before SDK call (free cancel)", execution.ExecutionID)

	return map[string]string{
		"status":  "canceled",
		"message": "Purchase canceled. No cloud API call was made; no cost incurred.",
	}, nil
}

// authorizeSessionRevokeExecution enforces the revoke-any / revoke-own RBAC
// matrix for scheduled executions (pre-SDK-call state). Mirrors
// authorizeSessionRevoke for completed purchases but operates on a
// PurchaseExecution (which has CreatedByUserID) rather than a
// PurchaseHistoryRecord (which has CloudAccountID). Each verb is also checked
// against the Constraints of the permission that grants it; a constrained
// revoke-any falls back to revoke-own when the creator matches.
func (h *Handler) authorizeSessionRevokeExecution(ctx context.Context, session *Session, execution *config.PurchaseExecution) error {
	if session.UserID == apiKeyAdminUserID {
		return nil
	}

	hasAny, err := h.sessionHoldsVerb(ctx, session, auth.ActionRevokeAny)
	if err != nil {
		return err
	}
	var anyErr error
	if hasAny {
		anyErr = h.requirePermissionConstraints(ctx, session, auth.ActionRevokeAny, auth.ResourcePurchases, revokeExecutionConstraintSets(execution))
		if anyErr == nil || !isForbidden(anyErr) {
			return anyErr
		}
	}

	hasOwn, err := h.sessionHoldsVerb(ctx, session, auth.ActionRevokeOwn)
	if err != nil {
		return err
	}
	if !hasOwn {
		if anyErr != nil {
			return anyErr
		}
		return NewClientError(403, "permission denied: requires revoke-any or revoke-own on purchases")
	}

	return h.authorizeRevokeOwnExecution(ctx, session, execution)
}

// authorizeRevokeOwnExecution requires the execution to have been created by
// this user, then checks revoke-own Constraints. NULL CreatedByUserID means a
// non-human or legacy creator: deny rather than allow an unscoped revoke
// (fail-closed).
func (h *Handler) authorizeRevokeOwnExecution(ctx context.Context, session *Session, execution *config.PurchaseExecution) error {
	if execution.CreatedByUserID == nil || *execution.CreatedByUserID != session.UserID {
		return NewClientError(403, "permission denied: cannot revoke another user's scheduled purchase")
	}
	return h.requirePermissionConstraints(ctx, session, auth.ActionRevokeOwn, auth.ResourcePurchases, revokeExecutionConstraintSets(execution))
}

// revokeConstraintSet is the strict request-side constraint set for a revoke:
// a dimension the stored row does not know is left empty, which a constrained
// grant on that dimension refuses. No MaxPurchaseAmount: a revoke is a refund,
// not a spend.
func revokeConstraintSet(accountID, provider, service, region string) auth.PermissionConstraints {
	return auth.PermissionConstraints{
		StrictScope: true, AccountIDs: knownScopeValue(accountID), Providers: knownScopeValue(provider),
		Services: knownScopeValue(service), Regions: knownScopeValue(region),
	}
}

// revokeExecutionConstraintSets builds one set per recommendation. An
// execution with none gets a single empty strict set so a constrained grant
// still denies instead of the empty slice failing the permission check loudly.
// The session cancel paths (issue #678) reuse it.
func revokeExecutionConstraintSets(execution *config.PurchaseExecution) []auth.PermissionConstraints {
	if len(execution.Recommendations) == 0 {
		return []auth.PermissionConstraints{revokeConstraintSet("", "", "", "")}
	}
	sets := make([]auth.PermissionConstraints, 0, len(execution.Recommendations))
	for i := range execution.Recommendations {
		rec := &execution.Recommendations[i]
		sets = append(sets, revokeConstraintSet(derefString(rec.CloudAccountID), rec.Provider, rec.Service, rec.Region))
	}
	return sets
}

func isForbidden(err error) bool {
	ce, ok := IsClientError(err)
	return ok && ce.code == 403
}

// dispatchProviderRevoke routes a revocation request to the correct
// provider-specific implementation. Extracted from revokePurchase to keep
// that function's cyclomatic complexity within the project limit.
func (h *Handler) dispatchProviderRevoke(ctx context.Context, record *config.PurchaseHistoryRecord, confirmed revokeConfirmBody) (any, error) {
	switch record.Provider {
	case "azure":
		return h.revokeAzurePurchase(ctx, record, confirmed)
	case "aws":
		// AWS does not expose a direct RI cancel API. Phase 2 (#291) adds the
		// AWS Support case path. Return 422 so the frontend hides this button.
		return nil, NewClientError(422, "AWS RIs cannot be revoked via direct API; contact AWS Support for a refund within 24h of purchase")
	case "gcp":
		return nil, NewClientError(422, "GCP commitments do not have a free-cancel window")
	default:
		return nil, NewClientError(422, fmt.Sprintf("provider %q does not support in-app revocation", record.Provider))
	}
}

// authorizeSessionRevoke enforces the revoke-any / revoke-own RBAC matrix, then
// the Constraints of the granting permission against the stored record. A
// revoke-any that fails on account access or constraints falls back to
// revoke-own, as requireSessionPurchaseAction does.
// Mirror of authorizeSessionCancel / authorizeSessionApprove patterns.
func (h *Handler) authorizeSessionRevoke(ctx context.Context, session *Session, record *config.PurchaseHistoryRecord) error {
	// The stateless admin API key has full access and no user row to resolve
	// permissions from. Administrators-group users fall through and pass via
	// the revoke-any check below, since {admin, *} matches
	// any requested permission.
	if session.UserID == apiKeyAdminUserID {
		return nil
	}

	hasAny, err := h.sessionHoldsVerb(ctx, session, auth.ActionRevokeAny)
	if err != nil {
		return err
	}
	var anyErr error
	if hasAny {
		anyErr = h.authorizeRevokeVerb(ctx, session, record, auth.ActionRevokeAny)
		if anyErr == nil || !isForbidden(anyErr) {
			return anyErr
		}
	}

	hasOwn, err := h.sessionHoldsVerb(ctx, session, auth.ActionRevokeOwn)
	if err != nil {
		return err
	}
	if !hasOwn {
		if anyErr != nil {
			return anyErr
		}
		return NewClientError(403, "permission denied: requires revoke-any or revoke-own on purchases")
	}
	return h.authorizeRevokeVerb(ctx, session, record, auth.ActionRevokeOwn)
}

// authorizeRevokeVerb applies account access and permission Constraints for
// one held verb. Values come from the stored record, never the request body.
func (h *Handler) authorizeRevokeVerb(ctx context.Context, session *Session, record *config.PurchaseHistoryRecord, verb string) error {
	if err := h.checkRevokeAccountAccess(ctx, session, record, verb == auth.ActionRevokeAny); err != nil {
		return err
	}
	return h.requirePermissionConstraints(ctx, session, verb, auth.ResourcePurchases, []auth.PermissionConstraints{
		revokeConstraintSet(derefString(record.CloudAccountID), record.Provider, record.Service, record.Region),
	})
}

// checkRevokeAccountAccess requires the purchase to be in a cloud account the
// session may access. revoke-any lifts no account scope (issue #386, the #92
// class): only an unrestricted revoke-any caller skips the check.
func (h *Handler) checkRevokeAccountAccess(ctx context.Context, session *Session, record *config.PurchaseHistoryRecord, revokeAny bool) error {
	// Goes through getAccountScope rather than calling GetAllowedAccountsAPI
	// directly: the direct call skipped the admin-API-key and nil-auth
	// branches, and the `len(allowed) > 0 &&` guard read an empty list as
	// unrestricted independently of any producer (issue #1748).
	scope, err := h.getAccountScope(ctx, session)
	if err != nil {
		return fmt.Errorf("account access check failed: %w", err)
	}
	if revokeAny && scope.AllowsAll() {
		return nil
	}
	// Purchase history rows pre-date created_by_user_id, so ownership is via
	// account access (creator scope: issue #950). Unattributed rows fail closed.
	// Match the way History does: a row may carry only the external account
	// id, and allow-lists may name accounts rather than list ids (issue #534).
	// A set CloudAccountID is authoritative; the external id is only a
	// fallback when it is absent, and is resolved within the row's provider
	// because external ids are unique per (provider, external_id) only.
	cloudID := derefString(record.CloudAccountID)
	if cloudID == "" && record.AccountID == "" {
		return NewClientError(403, "permission denied: cannot verify ownership for this purchase")
	}
	accounts, listErr := h.config.ListCloudAccounts(ctx, config.CloudAccountFilter{})
	if cloudID != "" {
		return revokeScopeResult(scope.Allows(cloudID, accountNameByID(accounts, cloudID)))
	}
	name := ""
	if listErr == nil {
		name = accountNameByExternalID(accounts, record.Provider, record.AccountID)
	}
	return revokeScopeResult(scope.Allows(record.AccountID, name))
}

func revokeScopeResult(allowed bool) error {
	if allowed {
		return nil
	}
	return NewClientError(403, "permission denied: purchase is in an account you do not have access to")
}

func accountNameByID(accounts []config.CloudAccount, id string) string {
	for i := range accounts {
		if accounts[i].ID == id {
			return accounts[i].Name
		}
	}
	return ""
}

// accountNameByExternalID returns the name of the single account with this
// provider and external id, or "" when none or more than one matches, so the
// caller cannot name-match an ambiguous account.
func accountNameByExternalID(accounts []config.CloudAccount, provider, externalID string) string {
	if a := uniqueAccountByExternalID(accounts, provider, externalID); a != nil {
		return a.Name
	}
	return ""
}

// uniqueAccountByExternalID returns the single account with this provider and
// external id, or nil when none or more than one matches.
func uniqueAccountByExternalID(accounts []config.CloudAccount, provider, externalID string) *config.CloudAccount {
	var found *config.CloudAccount
	for i := range accounts {
		if accounts[i].Provider != provider || accounts[i].ExternalID != externalID {
			continue
		}
		if found != nil {
			return nil
		}
		found = &accounts[i]
	}
	return found
}

// calculateAzureRevoke handles GET /api/purchases/revoke/calculate/{id}.
// It runs CalculateRefund against Azure and returns the quoted refund amount
// and currency so the frontend can show the user a confirmation modal before
// the destructive POST /revoke call.
//
// This is the first step of the two-step quote-then-confirm revoke UX
// (issue #290 Finding #4). No state is mutated; the result is used by the
// frontend to populate revokeConfirmBody.
func (h *Handler) calculateAzureRevoke(ctx context.Context, req *events.LambdaFunctionURLRequest, purchaseID string) (any, error) {
	_, orderID, reservationID, count, err := h.validateAzureRevokeRequest(ctx, req, purchaseID)
	if err != nil {
		return nil, err
	}

	calcClient, err := h.buildAzureCalculateRefundClient()
	if err != nil {
		return nil, fmt.Errorf("revoke/calculate: %w", err)
	}

	quantity := int32(count) // #nosec G115 -- Azure reservation count bounded by API limits (<<math.MaxInt32) //nolint:gosec
	calcResp, err := calcClient.Post(ctx, orderID, armreservations.CalculateRefundRequest{
		Properties: &armreservations.CalculateRefundRequestProperties{
			ReservationToReturn: &armreservations.ReservationToReturn{
				ReservationID: &reservationID,
				Quantity:      &quantity,
			},
			Scope: toPtr("Reservation"),
		},
	}, nil)
	if err != nil {
		if isAzureClientError(err) {
			return nil, NewClientError(400, fmt.Sprintf("Azure refund calculation rejected: %v", err))
		}
		return nil, fmt.Errorf("revoke/calculate: CalculateRefund failed: %w", err)
	}

	refundAmount, refundCurrency := extractAzureRefundQuote(calcResp)
	if refundAmount == nil || strings.TrimSpace(refundCurrency) == "" {
		return nil, NewClientError(422, "Azure returned no refund amount or currency for this reservation; cannot quote a refund, contact Azure Support to request one")
	}
	return &revokeQuoteResult{
		RefundAmount:   *refundAmount,
		RefundCurrency: refundCurrency,
		QuotedAt:       time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// validateAzureRevokeRequest runs the shared preflight for the Azure
// CalculateRefund endpoint: input + auth + session, load + authorize the
// purchase, enforce provider==azure and the 1h-safety-margin window check, and
// parse the reservation order/ID from the ARM path. Extracted to keep
// calculateAzureRevoke under the cyclomatic-complexity limit. Returns the loaded
// record plus the parsed orderID, reservationID, and commitment count.
func (h *Handler) validateAzureRevokeRequest(ctx context.Context, req *events.LambdaFunctionURLRequest, purchaseID string) (*config.PurchaseHistoryRecord, string, string, int, error) { //nolint:gocritic // unnamedResult: return names would conflict with body locals
	if purchaseID == "" {
		return nil, "", "", 0, NewClientError(400, "purchase_id is required")
	}
	if h.auth == nil {
		return nil, "", "", 0, NewClientError(403, "authentication service not configured")
	}

	session, err := h.requireSession(ctx, req)
	if err != nil {
		return nil, "", "", 0, err
	}

	record, err := h.config.GetPurchaseHistoryByPurchaseID(ctx, purchaseID)
	if err != nil {
		return nil, "", "", 0, fmt.Errorf("revoke/calculate: load purchase %s: %w", purchaseID, err)
	}
	if record == nil {
		return nil, "", "", 0, NewClientError(404, "purchase not found")
	}

	err = h.authorizeSessionRevoke(ctx, session, record)
	if err != nil {
		return nil, "", "", 0, err
	}

	orderID, reservationID, err := azureRevokeWindowAndIDs(record)
	if err != nil {
		return nil, "", "", 0, err
	}
	return record, orderID, reservationID, record.Count, nil
}

// azureRevokeWindowAndIDs enforces provider==azure and the 1h-safety-margin
// window check, then parses the reservation order/ID from the ARM path.
// Extracted from validateAzureRevokeRequest to keep both under the cyclomatic-
// complexity limit. Returns 422 ClientErrors for every reject case.
func azureRevokeWindowAndIDs(record *config.PurchaseHistoryRecord) (string, string, error) { //nolint:gocritic // unnamedResult: return names would conflict with body locals
	if record.Provider != "azure" {
		return "", "", NewClientError(422, fmt.Sprintf("provider %q does not support refund calculation", record.Provider))
	}

	windowClosesAt := record.Timestamp.AddDate(0, 0, AzureRevocationWindowDays)
	if record.RevocationWindowClosesAt != nil {
		windowClosesAt = *record.RevocationWindowClosesAt
	}
	// Apply the 1h safety margin so we stop offering the button before Azure's
	// hard edge (clock-skew protection, issue #290 Finding #3).
	if time.Now().UTC().After(windowClosesAt.Add(-azureRefundSafetyMargin)) {
		return "", "", NewClientError(422, fmt.Sprintf(
			"Azure reservation return window closed at %s (%d days after purchase)",
			windowClosesAt.Format(time.RFC3339), AzureRevocationWindowDays,
		))
	}

	orderID, reservationID, err := parseAzureReservationIDs(record.PurchaseID)
	if err != nil {
		return "", "", NewClientError(422, "cannot determine Azure reservation order ID from purchase record; contact Azure Support to request a refund")
	}
	if orderID == "" || reservationID == "" {
		return "", "", NewClientError(422, "cannot determine Azure reservation ID from purchase record; contact Azure Support to request a refund")
	}
	return orderID, reservationID, nil
}

// extractAzureRefundQuote pulls the refund amount and currency out of a
// CalculateRefund response, guarding every nil pointer in the chain. A missing
// amount is returned as nil and a missing currency as "", never as zero, so
// callers can tell "Azure quoted nothing" from "Azure quoted 0".
func extractAzureRefundQuote(resp armreservations.CalculateRefundClientPostResponse) (*float64, string) { //nolint:gocritic // unnamedResult: return names would conflict with body locals
	if resp.Properties == nil || resp.Properties.BillingRefundAmount == nil {
		return nil, ""
	}
	price := resp.Properties.BillingRefundAmount
	var amount *float64
	if price.Amount != nil {
		v := *price.Amount
		amount = &v
	}
	var currency string
	if price.CurrencyCode != nil {
		currency = *price.CurrencyCode
	}
	return amount, currency
}

// revokeAzurePurchase handles Azure reservation returns via the Azure
// Reservations API (CalculateRefund + Return). The reservation order ID and
// reservation ID are parsed from the purchase_id ARM resource path stored at
// purchase time.
func (h *Handler) revokeAzurePurchase(ctx context.Context, record *config.PurchaseHistoryRecord, confirmed revokeConfirmBody) (any, error) {
	// Prefer the window stamped on the row at purchase time (single source of
	// truth, issue #290). Fall back to recomputing from Timestamp for legacy
	// rows written before the column was populated, so they remain revocable.
	windowClosesAt := record.Timestamp.AddDate(0, 0, AzureRevocationWindowDays)
	if record.RevocationWindowClosesAt != nil {
		windowClosesAt = *record.RevocationWindowClosesAt
	}
	// Apply the 1h safety margin so we stop accepting revoke requests before
	// Azure's hard edge (clock-skew protection, issue #290 Finding #3).
	if time.Now().UTC().After(windowClosesAt.Add(-azureRefundSafetyMargin)) {
		return nil, NewClientError(422, fmt.Sprintf(
			"Azure reservation return window closed at %s (%d days after purchase)",
			windowClosesAt.Format(time.RFC3339), AzureRevocationWindowDays,
		))
	}

	orderID, reservationID, err := parseAzureReservationIDs(record.PurchaseID)
	if err != nil {
		logging.Warnf("revoke azure: cannot parse reservation IDs from purchase_id %q: %v", record.PurchaseID, err)
		return nil, NewClientError(422, "cannot determine Azure reservation order ID from purchase record; contact Azure Support to request a refund")
	}

	calcClient, returnClient, err := h.buildAzureRevokeClients()
	if err != nil {
		return nil, fmt.Errorf("revoke azure: %w", err)
	}

	return h.callAzureReturn(ctx, calcClient, returnClient, record, orderID, reservationID, confirmed)
}

func (h *Handler) getAzureRevokeFactory() azureRevokeClientFactory {
	if h.azureRevokeFactory != nil {
		return *h.azureRevokeFactory
	}
	return azureRevokeClientFactory{
		newCredential: func() (azcore.TokenCredential, error) {
			return azidentity.NewDefaultAzureCredential(nil)
		},
		newCalculateRefundClient: func(cred azcore.TokenCredential) (azureCalculateRefundClient, error) {
			return armreservations.NewCalculateRefundClient(cred, nil)
		},
		newReturnClient: func(cred azcore.TokenCredential) (azureReturnClient, error) {
			return armreservations.NewReturnClient(cred, nil)
		},
	}
}

func (h *Handler) buildAzureCalculateRefundClient() (azureCalculateRefundClient, error) {
	factory := h.getAzureRevokeFactory()
	cred, err := factory.newCredential()
	if err != nil {
		return nil, fmt.Errorf("obtain credential: %w", err)
	}
	calcClient, err := factory.newCalculateRefundClient(cred)
	if err != nil {
		return nil, fmt.Errorf("create calculate-refund client: %w", err)
	}
	return calcClient, nil
}

// buildAzureRevokeClients constructs both clients from one credential because
// the revoke flow calls CalculateRefund immediately before Return.
func (h *Handler) buildAzureRevokeClients() (azureCalculateRefundClient, azureReturnClient, error) {
	factory := h.getAzureRevokeFactory()
	cred, err := factory.newCredential()
	if err != nil {
		return nil, nil, fmt.Errorf("obtain credential: %w", err)
	}
	calcClient, err := factory.newCalculateRefundClient(cred)
	if err != nil {
		return nil, nil, fmt.Errorf("create calculate-refund client: %w", err)
	}
	returnClient, err := factory.newReturnClient(cred)
	if err != nil {
		return nil, nil, fmt.Errorf("create return client: %w", err)
	}
	return calcClient, returnClient, nil
}

// callAzureReturn executes the two-step Azure reservation return:
// CalculateRefund (to get the session ID and quoted amount) followed by Return.
// Extracted from revokeAzurePurchase to allow test injection of the two clients.
//
// confirmed: the quote the user consented to after the quote step
// (GET /revoke/calculate). It is mandatory; see checkConfirmedRefund.
func (h *Handler) callAzureReturn(
	ctx context.Context,
	calcClient azureCalculateRefundClient,
	returnClient azureReturnClient,
	record *config.PurchaseHistoryRecord,
	orderID, reservationID string,
	confirmed revokeConfirmBody,
) (any, error) {
	// Guard against an order-only ARM path (no /reservations/{id} segment),
	// which parseAzureReservationIDs returns with an empty reservationID.
	// Submitting a Return for an empty reservation would either fail opaquely
	// or, worse, be misinterpreted by the API; reject it up front so the
	// caller gets a clear, actionable error instead.
	if orderID == "" || reservationID == "" {
		return nil, NewClientError(422, "cannot determine Azure reservation ID from purchase record; contact Azure Support to request a refund")
	}
	if confirmed.ExpectedRefundAmount == nil || strings.TrimSpace(confirmed.ExpectedRefundCurrency) == "" {
		return nil, NewClientError(400, "expected_refund_amount and expected_refund_currency are required; fetch a quote from GET /api/purchases/{id}/revoke/calculate and confirm it")
	}

	// Step 1: CalculateRefund -> sessionID + quoted amount (TOCTOU check).
	quantity := int32(record.Count) // #nosec G115 -- Azure reservation count validated at purchase; bounded by API limits (<<math.MaxInt32) //nolint:gosec
	sessionID, calcRefundAmount, calcRefundCurrency, err := h.azureCalculateRefund(ctx, calcClient, orderID, reservationID, quantity)
	if err != nil {
		return nil, err
	}

	if quoteErr := checkConfirmedRefund(confirmed, calcRefundAmount, calcRefundCurrency); quoteErr != nil {
		return nil, quoteErr
	}

	// Partial-success guard (issue #290 Finding #6): flip the in-flight flag
	// BEFORE calling Azure Return so that if the subsequent MarkPurchaseRevoked
	// write fails, the row is visible to the finalize_revocations sweep rather
	// than silently stuck. Best-effort: if the flip itself fails, log and
	// continue — the in-flight flag is a safety net, not a hard precondition.
	if flipErr := h.config.FlipPurchaseRevocationInFlight(ctx, record.PurchaseID); flipErr != nil {
		logging.Warnf("revoke azure: FlipPurchaseRevocationInFlight for %s failed (continuing): %v", record.PurchaseID, flipErr)
	}

	// Step 2: Return (post the actual refund request).
	_, err = returnClient.Post(ctx, orderID, armreservations.RefundRequest{
		Properties: &armreservations.RefundRequestProperties{
			ReservationToReturn: &armreservations.ReservationToReturn{
				ReservationID: &reservationID,
				Quantity:      &quantity,
			},
			SessionID:    &sessionID,
			ReturnReason: toPtr("Revoked via CUDly within free-cancel window"),
			Scope:        toPtr("Reservation"),
		},
	}, nil)
	if err != nil {
		return nil, h.handleAzureReturnError(ctx, record, err)
	}

	return h.persistAzureRevocation(ctx, record, calcRefundAmount, calcRefundCurrency)
}

// checkConfirmedRefund is the TOCTOU-divergence check: the refund the user
// confirmed must match Azure's current CalculateRefund quote in currency and,
// within revokeQuoteEpsilon, in amount. A mismatch means the quote changed
// between confirmation and the call (e.g. a fee tier boundary was crossed).
// It fails closed when Azure returns no comparable quote, and never converts
// currencies (there is no trusted FX source).
func checkConfirmedRefund(confirmed revokeConfirmBody, calcRefundAmount *float64, calcRefundCurrency string) error {
	if calcRefundAmount == nil || strings.TrimSpace(calcRefundCurrency) == "" {
		return NewClientError(422, "Azure returned no refund quote to verify against the amount you confirmed; refusing to return the reservation")
	}
	expectedCurrency := strings.TrimSpace(confirmed.ExpectedRefundCurrency)
	if !strings.EqualFold(expectedCurrency, strings.TrimSpace(calcRefundCurrency)) {
		return NewClientError(422, fmt.Sprintf(
			"refund currency diverged: you confirmed %.2f %s but Azure now quotes %.2f %s; re-confirm to proceed",
			*confirmed.ExpectedRefundAmount, expectedCurrency, *calcRefundAmount, calcRefundCurrency,
		))
	}
	if math.Abs(*confirmed.ExpectedRefundAmount-*calcRefundAmount) > revokeQuoteEpsilon {
		return NewClientError(422, fmt.Sprintf(
			"refund amount diverged: you confirmed %.2f but Azure now quotes %.2f %s; re-confirm to proceed",
			*confirmed.ExpectedRefundAmount, *calcRefundAmount, calcRefundCurrency,
		))
	}
	return nil
}

// azureCalculateRefund runs the CalculateRefund step and parses out the session
// ID (required by Return) and the quoted refund amount/currency (for the TOCTOU
// check). Errors are classified into 400 (client) vs 500 (transient).
func (h *Handler) azureCalculateRefund(ctx context.Context, calcClient azureCalculateRefundClient, orderID, reservationID string, quantity int32) (string, *float64, string, error) { //nolint:gocritic // unnamedResult: return names would conflict with body locals
	calcResp, err := calcClient.Post(ctx, orderID, armreservations.CalculateRefundRequest{
		Properties: &armreservations.CalculateRefundRequestProperties{
			ReservationToReturn: &armreservations.ReservationToReturn{
				ReservationID: &reservationID,
				Quantity:      &quantity,
			},
			Scope: toPtr("Reservation"),
		},
	}, nil)
	if err != nil {
		if isAzureClientError(err) {
			return "", nil, "", NewClientError(400, fmt.Sprintf("Azure refund calculation rejected: %v", err))
		}
		return "", nil, "", fmt.Errorf("revoke azure: CalculateRefund failed: %w", err)
	}

	var sessionID string
	if calcResp.Properties != nil && calcResp.Properties.SessionID != nil {
		sessionID = *calcResp.Properties.SessionID
	}
	calcRefundAmount, calcRefundCurrency := extractAzureRefundQuote(calcResp)
	return sessionID, calcRefundAmount, calcRefundCurrency, nil
}

// handleAzureReturnError clears the in-flight flag (no refund was issued) and
// maps the Return error to the right status: 422 on the 7-day window edge, 400
// on other client errors, 500 otherwise.
func (h *Handler) handleAzureReturnError(ctx context.Context, record *config.PurchaseHistoryRecord, err error) error {
	// Azure Return failed. Clear the in-flight flag so the row is not left in
	// a permanently sticky state that would mislead the finalize_revocations
	// sweep into thinking Azure already issued a refund (Finding D, second-wave
	// CR). Best-effort: log and continue even if the clear fails.
	if clearErr := h.config.ClearRevocationInFlight(ctx, record.PurchaseID); clearErr != nil {
		logging.Warnf("revoke azure: ClearRevocationInFlight for %s failed after Return error (continuing): %v", record.PurchaseID, clearErr)
	}
	// Window-edge: if Azure rejects the Return with RefundPolicyViolated it
	// means our safety-margin check passed but Azure's clock disagreed (the
	// reservation crossed the 7-day boundary between our check and the API
	// call). Surface a clean 422 so the frontend can show a user-friendly
	// "window just closed" message (issue #290 Finding #3).
	if isAzureWindowEdgeError(err) {
		return NewClientError(422, "Azure reservation return window has closed; the 7-day refund period has expired")
	}
	if isAzureClientError(err) {
		return NewClientError(400, fmt.Sprintf("Azure refund rejected: %v", err))
	}
	return fmt.Errorf("revoke azure: Return failed: %w", err)
}

// persistAzureRevocation records the successful revocation with exponential-
// backoff retries. If every attempt fails, Azure has already refunded but the
// DB write could not land, so it returns a 207 RECONCILE_PENDING result (no
// retry) for the finalize_revocations sweep to reconcile, rather than a 500.
func (h *Handler) persistAzureRevocation(ctx context.Context, record *config.PurchaseHistoryRecord, calcRefundAmount *float64, calcRefundCurrency string) (any, error) {
	now := time.Now().UTC()
	markErr := h.config.MarkPurchaseRevoked(ctx, record.PurchaseID, now, "direct-api", "", calcRefundAmount, calcRefundCurrency)
	for attempt, backoff := range revokeMarkRetryBackoffs {
		if markErr == nil {
			break
		}
		logging.Warnf("revoke azure: MarkPurchaseRevoked attempt %d failed for %s: %v (retrying in %s)",
			attempt+1, record.PurchaseID, markErr, backoff)
		time.Sleep(backoff)
		markErr = h.config.MarkPurchaseRevoked(ctx, record.PurchaseID, now, "direct-api", "", calcRefundAmount, calcRefundCurrency)
	}
	if markErr != nil {
		// All retries failed. Azure has already refunded but we cannot persist
		// the revocation state. Return 207 Multi-Status so the frontend knows
		// the refund issued but not to retry — the finalize_revocations sweep
		// will reconcile the DB write on its next tick.
		logging.Errorf("revoke azure: MarkPurchaseRevoked failed for %s after %d attempts (Azure already returned): %v",
			record.PurchaseID, len(revokeMarkRetryBackoffs)+1, markErr)
		return &revokeReconcilePendingResult{
			Code:          "RECONCILE_PENDING",
			AzureReturned: true,
			Message:       "Refund issued. We will reconcile your audit record shortly. Do not retry.",
		}, nil
	}

	// PII policy: log execution and account IDs only, not user identifiers.
	logging.Infof("revoke azure: purchase_id=%s account_id=%s revoked_via=direct-api", record.PurchaseID, record.AccountID)

	return &revokePurchaseResult{
		Status:     "revoked",
		RevokedAt:  now.Format(time.RFC3339),
		RevokedVia: "direct-api",
	}, nil
}

// parseAzureReservationIDs extracts the reservation order ID and reservation ID
// from an Azure ARM resource path. The purchase_id is stored as the ARM
// resource ID at purchase time.
//
// Accepted formats (case-insensitive path segments):
//
//	/subscriptions/{sub}/providers/Microsoft.Capacity/reservationOrders/{orderID}/reservations/{resID}
//	/providers/Microsoft.Capacity/reservationOrders/{orderID}/reservations/{resID}
//	/providers/Microsoft.Capacity/reservationOrders/{orderID}
func parseAzureReservationIDs(purchaseID string) (orderID, reservationID string, err error) {
	lower := strings.ToLower(purchaseID)
	const orderKey = "reservationorders/"

	orderIdx := strings.Index(lower, orderKey)
	if orderIdx < 0 {
		return "", "", fmt.Errorf("no reservationOrders segment in %q", purchaseID)
	}
	afterOrder := purchaseID[orderIdx+len(orderKey):]

	resIdx := strings.Index(strings.ToLower(afterOrder), "/reservations/")
	if resIdx < 0 {
		// Order-only path.
		return strings.TrimRight(afterOrder, "/"), "", nil
	}
	orderID = afterOrder[:resIdx]
	reservationID = afterOrder[resIdx+len("/reservations/"):]
	if sl := strings.Index(reservationID, "/"); sl >= 0 {
		reservationID = reservationID[:sl]
	}
	return orderID, reservationID, nil
}

// isAzureClientError reports whether err represents a 4xx (client-side) Azure
// API rejection that the frontend should see as a user-actionable error rather
// than an internal server error.
//
// The check uses typed error inspection (errors.As to *azcore.ResponseError)
// rather than substring matching on err.Error(). The substring approach had two
// failure modes:
//  1. False positives: a network timeout whose message happens to contain "400"
//     or "404" would be misclassified as a client error, hiding transient infra
//     problems from the operator.
//  2. False negatives: Azure may return refund-policy errors with HTTP status
//     codes we did not enumerate as string literals (e.g. 403, 405).
//
// The typed approach classifies exactly the HTTP status codes Azure uses for
// policy violations and bad requests; all other errors (transport errors,
// 5xx, unknown error types) correctly classify as server-side.
func isAzureClientError(err error) bool {
	if err == nil {
		return false
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.StatusCode {
		case 400, 403, 404, 405, 409, 422:
			return true
		}
	}
	return false
}

// isAzureWindowEdgeError reports whether err is an Azure RefundPolicyViolated
// rejection from the Return API. This specific error code is returned when the
// reservation's 7-day return window has closed (either because the request
// arrived just after expiry due to clock skew, or because a partial return
// was already submitted). It is distinct from general client errors because
// the appropriate HTTP response is 422 with code AZURE_WINDOW_EDGE rather
// than the generic 400 "Azure refund rejected" path.
func isAzureWindowEdgeError(err error) bool {
	if err == nil {
		return false
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.ErrorCode == "RefundPolicyViolated"
	}
	return false
}

// toPtr returns a pointer to its argument. Generic helper used by the Azure
// revocation call-site to construct ARM struct fields without temp variables.
func toPtr[T any](v T) *T { return &v }
