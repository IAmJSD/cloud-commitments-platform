//go:build integration
// +build integration

package config

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// directExecRow is what an auditor reads back for one direct execute.
type directExecRow struct {
	status, providerRef, netPayable, netCurrency, providerStatus, errText *string
	quoted, maxPay                                                        string
	sources                                                               *string
	completedAt                                                           *time.Time
}

func readDirectExecRow(t *testing.T, ctx context.Context, store *PostgresStore, id string) directExecRow {
	t.Helper()
	var r directExecRow
	var quoted *string
	require.NoError(t, store.db.QueryRow(ctx, `
		SELECT status, provider_reference, net_payable::text, net_payable_currency, provider_status, error,
		       quoted_net_payable::text, max_payment_due::text, sources::text, completed_at
		  FROM ri_exchange_direct_executions WHERE id = $1`, id).
		Scan(&r.status, &r.providerRef, &r.netPayable, &r.netCurrency, &r.providerStatus, &r.errText,
			&quoted, &r.maxPay, &r.sources, &r.completedAt))
	if quoted != nil {
		r.quoted = *quoted
	}
	return r
}

func newDirectExec(provider string) *RIExchangeDirectExecution {
	rec := &RIExchangeDirectExecution{
		ID:                 uuid.NewString(),
		Provider:           provider,
		SourceIDs:          []string{"src-1"},
		Targets:            json.RawMessage(`[{"offering_id":"o","count":1}]`),
		MaxPaymentDue:      "500",
		MaxPaymentCurrency: "USD",
		ExecutedBy:         "alice@example.com",
	}
	if provider == "azure" {
		rec.ProviderAccountID = strPtr("sub-1")
		rec.Sources = json.RawMessage(`[{"reservation_id":"src-1","quantity":4}]`)
		rec.QuotedNetPayable = strPtr("-12.5")
		rec.ProviderReference = "sess-1"
	} else {
		rec.Region = strPtr("eu-central-1")
	}
	return rec
}

