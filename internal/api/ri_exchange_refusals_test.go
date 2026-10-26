package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	azurecompute "github.com/LeanerCloud/cloud-commitments-go/providers/azure/services/compute"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quoteOnlyEC2 answers GetReservedInstancesExchangeQuote with a fixed quote
// and counts accepts, so a test can run the real cloud-commitments-go
// ExecuteExchange guardrails without any AWS call.
type quoteOnlyEC2 struct {
	currency string
	quotes   int
	accepts  int
}

func (f *quoteOnlyEC2) GetReservedInstancesExchangeQuote(context.Context, *ec2.GetReservedInstancesExchangeQuoteInput, ...func(*ec2.Options)) (*ec2.GetReservedInstancesExchangeQuoteOutput, error) {
	f.quotes++
	out := &ec2.GetReservedInstancesExchangeQuoteOutput{IsValidExchange: aws.Bool(true), PaymentDue: aws.String("10.00")}
	if f.currency != "" {
		out.CurrencyCode = aws.String(f.currency)
	}
	return out, nil
}

func (f *quoteOnlyEC2) AcceptReservedInstancesExchangeQuote(context.Context, *ec2.AcceptReservedInstancesExchangeQuoteInput, ...func(*ec2.Options)) (*ec2.AcceptReservedInstancesExchangeQuoteOutput, error) {
	f.accepts++
	return &ec2.AcceptReservedInstancesExchangeQuoteOutput{ExchangeId: aws.String("riex-should-not-happen")}, nil
}

// TestExecuteExchange_NonUSDQuoteRefusedAs422 is the #521 regression test for
// AWS: the real ExecuteExchange refuses a quote that is not in USD with
// exchange.ErrQuoteNotUSD before accepting it. Before #521 the handler
// answered that refusal with a generic 500 "exchange execution failed" and
// recorded the audit outcome as unknown, although nothing was submitted.
func TestExecuteExchange_NonUSDQuoteRefusedAs422(t *testing.T) {
	for _, currency := range []string{"EUR", ""} {
		t.Run("currency="+currency, func(t *testing.T) {
			fake := &quoteOnlyEC2{currency: currency}
			store := newDirectExchangeAuditStore()
			h := awsAuditHandler(t, store, exchange.NewExchangeClientFromAPI(fake).Execute)

			_, err := h.executeExchange(auditRequest(context.Background(), awsAuditBody))

			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "a non-USD quote is a client error, not a generic 500: %v", err)
			assert.Equal(t, 422, ce.code)
			assert.Contains(t, ce.message, "not denominated in USD")
			assert.NotEqual(t, "exchange execution failed", ce.message)
			assert.Equal(t, 1, fake.quotes, "the refusal comes from the initial quote")
			assert.Zero(t, fake.accepts, "a non-USD quote must never be accepted")

			_, outcome, finalized := store.onlyRow(t)
			require.True(t, finalized)
			assert.Equal(t, config.RIExchangeDirectStatusFailed, outcome.Status,
				"the exchange was refused before accept, so the trail must say failed, not unknown")
		})
	}
}

// TestMapExchangeErrors_Refusals pins the mapping both exchange error mappers
// apply: a wrapped cloud-commitments-go refusal is a 422 with its message,
// provider client faults keep their 400, and anything else stays a 500.
func TestMapExchangeErrors_Refusals(t *testing.T) {
	notUSD := fmt.Errorf("aborting exchange at accept time: %w", fmt.Errorf("%w: quote is in %q", exchange.ErrQuoteNotUSD, "EUR"))
	scope := fmt.Errorf("azure: CalculateExchange: sources[0]: %w", azurecompute.ErrUnsupportedAppliedScope)
	mappers := map[string]func(string, error) error{
		"aws":   mapAWSExchangeError,
		"azure": mapAzureExchangeError,
	}
	for name, mapErr := range mappers {
		for _, refusal := range []error{notUSD, scope} {
			t.Run(name+"/"+refusal.Error(), func(t *testing.T) {
				ce, ok := IsClientError(mapErr("op failed", refusal))
				require.True(t, ok)
				assert.Equal(t, 422, ce.code)
				assert.Equal(t, refusal.Error(), ce.message)
			})
		}
		t.Run(name+"/other", func(t *testing.T) {
			ce, ok := IsClientError(mapErr("op failed", errors.New("connection reset")))
			require.True(t, ok)
			assert.Equal(t, 500, ce.code)
			assert.Equal(t, "op failed", ce.message)
		})
	}

	ce, ok := IsClientError(mapAWSExchangeError("op failed", &smithy.GenericAPIError{Code: "InvalidParameter", Message: "bad", Fault: smithy.FaultClient}))
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	ce, ok = IsClientError(mapAzureExchangeError("op failed", &azcore.ResponseError{StatusCode: 404}))
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
}
