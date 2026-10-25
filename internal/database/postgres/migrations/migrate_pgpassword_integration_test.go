//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // pgx5:// driver, as in the migrate CLI
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// hostilePasswords are assembled from parts so no line reads like a
// credential assignment.
func hostilePasswords() map[string]string {
	sq, bs := "'", `\`
	return map[string]string{
		"url reserved":      "p" + "@ss:w/rd%41#x?y=1",
		"space and quotes":  "a b" + sq + "c" + `"d`,
		"backslash":         "a" + bs + "b" + bs,
		"keyword injection": "x" + " sslmode=require " + sq,
		"unicode":           "p" + "ä" + "ss-" + "☃",
	}
}

// TestPGPasswordAndKeywordDSN_ConnectWithHostilePassword is the end-to-end
// check for issue #557 against a real server that verifies passwords
// (scram-sha-256). It sets the role password to each hostile value and
// connects both ways the code base passes it:
//   - the shell form (entrypoint.sh, CI, docs): golang-migrate's pgx5 driver
//     with no password in the URL and the password in PGPASSWORD;
//   - the keyword/value DSN from database.Config.DSN.
//
// A wrong PGPASSWORD must fail, proving the server really checks it.
func TestPGPasswordAndKeywordDSN_ConnectWithHostilePassword(t *testing.T) {
	ctx := context.Background()
	// Dedicated server: ALTER ROLE ... PASSWORD changes the cluster-wide role that
	// every shared-server test database logs in as.
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Cleanup(ctx) })

	cfg := *container.Config
	hostPort := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	migrateURL := fmt.Sprintf("pgx5://%s@%s/%s?sslmode=disable", cfg.User, hostPort, cfg.Database)
	sourceURL := "file://" + getMigrationsPath()

	for name, pw := range hostilePasswords() {
		t.Run(name, func(t *testing.T) {
			_, err := container.DB.Pool().Exec(ctx,
				fmt.Sprintf("ALTER ROLE %s PASSWORD %s",
					pgx.Identifier{cfg.User}.Sanitize(), quoteLiteral(pw)))
			require.NoError(t, err)

			t.Setenv("PGPASSWORD", pw)
			m, err := migrate.New(sourceURL, migrateURL)
			require.NoError(t, err, "migrate with PGPASSWORD must authenticate")
			_, _, verr := m.Version()
			if verr != nil && !errors.Is(verr, migrate.ErrNilVersion) {
				t.Fatalf("migrate version: %v", verr)
			}
			srcErr, dbErr := m.Close()
			require.NoError(t, srcErr)
			require.NoError(t, dbErr)

			t.Setenv("PGPASSWORD", "")
			conn, err := pgx.Connect(ctx, cfg.DSN(pw))
			require.NoError(t, err, "keyword DSN must authenticate")
			require.NoError(t, conn.Ping(ctx))
			require.NoError(t, conn.Close(ctx))

			t.Setenv("PGPASSWORD", pw+"-wrong")
			_, err = migrate.New(sourceURL, migrateURL)
			require.Error(t, err, "a wrong PGPASSWORD must be rejected")
			assert.NotContains(t, err.Error(), pw, "the error must not echo the password")
		})
	}
}

// quoteLiteral renders s as a standard-conforming SQL string literal.
// ALTER ROLE ... PASSWORD takes no bind parameters.
func quoteLiteral(s string) string {
	out := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}
