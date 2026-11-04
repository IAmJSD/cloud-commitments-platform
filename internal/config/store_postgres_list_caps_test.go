package config

// Unbounded list reads clamped to MaxListLimit (#120). Each test pins the
// bound LIMIT argument, so dropping the LIMIT (no argument bound) or raising
// it fails the mock's argument match.

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPGXMock_ListAccountRegistrations_ClampsToMaxListLimit(t *testing.T) {
	status, provider := "pending", "aws"
	cases := []struct {
		name    string
		filter  AccountRegistrationFilter
		pattern string
		args    []any
	}{
		{"no filter", AccountRegistrationFilter{}, `ORDER BY created_at DESC, id LIMIT \$1$`, []any{MaxListLimit}},
		{"status and provider", AccountRegistrationFilter{Status: &status, Provider: &provider},
			`status = \$1 AND provider = \$2 ORDER BY created_at DESC, id LIMIT \$3$`, []any{status, provider, MaxListLimit}},
		{"every filter", AccountRegistrationFilter{Status: &status, Provider: &provider, Search: "acme"},
			`ESCAPE '\\'\) ORDER BY created_at DESC, id LIMIT \$4$`, []any{status, provider, "%acme%", MaxListLimit}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMock(t)
			store := storeWith(mock)
			mock.ExpectQuery(tc.pattern).WithArgs(tc.args...).
				WillReturnRows(pgxmock.NewRows(registrationCols()))

			regs, err := store.ListAccountRegistrations(context.Background(), tc.filter)
			require.NoError(t, err)
			assert.Empty(t, regs)
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestPGXMock_GetLadderConfigs_ClampsToMaxListLimit(t *testing.T) {
	mock := newMock(t)
	store := storeWith(mock)
	cols := []string{
		"id", "cloud_account_id", "provider", "enabled", "mode", "cadence",
		"target_coverage", "buffer_fraction", "baseline_percentile",
		"lookback_days", "buffer_utilization_threshold",
		"max_hourly_commit_per_run", "max_actions_per_run",
		"ramp_schedule", "created_at", "updated_at",
	}
	mock.ExpectQuery(`(?s)FROM ladder_configs\s+ORDER BY created_at DESC, id\s+LIMIT \$1`).
		WithArgs(MaxListLimit).
		WillReturnRows(pgxmock.NewRows(cols))

	configs, err := store.GetLadderConfigs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []LadderConfigDB{}, configs, "no rows still reads as an empty slice, not nil")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// suppressionRows returns n active suppression rows in the column order
// ListActiveSuppressions scans.
func suppressionRows(n int) *pgxmock.Rows {
	now := time.Now().UTC()
	rows := pgxmock.NewRows([]string{
		"id", "execution_id", "account_id", "provider", "service", "region",
		"resource_type", "engine", "suppressed_count", "expires_at", "created_at",
		"availability_zone", "platform", "tenancy",
	})
	for i := 0; i < n; i++ {
		rows.AddRow("sup", "exec", "acct", "aws", "ec2", "us-east-1",
			"t4g.nano", "", 1, now.Add(time.Hour), now, nil, nil, nil)
	}
	return rows
}

// ListActiveSuppressions stays unbounded: a large tenant can hold more than
// MaxListLimit active rows, and every row must reach the suppression index.
func TestPGXMock_ListActiveSuppressions_ReturnsEveryRowAboveMaxListLimit(t *testing.T) {
	mock := newMock(t)
	store := storeWith(mock)
	mock.ExpectQuery(`(?s)FROM purchase_suppressions\s+WHERE expires_at > NOW\(\)\s*$`).
		WillReturnRows(suppressionRows(MaxListLimit + 5))

	sups, err := store.ListActiveSuppressions(context.Background())
	require.NoError(t, err)
	assert.Len(t, sups, MaxListLimit+5)
	assert.NoError(t, mock.ExpectationsWereMet())
}
