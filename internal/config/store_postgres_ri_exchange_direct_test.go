package config

// store_postgres_ri_exchange_direct_test.go — pgxmock coverage for the
// direct-execute RI exchange audit trail (issue #82). The statement shapes,
// argument wiring and fail-loud guards are pinned here; the compare-and-set,
// the table's CHECK constraints and the concurrent-finalize race run against a
// live Postgres in store_postgres_ri_exchange_direct_db_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const directExecTestID = "3f2e1d0c-9b8a-4766-a5b4-c3d2e1f0abcd"

func validDirectExecution() *RIExchangeDirectExecution {
	return &RIExchangeDirectExecution{
		ID:                 directExecTestID,
		Provider:           "azure",
		CloudAccountID:     strPtr("acct-1"),
		ProviderAccountID:  strPtr("sub-1"),
		SourceIDs:          []string{"res-1"},
		Sources:            json.RawMessage(`[{"reservation_id":"res-1","quantity":4}]`),
		Targets:            json.RawMessage(`[{"sku":"Standard_D8s_v3","location":"eastus","term":"P3Y","quantity":2}]`),
		MaxPaymentDue:      "100",
		MaxPaymentCurrency: "USD",
		QuotedNetPayable:   strPtr("-12.5"),
		ProviderReference:  "sess-1",
		ExecutedBy:         "alice@example.com",
	}
}

func TestPGXMock_SaveRIExchangeDirectExecution_InsertsSubmittedRow(t *testing.T) {
	mock := newMock(t)
	store := storeWith(mock)
	rec := validDirectExecution()

	mock.ExpectExec("INSERT INTO ri_exchange_direct_executions").
		WithArgs(
			directExecTestID, "azure", rec.CloudAccountID, rec.ProviderAccountID, rec.Region,
			[]string{"res-1"}, strPtr(`[{"reservation_id":"res-1","quantity":4}]`),
			`[{"sku":"Standard_D8s_v3","location":"eastus","term":"P3Y","quantity":2}]`,
			"100", "USD", rec.QuotedNetPayable, pgxmock.AnyArg(),
			rec.ExecutedByUserID, "alice@example.com", rec.ExecutedByAPIKeyID,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	require.NoError(t, store.SaveRIExchangeDirectExecution(context.Background(), rec))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestPGXMock_SaveRIExchangeDirectExecution_RejectsInvalidRecord: a row the
// table would refuse fails before the round trip, naming the problem, so the
// handler refuses the exchange with a useful error.
func TestPGXMock_SaveRIExchangeDirectExecution_RejectsInvalidRecord(t *testing.T) {
	cases := map[string]func(*RIExchangeDirectExecution){
		"non-UUID id":        func(r *RIExchangeDirectExecution) { r.ID = "exec-1" },
		"unknown provider":   func(r *RIExchangeDirectExecution) { r.Provider = "gcp" },
		"no sources":         func(r *RIExchangeDirectExecution) { r.SourceIDs = nil },
		"empty targets":      func(r *RIExchangeDirectExecution) { r.Targets = json.RawMessage(`[]`) },
		"nil targets":        func(r *RIExchangeDirectExecution) { r.Targets = nil },
		"object targets":     func(r *RIExchangeDirectExecution) { r.Targets = json.RawMessage(`{"sku":"x"}`) },
		"empty sources json": func(r *RIExchangeDirectExecution) { r.Sources = json.RawMessage(`[]`) },
		"no cap":             func(r *RIExchangeDirectExecution) { r.MaxPaymentDue = " " },
		"no cap currency":    func(r *RIExchangeDirectExecution) { r.MaxPaymentCurrency = "" },
		"no actor":           func(r *RIExchangeDirectExecution) { r.ExecutedBy = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			mock := newMock(t)
			store := storeWith(mock)
			rec := validDirectExecution()
			mutate(rec)
			require.Error(t, store.SaveRIExchangeDirectExecution(context.Background(), rec))
			assert.NoError(t, mock.ExpectationsWereMet(), "nothing may reach the database")
		})
	}
}

func TestPGXMock_SaveRIExchangeDirectExecution_DBErrorSurfaces(t *testing.T) {
	mock := newMock(t)
	store := storeWith(mock)
	mock.ExpectExec("INSERT INTO ri_exchange_direct_executions").
		WithArgs(anyArgsCfg(15)...).
		WillReturnError(errors.New("connection refused"))

	err := store.SaveRIExchangeDirectExecution(context.Background(), validDirectExecution())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestPGXMock_FinalizeRIExchangeDirectExecution_CompareAndSet pins that the
// outcome write is guarded by status = 'submitted' and that a zero-row result
// is reported as ErrRIExchangeDirectExecutionNotSubmitted, never as success.
func TestPGXMock_FinalizeRIExchangeDirectExecution_CompareAndSet(t *testing.T) {
	outcome := RIExchangeDirectOutcome{
		Status: RIExchangeDirectStatusCompleted, ProviderReference: "riex-1",
		NetPayable: strPtr("40.25"), NetPayableCurrency: "USD",
	}

	t.Run("submitted row is finalized", func(t *testing.T) {
		mock := newMock(t)
		store := storeWith(mock)
		mock.ExpectExec(`UPDATE ri_exchange_direct_executions(.|\n)*WHERE id = \$1(.|\n)*AND status = 'submitted'`).
			WithArgs(directExecTestID, "completed", pgxmock.AnyArg(), outcome.NetPayable, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		require.NoError(t, store.FinalizeRIExchangeDirectExecution(context.Background(), directExecTestID, outcome))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("finalized or missing row is refused", func(t *testing.T) {
		mock := newMock(t)
		store := storeWith(mock)
		mock.ExpectExec("UPDATE ri_exchange_direct_executions").
			WithArgs(anyArgsCfg(7)...).
			WillReturnResult(pgxmock.NewResult("UPDATE", 0))
		err := store.FinalizeRIExchangeDirectExecution(context.Background(), directExecTestID, outcome)
		require.ErrorIs(t, err, ErrRIExchangeDirectExecutionNotSubmitted)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("non-terminal status never reaches the database", func(t *testing.T) {
		for _, status := range []string{RIExchangeDirectStatusSubmitted, "", "processing"} {
			mock := newMock(t)
			store := storeWith(mock)
			err := store.FinalizeRIExchangeDirectExecution(context.Background(), directExecTestID, RIExchangeDirectOutcome{Status: status})
			require.Error(t, err, status)
			assert.NoError(t, mock.ExpectationsWereMet())
		}
	})
}
