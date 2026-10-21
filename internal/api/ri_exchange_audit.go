package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	azurecompute "github.com/LeanerCloud/cloud-commitments-go/providers/azure/services/compute"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/ctxutil"
	"github.com/google/uuid"
)

// Audit trail for RI exchanges executed directly through the API (issue #82).
//
// The scheduled approve/reject flow records every exchange in
// ri_exchange_history; the two direct execute endpoints recorded nothing. Both
// now write an ri_exchange_direct_executions row in two steps:
//
//  1. beginDirectExchange inserts it as "submitted" after every gate and
//     guardrail has passed and BEFORE the submit-time idempotency claim. A
//     failed insert refuses the exchange, so no irreversible exchange runs
//     without a trail, and because the claim has not been taken yet the
//     refusal leaves nothing behind to block an honest retry.
//  2. finalizeDirectExchange records the outcome once the provider call (or
//     the claim) has answered, on a detached context like the approval flow's
//     terminal ledger write (#790), so a client disconnect cannot leave a
//     committed exchange recorded as merely "submitted".

// beginDirectExchange records the audit row for a direct execute, then takes
// the submit-time idempotency claim for claimKey. It returns the audit row's
// ID, which the caller finalizes with the provider's outcome.
//
// A claim refusal (an identical submit inside the window, or a claim store
// failure) finalizes the row as failed: the request never reached the
// provider, and the row records that an identical execute was attempted and
// refused.
func (h *Handler) beginDirectExchange(ctx context.Context, audit *config.RIExchangeDirectExecution, claimKey string) (string, error) {
	if err := audit.Validate(); err != nil {
		return "", fmt.Errorf("invalid RI exchange audit record, exchange not executed: %w", err)
	}
	if err := h.config.SaveRIExchangeDirectExecution(ctx, audit); err != nil {
		logging.Errorf("ri-exchange direct execute: failed to record audit row, refusing to execute: %v", err)
		return "", fmt.Errorf("failed to record the RI exchange audit trail, exchange not executed: %w", err)
	}
	if err := h.claimExchangeSubmit(ctx, claimKey); err != nil {
		h.finalizeDirectExchange(ctx, audit.ID, config.RIExchangeDirectOutcome{
			Status: config.RIExchangeDirectStatusFailed,
			Error:  "not submitted to the provider: " + err.Error(),
		})
		return "", err
	}
	return audit.ID, nil
}

// finalizeDirectExchange records the outcome of a direct execute. A failure is
// logged, not returned: by now the provider has answered, and the caller must
// get that answer. The row then stays "submitted", which is itself the signal
// that the outcome was never recorded.
func (h *Handler) finalizeDirectExchange(ctx context.Context, id string, outcome config.RIExchangeDirectOutcome) {
	saveCtx, cancel := ctxutil.DetachedWithTimeout(ctx, riExchangeTerminalSaveTimeout)
	defer cancel()
	if err := h.config.FinalizeRIExchangeDirectExecution(saveCtx, id, outcome); err != nil {
		logging.Errorf("ri-exchange direct execute %s: failed to record outcome %q in the audit trail: %v", id, outcome.Status, err)
	}
}

// directExchangeFailureStatus classifies a failed provider call for the audit
// trail from the error the handler returns for it: a 4xx means the provider
// rejected the request as a client fault, so the exchange did not happen;
// anything else (timeout, 5xx, transport error) does not prove that, so the
// exchange may have committed and the outcome is recorded as unknown.
func directExchangeFailureStatus(mapped error) string {
	if ce, ok := IsClientError(mapped); ok && ce.code >= 400 && ce.code < 500 {
		return config.RIExchangeDirectStatusFailed
	}
	return config.RIExchangeDirectStatusUnknown
}

// directExchangeActor fills the executed-by fields from the session. The
// user and API key IDs are kept only when they are UUIDs (the stateless admin
// API key session carries a sentinel user ID); executedBy always names the
// actor, falling back to that sentinel, as the 4-eyes comparison does.
func directExchangeActor(audit *config.RIExchangeDirectExecution, session *Session) {
	audit.ExecutedByUserID = resolveCreatorUserID(session)
	audit.ExecutedBy = fourEyesActorIdentity(session)
	if validateUUID(session.UserAPIKeyID) == nil {
		keyID := session.UserAPIKeyID
		audit.ExecutedByAPIKeyID = &keyID
	}
}

// auditJSON marshals v for an audit JSON column. Marshaling the request
// shapes used here cannot fail; if it ever did, the nil result fails
// RIExchangeDirectExecution.Validate and the exchange is refused.
func auditJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// auditDecimal renders r as a plain decimal NUMERIC accepts. big.Rat accepts
// inputs such as "1/3" or "0x10" that a NUMERIC cast would reject, so amounts
// are stored from the parsed value, never the raw request string. A value
// with no finite decimal expansion is rounded to 6 places, the precision of
// the exchange ledger's own DECIMAL(20,6) amounts.
func auditDecimal(r *big.Rat) string {
	prec, exact := r.FloatPrec()
	if !exact {
		prec = 6
	}
	return r.FloatString(prec)
}

