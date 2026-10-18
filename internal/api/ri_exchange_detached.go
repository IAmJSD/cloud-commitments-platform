package api

import (
	"context"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
)

// Budgets for RI exchange work that must outlive the HTTP request (#790).
//
// Once an exchange is submitted to the provider it cannot be undone, so a
// client disconnect or the container server's 30s per-request timeout must not
// cancel the provider call or the terminal ledger write that records it. Both
// run on contexts that keep the request's values but drop its cancellation,
// each bounded by its own budget so a hung provider or database cannot pin the
// goroutine forever. On Lambda the invocation itself is still bounded by the
// function timeout, which no context can extend, so that timeout must cover
// the sum of both budgets plus a 30s margin (lambda_timeout >= 300, enforced
// by Terraform validation and TestLambdaTimeoutCoversRIExchangeBudget, #913).
//
// The detaching itself is ctxutil.DetachedWithTimeout, shared with the
// purchase path (#706 / #789).
const (
	riExchangeExecutionTimeout    = 4 * time.Minute
	riExchangeTerminalSaveTimeout = 30 * time.Second
)

// awsExchangeExecutor returns the AWS RI exchange execution function: the
// test-injected executeExchangeFn when set, exchange.ExecuteExchange otherwise.
func (h *Handler) awsExchangeExecutor() func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
	if h.executeExchangeFn != nil {
		return h.executeExchangeFn
	}
	return exchange.ExecuteExchange
}