// TestPostgresStoreDB_RIExchangeDirectExecution runs the issue #82 audit
// trail against a real PostgreSQL: the row starts submitted with no
// completed_at, is finalized exactly once, keeps the first outcome when a
// second finalize races or follows it, and the table itself refuses rows that
// would misreport an exchange.
func TestPostgresStoreDB_RIExchangeDirectExecution(t *testing.T) {
	conn := setupTestContainerDB(t)
	if conn == nil {
		return
	}
	store := NewPostgresStore(conn)
	ctx := context.Background()

	t.Run("submitted then completed, amounts stored exactly", func(t *testing.T) {
		rec := newDirectExec("azure")
		require.NoError(t, store.SaveRIExchangeDirectExecution(ctx, rec))

		row := readDirectExecRow(t, ctx, store, rec.ID)
		require.NotNil(t, row.status)
		assert.Equal(t, RIExchangeDirectStatusSubmitted, *row.status)
		assert.Nil(t, row.completedAt)
		assert.Equal(t, "-12.5", row.quoted, "a refund quote must survive as a negative amount")
		assert.Equal(t, "500", row.maxPay)
		require.NotNil(t, row.sources)
		assert.JSONEq(t, `[{"reservation_id":"src-1","quantity":4}]`, *row.sources)

		require.NoError(t, store.FinalizeRIExchangeDirectExecution(ctx, rec.ID, RIExchangeDirectOutcome{
			Status: RIExchangeDirectStatusCompleted, NetPayable: strPtr("-12.5"), NetPayableCurrency: "USD", ProviderStatus: "Succeeded",
		}))
		row = readDirectExecRow(t, ctx, store, rec.ID)
		assert.Equal(t, RIExchangeDirectStatusCompleted, *row.status)
		require.NotNil(t, row.completedAt)
		require.NotNil(t, row.providerRef)
		assert.Equal(t, "sess-1", *row.providerRef, "an outcome without a reference keeps the one recorded at submit")
		require.NotNil(t, row.netPayable)
		assert.Equal(t, "-12.5", *row.netPayable)
		assert.Equal(t, "Succeeded", *row.providerStatus)
		assert.Nil(t, row.errText)
	})

	t.Run("AWS row has no sources detail and takes the exchange id at finalize", func(t *testing.T) {
		rec := newDirectExec("aws")
		require.NoError(t, store.SaveRIExchangeDirectExecution(ctx, rec))
		row := readDirectExecRow(t, ctx, store, rec.ID)
		assert.Nil(t, row.sources)
		assert.Nil(t, row.providerRef)

		require.NoError(t, store.FinalizeRIExchangeDirectExecution(ctx, rec.ID, RIExchangeDirectOutcome{
			Status: RIExchangeDirectStatusUnknown, ProviderReference: "riex-1", Error: "context deadline exceeded",
		}))
		row = readDirectExecRow(t, ctx, store, rec.ID)
		assert.Equal(t, RIExchangeDirectStatusUnknown, *row.status)
		assert.Equal(t, "riex-1", *row.providerRef)
		assert.Equal(t, "context deadline exceeded", *row.errText)
		assert.Nil(t, row.netPayable)
	})

	t.Run("a finalized row is never rewritten", func(t *testing.T) {
		rec := newDirectExec("aws")
		require.NoError(t, store.SaveRIExchangeDirectExecution(ctx, rec))
		require.NoError(t, store.FinalizeRIExchangeDirectExecution(ctx, rec.ID, RIExchangeDirectOutcome{Status: RIExchangeDirectStatusCompleted, ProviderReference: "riex-2"}))

		err := store.FinalizeRIExchangeDirectExecution(ctx, rec.ID, RIExchangeDirectOutcome{Status: RIExchangeDirectStatusFailed, Error: "late"})
		require.ErrorIs(t, err, ErrRIExchangeDirectExecutionNotSubmitted)
		row := readDirectExecRow(t, ctx, store, rec.ID)
		assert.Equal(t, RIExchangeDirectStatusCompleted, *row.status)
		assert.Nil(t, row.errText)
	})

	t.Run("finalizing a row that does not exist is refused", func(t *testing.T) {
		err := store.FinalizeRIExchangeDirectExecution(ctx, uuid.NewString(), RIExchangeDirectOutcome{Status: RIExchangeDirectStatusCompleted})
		require.ErrorIs(t, err, ErrRIExchangeDirectExecutionNotSubmitted)
	})

	t.Run("a finalize racing an uncommitted finalize keeps the first outcome", func(t *testing.T) {
		rec := newDirectExec("aws")
		require.NoError(t, store.SaveRIExchangeDirectExecution(ctx, rec))

		// Hold the first outcome in a still-open transaction. A finalize that
		// READ the status and then wrote would see 'submitted' (the other
		// write is uncommitted) and overwrite it; the compare-and-set must
		// instead wait for the row lock and then find nothing to update.
		tx, err := conn.Begin(ctx)
		require.NoError(t, err)
		tag, err := tx.Exec(ctx, `
			UPDATE ri_exchange_direct_executions SET status = 'completed', provider_reference = 'riex-first', completed_at = now()
			 WHERE id = $1 AND status = 'submitted'`, rec.ID)
		require.NoError(t, err)
		require.EqualValues(t, 1, tag.RowsAffected())
		holderPID := testhelpers.BackendPID(ctx, t, tx)

		done := make(chan error, 1)
		go func() {
			done <- store.FinalizeRIExchangeDirectExecution(ctx, rec.ID, RIExchangeDirectOutcome{Status: RIExchangeDirectStatusFailed, Error: "second"})
		}()
		if err, answered := testhelpers.AwaitBlockedBy(ctx, t, conn.Pool(), holderPID, done); answered {
			_ = tx.Rollback(ctx)
			t.Fatalf("the second finalize answered (%v) while the first was uncommitted; it must contend for the row", err)
		}
		require.NoError(t, tx.Commit(ctx))

		require.ErrorIs(t, <-done, ErrRIExchangeDirectExecutionNotSubmitted)
		row := readDirectExecRow(t, ctx, store, rec.ID)
		assert.Equal(t, RIExchangeDirectStatusCompleted, *row.status)
		assert.Equal(t, "riex-first", *row.providerRef)
		assert.Nil(t, row.errText, "the losing finalize must not leak its outcome into the row")
	})

	t.Run("the table refuses rows that misreport an exchange", func(t *testing.T) {
		insert := func(status, completedAt, targets, provider string) error {
			_, err := conn.Exec(ctx, `
				INSERT INTO ri_exchange_direct_executions
				  (id, provider, source_ids, targets, max_payment_due, max_payment_currency, executed_by, status, completed_at)
				VALUES ($1, $2, ARRAY['src-1'], $3::jsonb, 1, 'USD', 'alice@example.com', $4, `+completedAt+`)`,
				uuid.NewString(), provider, targets, status)
			return err
		}
		const okTargets = `[{"offering_id":"o","count":1}]`
		assert.Error(t, insert("submitted", "now()", okTargets, "aws"), "a submitted row has no completion time")
		assert.Error(t, insert("completed", "NULL", okTargets, "aws"), "a finalized row must carry its completion time")
		assert.Error(t, insert("processing", "NULL", okTargets, "aws"), "only the four documented statuses")
		assert.Error(t, insert("submitted", "NULL", `[]`, "aws"), "targets must not be empty")
		assert.Error(t, insert("submitted", "NULL", okTargets, "gcp"), "aws and azure only")
		assert.NoError(t, insert("submitted", "NULL", okTargets, "aws"), "control: a well-formed row is accepted")

		_, err := conn.Exec(ctx, `
			INSERT INTO ri_exchange_direct_executions (provider, source_ids, targets, max_payment_due, max_payment_currency, executed_by)
			VALUES ('aws', ARRAY['src-1'], $1::jsonb, 1, 'USD', '')`, okTargets)
		assert.Error(t, err, "every row names who executed it")
	})
}