// auditDecimalString canonicalizes a provider-reported decimal string, or
// returns nil when it is empty or unparseable.
func auditDecimalString(s string) *string {
	r, err := exchange.ParseDecimalRat(s)
	if err != nil {
		return nil
	}
	d := auditDecimal(r)
	return &d
}

// auditFloat renders a provider-reported float amount, or nil when absent or
// not finite.
func auditFloat(f *float64) *string {
	if f == nil || math.IsNaN(*f) || math.IsInf(*f, 0) {
		return nil
	}
	d := strconv.FormatFloat(*f, 'f', -1, 64)
	return &d
}

// optionalString returns nil for "" so an absent value is stored as NULL.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// auditCloudAccountID maps the resolved cloud account to the audit column:
// the unattributed sentinel means no account is registered, stored as NULL.
func auditCloudAccountID(cloudAccountID string) *string {
	if cloudAccountID == unattributedAccountConstraint {
		return nil
	}
	return optionalString(cloudAccountID)
}

// awsDirectExchangeAudit builds the audit row for a direct AWS execute.
// The legacy target_offering_id/target_count singleton is recorded as a
// one-element targets list, the shape it is executed as.
func awsDirectExchangeAudit(session *Session, cloudAccountID string, body ExchangeExecuteRequestBody, maxRat *big.Rat) *config.RIExchangeDirectExecution {
	targets := body.Targets
	if len(targets) == 0 {
		targets = []ExchangeTargetBody{{OfferingID: body.TargetOfferingID, Count: body.TargetCount}}
	}
	audit := &config.RIExchangeDirectExecution{
		ID:                 uuid.NewString(),
		Provider:           "aws",
		CloudAccountID:     auditCloudAccountID(cloudAccountID),
		Region:             optionalString(body.Region),
		SourceIDs:          body.RIIDs,
		Targets:            auditJSON(targets),
		MaxPaymentDue:      auditDecimal(maxRat),
		MaxPaymentCurrency: "USD",
	}
	directExchangeActor(audit, session)
	return audit
}

// awsDirectExchangeOutcome builds the outcome of a direct AWS execute.
// ExecuteExchange may return its quote alongside an error (a guardrail
// refusal), so the quoted amount is recorded whenever there is one.
func awsDirectExchangeOutcome(exchangeID string, quote *exchange.ExchangeQuoteSummary, providerErr, mapped error) config.RIExchangeDirectOutcome {
	outcome := config.RIExchangeDirectOutcome{
		Status:            config.RIExchangeDirectStatusCompleted,
		ProviderReference: exchangeID,
	}
	if quote != nil {
		outcome.NetPayable = auditDecimalString(handlerAcceptedAmount(quote, quote.PaymentDueRaw))
		outcome.NetPayableCurrency = quote.CurrencyCode
	}
	if providerErr != nil {
		outcome.Status = directExchangeFailureStatus(mapped)
		outcome.Error = providerErr.Error()
	}
	return outcome
}

// azureDirectExchangeAudit builds the audit row for a direct Azure execute
// from the request and the fresh re-quote that cleared the guardrails.
func azureDirectExchangeAudit(session *Session, accountID string, body AzureExecuteExchangeRequestBody, maxRat *big.Rat, preview *azurecompute.ExchangePreview) *config.RIExchangeDirectExecution {
	sourceIDs := make([]string, 0, len(body.Sources))
	for _, s := range body.Sources {
		sourceIDs = append(sourceIDs, s.ReservationID)
	}
	audit := &config.RIExchangeDirectExecution{
		ID:                 uuid.NewString(),
		Provider:           "azure",
		CloudAccountID:     auditCloudAccountID(accountID),
		ProviderAccountID:  optionalString(body.SubscriptionID),
		SourceIDs:          sourceIDs,
		Sources:            auditJSON(body.Sources),
		Targets:            auditJSON(body.Targets),
		MaxPaymentDue:      auditDecimal(maxRat),
		MaxPaymentCurrency: body.Currency,
		QuotedNetPayable:   auditFloat(preview.NetPayable),
		ProviderReference:  preview.SessionID,
	}
	directExchangeActor(audit, session)
	return audit
}

// azureDirectExchangeOutcome builds the outcome of a direct Azure execute.
func azureDirectExchangeOutcome(result *azurecompute.ExchangeResult, providerErr, mapped error) config.RIExchangeDirectOutcome {
	if providerErr != nil {
		return config.RIExchangeDirectOutcome{
			Status: directExchangeFailureStatus(mapped),
			Error:  providerErr.Error(),
		}
	}
	outcome := config.RIExchangeDirectOutcome{Status: config.RIExchangeDirectStatusCompleted}
	if result != nil {
		outcome.ProviderReference = result.SessionID
		outcome.NetPayable = auditFloat(result.NetPayable)
		outcome.NetPayableCurrency = result.NetPayableCurrency
		outcome.ProviderStatus = result.Status
	}
	return outcome
}
