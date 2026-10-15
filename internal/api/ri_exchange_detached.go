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
// function timeout, which no context can extend.
//
// Same pattern as purchase.detachedTimeout (#706 / #789); kept local to the
// exchange path rather than shared so the two money paths can be reconciled
// deliberately.
const (
	riExchangeExecutionTimeout    = 4 * time.Minute
	riExchangeTerminalSaveTimeout = 30 * time.Second
)

// detachedRIExchangeContext keeps ctx values but drops its cancellation and
// deadline, replacing them with a fresh timeout of d.
func detachedRIExchangeContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}

// awsExchangeExecutor returns the AWS RI exchange execution function: the
// test-injected executeExchangeFn when set, exchange.ExecuteExchange otherwise.
func (h *Handler) awsExchangeExecutor() func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
	if h.executeExchangeFn != nil {
		return h.executeExchangeFn
	}
	return exchange.ExecuteExchange
}
