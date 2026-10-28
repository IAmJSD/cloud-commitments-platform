package config

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

func TestListStoredRecommendationsAzurePricingReadiness(t *testing.T) {
	queryErr := errors.New("migration query failed")
	for _, tc := range []struct {
		name                  string
		version               int
		dirty, missing, ready bool
		err                   error
	}{
		{name: "old", version: 105},
		{name: "dirty", version: 106, dirty: true},
		{name: "missing", missing: true},
		{name: "query error", err: queryErr},
		{name: "ready", version: 106, ready: true},
		{name: "later", version: 107, ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := newMock(t)
			defer pool.Close()
			query := pool.ExpectQuery("SELECT version, dirty FROM schema_migrations")
			if tc.err != nil {
				query.WillReturnError(tc.err)
			} else {
				rows := pgxmock.NewRows([]string{"version", "dirty"})
				if !tc.missing {
					rows.AddRow(tc.version, tc.dirty)
				}
				query.WillReturnRows(rows)
			}
			if tc.ready {
				pool.ExpectQuery("SELECT payload FROM recommendations WHERE provider = \\$1").WithArgs("azure").WillReturnRows(pgxmock.NewRows([]string{"payload"}))
			}
			_, err := storeWith(pool).ListStoredRecommendations(context.Background(), RecommendationFilter{Provider: "azure", RequireAzurePricingMigration: true})
			switch {
			case tc.ready:
				require.NoError(t, err)
			case tc.err != nil:
				require.ErrorIs(t, err, queryErr)
				require.NotErrorIs(t, err, ErrAzurePricingNotReady)
			default:
				require.ErrorIs(t, err, ErrAzurePricingNotReady)
			}
			require.NoError(t, pool.ExpectationsWereMet())
		})
	}
}

func TestListStoredRecommendationsDisplayDoesNotRequireAzureMigration(t *testing.T) {
	t.Parallel()
	pool := newMock(t)
	defer pool.Close()
	pool.ExpectQuery("SELECT payload FROM recommendations WHERE provider = \\$1").WithArgs("azure").WillReturnRows(pgxmock.NewRows([]string{"payload"}))
	_, err := storeWith(pool).ListStoredRecommendations(context.Background(), RecommendationFilter{Provider: "azure"})
	require.NoError(t, err)
	require.NoError(t, pool.ExpectationsWereMet())
}
