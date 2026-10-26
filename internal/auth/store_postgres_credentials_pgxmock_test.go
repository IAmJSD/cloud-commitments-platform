package auth

// pgxmock tests for how EnableMFA names a refused enrollment (issue #568 (d)):
// an expired pending secret is ErrMFAEnrollmentExpired, anything else stays
// ErrUserChanged. The real-Postgres behavior is pinned by
// TestIntegration_EnableMFARefusalReason.

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	enableMFAUpdateRegex  = `(?s)UPDATE users SET mfa_enabled = true, mfa_secret = \$2.*WHERE id = \$1 AND mfa_enabled = false AND mfa_pending_secret = \$2\s+AND mfa_pending_secret_expires_at > NOW\(\)`
	enableMFARefusalRegex = `(?s)SELECT \(mfa_pending_secret_expires_at <= NOW\(\)\) IS TRUE FROM users\s+WHERE id = \$1 AND mfa_enabled = false AND mfa_pending_secret = \$2`
)

func TestPGXMock_EnableMFA_RefusalReason(t *testing.T) {
	hashes := []string{"hash-1"}
	tests := []struct {
		name        string
		diagnose    func(pgxmock.PgxPoolIface)
		want        error
		wantExpired bool
	}{
		{
			name: "expired-pending-secret",
			diagnose: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(enableMFARefusalRegex).WithArgs("user-1", "PENDING").
					WillReturnRows(pgxmock.NewRows([]string{"expired"}).AddRow(true))
			},
			want:        ErrMFAEnrollmentExpired,
			wantExpired: true,
		},
		{
			name: "same-pending-secret-not-expired",
			diagnose: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(enableMFARefusalRegex).WithArgs("user-1", "PENDING").
					WillReturnRows(pgxmock.NewRows([]string{"expired"}).AddRow(false))
			},
			want: ErrUserChanged,
		},
		{
			name: "pending-secret-gone-or-mfa-on",
			diagnose: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(enableMFARefusalRegex).WithArgs("user-1", "PENDING").WillReturnError(pgx.ErrNoRows)
			},
			want: ErrUserChanged,
		},
		{
			name: "diagnosis-query-fails",
			diagnose: func(m pgxmock.PgxPoolIface) {
				m.ExpectQuery(enableMFARefusalRegex).WithArgs("user-1", "PENDING").WillReturnError(errors.New("connection reset"))
			},
			want: ErrUserChanged,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := newAuthPgxMock(t)
			store := NewPostgresStore(mock)
			mock.ExpectExec(enableMFAUpdateRegex).WithArgs("user-1", "PENDING", hashes, int64(7)).
				WillReturnResult(pgxmock.NewResult("UPDATE", 0))
			tt.diagnose(mock)

			err := store.EnableMFA(t.Context(), "user-1", "PENDING", 7, hashes)
			require.ErrorIs(t, err, tt.want)
			assert.Equal(t, tt.wantExpired, errors.Is(err, ErrMFAEnrollmentExpired))
			assert.Equal(t, !tt.wantExpired, errors.Is(err, ErrUserChanged))
		})
	}
}

func TestPGXMock_EnableMFA_SuccessSkipsDiagnosis(t *testing.T) {
	mock := newAuthPgxMock(t)
	store := NewPostgresStore(mock)
	mock.ExpectExec(enableMFAUpdateRegex).WithArgs("user-1", "PENDING", []string{"hash-1"}, int64(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	require.NoError(t, store.EnableMFA(t.Context(), "user-1", "PENDING", 7, []string{"hash-1"}))
}
