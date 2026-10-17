package config

// Unit tests for how CreateAccountRegistration classifies insert errors
// (issue #137): unique violations are recognized by SQLSTATE and constraint
// name, never by the words in the error message.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func execRegistrationInsertError(t *testing.T, dbErr error) error {
	t.Helper()
	mock := newMock(t)
	defer mock.Close()
	mock.ExpectExec("INSERT INTO account_registrations").
		WithArgs(anyArgsCfg(24)...).
		WillReturnError(dbErr)

	err := storeWith(mock).CreateAccountRegistration(context.Background(), &AccountRegistration{
		ReferenceToken: "tok", Status: "pending", Provider: "aws", ExternalID: "123456789012",
		AccountName: "acct", ContactEmail: "a@example.com",
	})
	require.NoError(t, mock.ExpectationsWereMet())
	require.Error(t, err)
	return err
}

func TestCreateAccountRegistration_PendingUniqueViolation_IsDuplicate(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: "idx_account_registrations_pending_unique",
		Message: "duplicate key value violates unique constraint"}
	err := execRegistrationInsertError(t, pgErr)

	assert.ErrorIs(t, err, ErrDuplicatePendingRegistration)
	assert.NotErrorIs(t, err, ErrReferenceTokenCollision)
	var got *pgconn.PgError
	require.ErrorAs(t, err, &got, "driver error must stay reachable for callers")
	assert.Same(t, pgErr, got)
}

// A locale-translated (lc_messages) server message carries no English words;
// the SQLSTATE and constraint name still identify it.
func TestCreateAccountRegistration_TranslatedMessage_IsDuplicate(t *testing.T) {
	err := execRegistrationInsertError(t, &pgconn.PgError{Code: "23505",
		ConstraintName: "idx_account_registrations_pending_unique",
		Message:        "llave duplicada viola restricción de unicidad"})
	assert.ErrorIs(t, err, ErrDuplicatePendingRegistration)
}

func TestCreateAccountRegistration_ReferenceTokenViolation_IsCollisionNotDuplicate(t *testing.T) {
	err := execRegistrationInsertError(t, &pgconn.PgError{Code: "23505",
		ConstraintName: "account_registrations_reference_token_key",
		Message:        `duplicate key value violates unique constraint "account_registrations_reference_token_key"`})

	assert.ErrorIs(t, err, ErrReferenceTokenCollision)
	assert.NotErrorIs(t, err, ErrDuplicatePendingRegistration)
}

func TestCreateAccountRegistration_NonDuplicateErrors_AreNotDuplicate(t *testing.T) {
	cases := map[string]error{
		"unique violation on another constraint": &pgconn.PgError{Code: "23505",
			ConstraintName: "account_registrations_pkey",
			Message:        `duplicate key value violates unique constraint "account_registrations_pkey"`},
		"other SQLSTATE naming the pending index": &pgconn.PgError{Code: "23514",
			ConstraintName: "idx_account_registrations_pending_unique",
			Message:        "duplicate key check failed"},
		"plain error whose text says duplicate key": errors.New(
			`duplicate key value violates unique constraint "idx_account_registrations_pending_unique"`),
	}
	for name, dbErr := range cases {
		t.Run(name, func(t *testing.T) {
			err := execRegistrationInsertError(t, dbErr)
			assert.NotErrorIs(t, err, ErrDuplicatePendingRegistration)
			assert.NotErrorIs(t, err, ErrReferenceTokenCollision)
			assert.ErrorIs(t, err, dbErr, "original error must stay wrapped")
		})
	}
}

func TestCreateAccountRegistration_Success(t *testing.T) {
	mock := newMock(t)
	defer mock.Close()
	mock.ExpectExec("INSERT INTO account_registrations").
		WithArgs(anyArgsCfg(24)...).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	reg := &AccountRegistration{ReferenceToken: "tok", Status: "pending", Provider: "aws"}
	require.NoError(t, storeWith(mock).CreateAccountRegistration(context.Background(), reg))
	assert.NotEmpty(t, reg.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}
