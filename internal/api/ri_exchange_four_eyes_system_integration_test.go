//go:build integration
// +build integration

package api

// Real-Postgres check for issue #518: every production RI exchange row is
// written by the scheduler or the ladder engine through the server package's
// exchangeToConfigRecord, which stores no creator. The 4-eyes gate must read
// that NULL back from the real store and still refuse an approval that carries
// no identified approver, leaving the row pending and unattributed.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

const systemExchangeRawToken = "system-exchange-raw-token"

// seedSystemCreatedExchange stores a pending exchange in the shape the scheduler
// writes it: no creator, a raw approval token (hashed by the store).
func seedSystemCreatedExchange(ctx context.Context, t *testing.T, store *config.PostgresStore) string {
	t.Helper()
	expires := time.Now().Add(24 * time.Hour)
	rec := &config.RIExchangeRecord{
		AccountID:          "123456789012",
		Region:             "us-east-1",
		SourceRIIDs:        []string{"ri-518"},
		SourceInstanceType: "m5.large",
		SourceCount:        1,
		TargetOfferingID:   "offering-518",
		TargetInstanceType: "c5.large",
		TargetCount:        1,
		PaymentDue:         "10.00",
		Status:             "pending",
		ApprovalToken:      systemExchangeRawToken,
		Mode:               "manual",
		ExpiresAt:          &expires,
	}
	require.NoError(t, store.SaveRIExchangeRecord(ctx, rec))
	got, err := store.GetRIExchangeRecord(ctx, rec.ID)
	require.NoError(t, err)
	require.Nil(t, got.CreatedByUserID, "fixture must read back with no creator, as scheduler rows do")
	return rec.ID
}

func setupSystemExchangeHandler(ctx context.Context, t *testing.T, fourEyes bool) (*Handler, *config.PostgresStore, *int) {
	t.Helper()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	store := config.NewPostgresStore(container.DB)
	_, err := store.UpdateGlobalConfigAtomic(ctx, func(c *config.GlobalConfig) error {
		c.RequireDifferentApprover = fourEyes
		return nil
	})
	require.NoError(t, err)

	executions := 0
	h := &Handler{
		config: store,
		// Never reach AWS: count the call and fail it.
		executeExchangeFn: func(context.Context, exchange.ExchangeExecuteRequest) (string, *exchange.ExchangeQuoteSummary, error) {
			executions++
			return "", nil, errors.New("fixture executor: no real exchange")
		},
	}
	return h, store, &executions
}

func TestApproveRIExchange_Integration_SystemCreatedFourEyes(t *testing.T) {
	ctx := context.Background()

	t.Run("4-eyes on: token without session is refused and the row stays pending", func(t *testing.T) {
		h, store, executions := setupSystemExchangeHandler(ctx, t, true)
		id := seedSystemCreatedExchange(ctx, t, store)

		_, err := h.approveRIExchange(ctx, &events.LambdaFunctionURLRequest{}, id, systemExchangeRawToken)
		require.Error(t, err)
		ce, ok := IsClientError(err)
		require.True(t, ok, "want ClientError, got %v", err)
		assert.Equal(t, 500, ce.code)
		assert.Contains(t, err.Error(), "no session could be resolved")

		got, err := store.GetRIExchangeRecord(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "pending", got.Status)
		assert.Nil(t, got.ApprovedBy)
		assert.Zero(t, *executions)
	})

	// Control: the same seed is approvable with 4-eyes off, so the refusal
	// above comes from the 4-eyes gate and not from the fixture.
	t.Run("4-eyes off: the same token approval leaves pending", func(t *testing.T) {
		h, store, _ := setupSystemExchangeHandler(ctx, t, false)
		id := seedSystemCreatedExchange(ctx, t, store)

		_, _ = h.approveRIExchange(ctx, &events.LambdaFunctionURLRequest{}, id, systemExchangeRawToken)

		got, err := store.GetRIExchangeRecord(ctx, id)
		require.NoError(t, err)
		assert.NotEqual(t, "pending", got.Status)
	})
}
