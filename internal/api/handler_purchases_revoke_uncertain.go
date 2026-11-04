package api

// handler_purchases_revoke_uncertain.go holds the two guards issue #1529 adds
// around the Azure Return in the revoke handler:
//
//   - a Return that fails without a definite answer from Azure keeps the
//     in-flight flag and tells the user the outcome is uncertain, as the RI
//     exchange handler does for its ambiguous provider failures;
//   - on Lambda, the claim is refused when too little invocation time is left
//     to post the Return and record its result.

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	armreservations "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/reservations/armreservations"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// revokeResponseMargin is the invocation time kept back, past the detached
// Return and the write that follows it, for building and returning the
// response. Nothing after that write talks to Azure or the database, so a few
// seconds is plenty; the 30s lambdatimeout.Margin also has to cover the work
// before the claim, which by the time of this check has already run.
const revokeResponseMargin = 5 * time.Second

// revokeClaimMinRemaining is the least invocation time a Lambda revoke must
// have left to take the in-flight claim (issue #1529). Past the claim the
// handler runs the detached Return (revokeReturnTimeout), then exactly one of
// the writes recording an accepted Return or the in-flight clear after a
// refused one (each bounded by revokePersistTimeout; an uncertain failure
// writes nothing), then the response. Lambda ends the invocation at its
// timeout whatever a context says, so a claim taken with less time left could
// be cut off mid-Return or before the result is recorded. 3m35s with the
// current budgets.
const revokeClaimMinRemaining = revokeReturnTimeout + revokePersistTimeout + revokeResponseMargin

// errRevokeTooLittleTime is the message for a claim refused for lack of time.
const errRevokeTooLittleTime = "not enough time was left in this request to return the reservation safely; " +
	"nothing was sent to Azure and the purchase is unchanged, so retry the revoke"

// errRevokeReturnUncertain mirrors the RI exchange handler's wording for an
// ambiguous provider failure (executeAzureExchange).
const errRevokeReturnUncertain = "Azure reservation return failed and may already have been accepted by Azure; " +
	"verify the reservation state in the Azure portal before retrying"

// checkRevokeTimeLeft refuses the claim on Lambda when the invocation has less
// than revokeClaimMinRemaining left. It runs before the claim, so a refusal
// leaves no flag behind and posts no Return; 503 says the request can simply
// be retried.
//
// Only Lambda needs it: elsewhere the process outlives the request, so the
// detached Return and writes finish after the request's 30s deadline
// (internal/server/http.go), and that deadline already guards the claim
// itself, which runs on the request context.
func (h *Handler) checkRevokeTimeLeft(ctx context.Context, purchaseID string) error {
	if !h.isLambda {
		return nil
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	if left := time.Until(deadline); left < revokeClaimMinRemaining {
		logging.Warnf("revoke azure: purchase_id=%s refused before the claim: %s of invocation time left, need %s (no Return posted)",
			purchaseID, left.Round(time.Millisecond), revokeClaimMinRemaining)
		return NewClientError(http.StatusServiceUnavailable, errRevokeTooLittleTime)
	}
	return nil
}

// returnAttemptsKey carries a *atomic.Int32 that returnAttemptPolicy bumps once
// per HTTP request the Return client sends, retries included.
type returnAttemptsKey struct{}

// withReturnAttemptCounter returns ctx carrying a fresh send counter.
func withReturnAttemptCounter(ctx context.Context) (context.Context, *atomic.Int32) {
	n := new(atomic.Int32)
	return context.WithValue(ctx, returnAttemptsKey{}, n), n
}

// returnAttemptPolicy counts the sends of a Return. azcore retries a POST on
// transport errors and on 408, 429 and 5xx, so a 4xx that ends the call may
// answer a retry of a try Azure already accepted (a connection reset after
// the 202, say). Only a 4xx from the one and only send is a definite refusal.
// It sits among the per-retry policies, after the bearer-token policy, so it
// counts only requests that reach the transport.
type returnAttemptPolicy struct{}

func (returnAttemptPolicy) Do(req *policy.Request) (*http.Response, error) {
	if n, ok := req.Raw().Context().Value(returnAttemptsKey{}).(*atomic.Int32); ok {
		n.Add(1)
	}
	return req.Next()
}

// newAzureReturnClient builds the armreservations Return client with
// returnAttemptPolicy installed. opts may be nil; it is not modified.
func newAzureReturnClient(cred azcore.TokenCredential, opts *arm.ClientOptions) (*armreservations.ReturnClient, error) {
	o := arm.ClientOptions{}
	if opts != nil {
		o = *opts
	}
	o.PerRetryPolicies = append(append([]policy.Policy(nil), o.PerRetryPolicies...), returnAttemptPolicy{})
	return armreservations.NewReturnClient(cred, &o)
}

// isAzureDefiniteRefusal reports whether a failed Return is known not to have
// been accepted: Azure answered the only request sent with a 4xx other than
// 408 Request Timeout. Everything else (a transport error, a 5xx, the
// revokeReturnTimeout deadline, a response that could not be read, a 4xx
// after a retry, or any status other than the 202 the SDK expects) leaves
// open whether Azure took the Return.
//
// attempts is the send count from returnAttemptPolicy. A client built without
// the policy reports 0 and is judged on the status alone.
func isAzureDefiniteRefusal(err error, attempts int32) bool {
	if attempts > 1 {
		return false
	}
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		return false
	}
	code := respErr.StatusCode
	return code >= 400 && code < 500 && code != http.StatusRequestTimeout
}

// azureReturnUncertain handles a Return that failed without a definite answer
// (issue #1529). The in-flight flag stays set, so no request can post a
// second Return for this purchase, and the user is told to check the Azure
// portal. If no Return gets recorded, the finalize_revocations sweep clears
// the flag after its 2h stale window and logs the row for an operator.
func azureReturnUncertain(record *config.PurchaseHistoryRecord, attempts int32, err error) error {
	logging.Errorf("revoke azure: purchase_id=%s account_id=%s Return failed without a definite answer after %d send(s), outcome uncertain: %v. "+
		"Kept the in-flight flag so no second Return is posted; finalize_revocations releases it after 2h if no Return is recorded. "+
		"If Azure did accept the Return, reconcile the row by hand",
		record.PurchaseID, record.AccountID, attempts, err)
	return NewClientError(http.StatusInternalServerError, errRevokeReturnUncertain)
}
