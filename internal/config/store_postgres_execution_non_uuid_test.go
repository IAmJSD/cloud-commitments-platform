package config

// Unit tests pinning how narrowly GetExecutionByID turns a driver error into
// ErrNotFound (issue #1102 item 5). Only SQLSTATE 22P02 (a value the uuid
// column cannot hold) means "no such execution"; any other Postgres error is a
// real failure and must not reach callers as a 404. The real-Postgres side of
// 22P02 is covered by TestGetExecutionByID_NonUUIDIsNotFoundPostgres.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getExecutionByIDWithDriverError(t *testing.T, dbErr error) error {
	t.Helper()
	mock := newMock(t)
	defer mock.Close()
	mock.ExpectQuery("FROM purchase_executions").
		WithArgs("exec-1").
		WillReturnError(dbErr)

	exec, err := storeWith(mock).GetExecutionByID(context.Background(), "exec-1")
	require.NoError(t, mock.ExpectationsWereMet())
	require.Error(t, err)
	assert.Nil(t, exec)
	return err
}

func TestGetExecutionByID_InvalidTextRepresentationIsNotFound(t *testing.T) {
	err := getExecutionByIDWithDriverError(t, &pgconn.PgError{Code: "22P02",
		Message: `invalid input syntax for type uuid: "exec-1"`})
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetExecutionByID_OtherPostgresErrorsAreNotNotFound(t *testing.T) {
	for _, code := range []string{
		"57P01", // admin_shutdown
		"53300", // too_many_connections
		"42P01", // undefined_table
		"22P03", // invalid_binary_representation, the code next to 22P02
	} {
		t.Run(code, func(t *testing.T) {
			pgErr := &pgconn.PgError{Code: code, Message: "boom"}
			err := getExecutionByIDWithDriverError(t, pgErr)

			assert.NotErrorIs(t, err, ErrNotFound)
			var got *pgconn.PgError
			require.ErrorAs(t, err, &got, "driver error must stay reachable for callers")
			assert.Same(t, pgErr, got)
		})
	}
}
