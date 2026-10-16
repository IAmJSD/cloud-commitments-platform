package migrations

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestBuildMigrateDSN_RoundTripsReservedCharacters builds the DSN from pool
// configs whose credentials contain URL-reserved characters and checks that
// both net/url and pgx recover the exact original values (issue #301).
func TestBuildMigrateDSN_RoundTripsReservedCharacters(t *testing.T) {
	const (
		reserved = "p@ss:word/with?x=1#frag%41 space"
		plus     = "a+b c"
	)
	cases := map[string]struct{ user, password string }{
		"reserved characters": {user: "cudly", password: reserved},
		"literal plus":        {user: "cudly", password: plus},
		"reserved in user":    {user: "us@er:name", password: "plain"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			poolCfg, err := pgxpool.ParseConfig("postgres://x:y@db.example.com:5433/cudly?sslmode=require")
			require.NoError(t, err)
			poolCfg.ConnConfig.User = tc.user
			poolCfg.ConnConfig.Password = tc.password

			dsn := buildMigrateDSN(poolCfg)

			parsed, err := url.Parse(dsn)
			require.NoError(t, err)
			password, _ := parsed.User.Password()
			assert.Equal(t, tc.user, parsed.User.Username())
			assert.Equal(t, tc.password, password)
			assert.Equal(t, "db.example.com", parsed.Hostname())
			assert.Equal(t, "5433", parsed.Port())
			assert.Equal(t, "/cudly", parsed.Path)
			assert.Equal(t, "require", parsed.Query().Get("sslmode"))
			assert.NotContains(t, dsn, "%252541", "the password must not be double-encoded")

			pgxCfg, err := pgx.ParseConfig(strings.Replace(dsn, migrateURLScheme+"://", "postgres://", 1))
			require.NoError(t, err)
			assert.Equal(t, tc.user, pgxCfg.User)
			assert.Equal(t, tc.password, pgxCfg.Password)
			assert.Equal(t, "db.example.com", pgxCfg.Host)
			assert.Equal(t, uint16(5433), pgxCfg.Port)
			assert.Equal(t, "cudly", pgxCfg.Database)
		})
	}
}

// shellMigrationSources lists every file that runs the migrate CLI from shell
// against a pgx5:// URL (issue #557). Each passes the password through
// PGPASSWORD, which pgx reads when the URL has none, so no shell encoder
// (jq, awk) is needed and the secret never appears in the URL or argv.
var shellMigrationSources = []string{
	".github/workflows/ci.yml",
	".github/workflows/database-migration.yml",
	"scripts/entrypoint.sh",
	"docs/DEPLOYMENT.md",
}

// pgx5UserInfo captures the userinfo of a pgx5:// URL in shell text.
var pgx5UserInfo = regexp.MustCompile(`pgx5://([^@/\s"]*)@`)

// migrateWithPGPassword is the only accepted form for a migrate invocation
// whose step (if any) does not set PGPASSWORD in its env.
const migrateWithPGPassword = `PGPASSWORD="$DB_PASSWORD" migrate -path `

// TestShellMigrationURLs_PassPasswordViaPGPASSWORD guards issue #557: no shell
// migration URL carries a password, no shell percent-encoder remains, and
// every migrate invocation gets the password from PGPASSWORD.
func TestShellMigrationURLs_PassPasswordViaPGPASSWORD(t *testing.T) {
	root := mainModuleRoot(t)
	for _, rel := range shellMigrationSources {
		t.Run(rel, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join(root, rel))
			require.NoError(t, err)
			text := string(content)
			assertNoPasswordInURLs(t, text)
			assert.NotContains(t, text, "ENCODED_PASSWORD", "shell password encoders are gone; use PGPASSWORD")
			assert.NotContains(t, text, "@uri", "shell password encoders are gone; use PGPASSWORD")

			var invocations int
			if strings.HasSuffix(rel, ".yml") {
				invocations = checkWorkflowMigrateSteps(t, content)
			} else {
				invocations = checkMigrateLines(t, text, false)
			}
			assert.NotZero(t, invocations, "no migrate invocation found in %s; update this guard", rel)
		})
	}
}

func assertNoPasswordInURLs(t *testing.T, text string) {
	t.Helper()
	matches := pgx5UserInfo.FindAllStringSubmatch(text, -1)
	assert.NotEmpty(t, matches, "no pgx5:// URL with a user found; update this guard")
	for _, m := range matches {
		assert.NotContains(t, m[1], ":", "migration URL carries a password: %s", m[0])
	}
}

// checkMigrateLines asserts that each `migrate -path` line either runs in a
// step whose env sets PGPASSWORD or sets it itself, and returns the count.
func checkMigrateLines(t *testing.T, script string, envHasPGPassword bool) int {
	t.Helper()
	var n int
	for _, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.Contains(line, "migrate -path ") {
			continue
		}
		n++
		if !envHasPGPassword {
			assert.True(t, strings.HasPrefix(line, migrateWithPGPassword),
				"migrate must get the password from PGPASSWORD: %s", line)
		}
	}
	return n
}

// checkWorkflowMigrateSteps applies checkMigrateLines to every step of a
// workflow, honoring a PGPASSWORD set in the step's env.
func checkWorkflowMigrateSteps(t *testing.T, content []byte) int {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Env map[string]string `yaml:"env"`
				Run string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(content, &wf))
	var n int
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			_, hasEnv := step.Env["PGPASSWORD"]
			n += checkMigrateLines(t, step.Run, hasEnv)
		}
	}
	return n
}
