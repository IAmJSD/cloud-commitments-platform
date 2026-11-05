package api

import (
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/stretchr/testify/assert"
)

// The note an operator's resolve call stores is the admin's own text: exchange
// history shows it as written, while provider text in the same column does not.
func TestResolveNote_ShowsAsWrittenInHistory(t *testing.T) {
	check := config.RIExchangeCloudCheck{AccountID: strings.Repeat("7", 12), Region: "us-east-1", CheckedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	for name, body := range map[string]riExchangeResolveRequest{
		"completed":      {Outcome: config.StatusCompleted, ExchangeID: "ri-exch-0123", Reason: `checked in the console; it says "done"; ticket 42 é`},
		"failed":         {Outcome: config.StatusFailed, Reason: "the exchange never reached AWS, confirmed with support"},
		"semicolon text": {Outcome: config.StatusFailed, Reason: "first; second; third"},
	} {
		t.Run(name, func(t *testing.T) {
			note := body.note(check)
			assert.Equal(t, note, purchase.SanitizeStoredError(note))
			assert.Equal(t, note+"; "+note, purchase.SanitizeStoredError(note+"; "+note))
		})
	}
	provider := "operation error EC2: AcceptReservedInstancesExchangeQuote, RequestID: 11111111, api error"
	assert.Equal(t, purchase.ErrCategoryMessage(purchase.ErrCategoryUnknown), purchase.SanitizeStoredError(provider))
	note := riExchangeResolveRequest{Outcome: config.StatusFailed, Reason: "ten chars ok"}.note(check)
	assert.Equal(t, purchase.ErrCategoryMessage(purchase.ErrCategoryUnknown), purchase.SanitizeStoredError(note+"; "+provider))
	assert.Equal(t, purchase.ErrCategoryMessage(purchase.ErrCategoryUnknown), purchase.SanitizeStoredError(note+" trailing provider text"))
}
