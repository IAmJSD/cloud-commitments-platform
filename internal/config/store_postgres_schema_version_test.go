package config

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeVersionRow struct {
	version int
	dirty   bool
	err     error
}

func (r fakeVersionRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int) = r.version
	*dest[1].(*bool) = r.dirty
	return nil
}

type fakeVersionQuerier struct{ row fakeVersionRow }

func (q fakeVersionQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }

// main deploys before prod migrations are applied, so the settlement paths
// must know whether the columns exist (#999).
func TestSettlementSchemaReady(t *testing.T) {
	t.Parallel()
	v := settlementMigrationVersion
	cases := []struct {
		name string
		row  fakeVersionRow
		want bool
	}{
		{"nothing applied", fakeVersionRow{err: pgx.ErrNoRows}, false},
		{"before the migration", fakeVersionRow{version: v - 1}, false},
		{"at the migration", fakeVersionRow{version: v}, true},
		{"at the migration, dirty", fakeVersionRow{version: v, dirty: true}, false},
		{"after it, even with a later dirty migration", fakeVersionRow{version: v + 1, dirty: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := settlementSchemaReady(context.Background(), fakeVersionQuerier{tc.row})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
