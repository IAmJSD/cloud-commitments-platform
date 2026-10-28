package api

import (
	"errors"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
)

// exchangeRefusals are the exported sentinels cloud-commitments-go wraps when
// it refuses an RI exchange before reaching the provider's commit call:
//
//   - exchange.ErrQuoteNotUSD: the AWS quote is not in USD (or names no
//     currency), so the USD max_payment_due_usd cap cannot be enforced
//     against it. Returned by ExecuteExchange before
//     AcceptReservedInstancesExchangeQuote.
//   - errAzureUnsupportedAppliedScope (azurecompute.ErrUnsupportedAppliedScope,
//     aliased in provider_wiring.go): a source reservation's applied scope
//     cannot be carried onto the purchased reservation, or the sources mix
//     scopes. Returned by CalculateExchange before any Azure call.
//
// Both describe the request or the account it targets, not a server fault,
// and both prove the exchange did not happen.
var exchangeRefusals = []error{
	exchange.ErrQuoteNotUSD,
	errAzureUnsupportedAppliedScope,
}

// exchangeRefusalError returns a 422 carrying err's message when err wraps
// one of exchangeRefusals, or nil otherwise. 422 matches the Azure execute
// guardrails (checkAzureExchangeMoneyGuardrails), which refuse a currency
// mismatch the same way, and a 4xx makes directExchangeFailureStatus record
// the audit row as failed rather than unknown, which is correct because these
// refusals all happen before the provider's commit call.
func exchangeRefusalError(err error) error {
	for _, sentinel := range exchangeRefusals {
		if errors.Is(err, sentinel) {
			return NewClientError(422, err.Error())
		}
	}
	return nil
}
