package config

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var serviceConfigCols = []string{
	"provider", "service", "enabled", "term", "payment", "coverage", "ramp_schedule",
	"include_engines", "exclude_engines", "include_regions", "exclude_regions",
	"include_types", "exclude_types", "min_count",
}

// expectServiceConfigLockedRead queues the BEGIN, the per-service advisory
// lock and the FOR UPDATE read UpdateServiceConfigAtomic issues, in order.
func expectServiceConfigLockedRead(mock pgxmock.PgxPoolIface, rows *pgxmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").WithArgs(serviceConfigLockPrefix + "aws/rds").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery(`FROM service_configs\s+WHERE provider = \$1 AND service = \$2 FOR UPDATE`).
		WithArgs("aws", "rds").WillReturnRows(rows)
}

// TestPGXMock_UpdateServiceConfigAtomic_LockedReadMergeWrite pins issue #1488:
// the read, the caller's merge and the upsert run in ONE transaction, after the
// per-service advisory lock and with the row read FOR UPDATE. pgxmock enforces
// expectation order, so a pass means no statement escaped the transaction.
func TestPGXMock_UpdateServiceConfigAtomic_LockedReadMergeWrite(t *testing.T) {
	mock := newMock(t)
	store := storeWith(mock)

	expectServiceConfigLockedRead(mock, pgxmock.NewRows(serviceConfigCols).AddRow(
		"aws", "rds", true, 3, "all-upfront", 50.0, RampImmediate,
		[]string{"mysql"}, []string{}, []string{}, []string{}, []string{}, []string{}, 4,
	))
	mock.ExpectExec("INSERT INTO service_configs").
		WithArgs("aws", "rds", true, 3, "all-upfront", 80.0, RampImmediate,
			[]string{"mysql"}, []string{}, []string{}, []string{}, []string{}, []string{}, 4).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	saved, err := store.UpdateServiceConfigAtomic(context.Background(), "aws", "rds", func(existing *ServiceConfig) (*ServiceConfig, error) {
		require.NotNil(t, existing, "apply must see the locked row")
		next := *existing
		next.Coverage = 80
		return &next, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 80.0, saved.Coverage)
	assert.Equal(t, 4, saved.MinCount)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestPGXMock_UpdateServiceConfigAtomic_NoRowPassesNil: with no stored row,
// apply gets nil (not an error) and its row is inserted under the same lock.
func TestPGXMock_UpdateServiceConfigAtomic_NoRowPassesNil(t *testing.T) {
	mock := newMock(t)
	store := storeWith(mock)

	expectServiceConfigLockedRead(mock, pgxmock.NewRows(serviceConfigCols))
	mock.ExpectExec("INSERT INTO service_configs").WithArgs(anyArgsCfg(14)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	sawNil := false
	_, err := store.UpdateServiceConfigAtomic(context.Background(), "aws", "rds", func(existing *ServiceConfig) (*ServiceConfig, error) {
		sawNil = existing == nil
		return &ServiceConfig{Provider: "aws", Service: "rds", Term: 1, Payment: "no-upfront"}, nil
	})
	require.NoError(t, err)
	assert.True(t, sawNil)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestPGXMock_UpdateServiceConfigAtomic_AbortsWithoutWrite: an apply error (a
// validation failure) or a row keyed on another service rolls back with no
// upsert. The apply error propagates unchanged.
func TestPGXMock_UpdateServiceConfigAtomic_AbortsWithoutWrite(t *testing.T) {
	sentinel := errors.New("validation failed")
	cases := map[string]struct {
		next    *ServiceConfig
		err     error
		wantErr string
	}{
		"apply error":   {err: sentinel, wantErr: "validation failed"},
		"nil row":       {wantErr: "same key"},
		"other service": {next: &ServiceConfig{Provider: "aws", Service: "ec2"}, wantErr: "same key"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mock := newMock(t)
			store := storeWith(mock)
			expectServiceConfigLockedRead(mock, pgxmock.NewRows(serviceConfigCols))
			// No ExpectExec("INSERT ..."): the upsert must not run.
			mock.ExpectRollback()

			_, err := store.UpdateServiceConfigAtomic(context.Background(), "aws", "rds", func(*ServiceConfig) (*ServiceConfig, error) {
				return tc.next, tc.err
			})
			require.ErrorContains(t, err, tc.wantErr)
			if tc.err != nil {
				assert.ErrorIs(t, err, tc.err)
			}
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// TestPGXMock_UpdateServiceConfigAtomic_ReadErrorAborts: a failed read is not
// mistaken for a missing row; apply never runs and nothing is written.
func TestPGXMock_UpdateServiceConfigAtomic_ReadErrorAborts(t *testing.T) {
	mock := newMock(t)
	store := storeWith(mock)
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery("FOR UPDATE").WithArgs("aws", "rds").WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	_, err := store.UpdateServiceConfigAtomic(context.Background(), "aws", "rds", func(*ServiceConfig) (*ServiceConfig, error) {
		t.Fatal("apply must not run after a failed read")
		return nil, nil
	})
	require.ErrorContains(t, err, "connection reset")
	assert.NoError(t, mock.ExpectationsWereMet())
}
