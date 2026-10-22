package migrations

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migrateCLIBuildSources lists every file that puts a golang-migrate CLI binary
// on a machine or into an image. Each must build migrateCLIPackage as a package
// of this module with -tags=pgx5, the way Dockerfile documents: no @version
// suffix (which would resolve golang-migrate's own superseded dependency pins)
// and no upstream release tarball (which carries upstream's toolchain and
// dependency set, and the lib/pq-backed postgres driver). Dockerfile.dev was
// left on a v4.17.0 tarball after every other source moved (issue #45), so the
// dev container ran an older migrate, with lib/pq, than production.
var migrateCLIBuildSources = []string{
	"Dockerfile",
	"Dockerfile.dev",
	"Makefile",
	".github/workflows/ci.yml",
	".github/workflows/database-migration.yml",
}

var (
	// migrateReleaseDownload matches a download of an upstream migrate release
	// artifact, e.g. .../golang-migrate/migrate/releases/download/v4.17.0/...
	migrateReleaseDownload = regexp.MustCompile(`golang-migrate/migrate/releases/download/`)
	// migrateCLIBuildCmd matches a `go build` or `go install` command.
	migrateCLIBuildCmd = regexp.MustCompile(`\bgo\s+(build|install)\b`)
	// migrateCLIPgx5Tag matches -tags=pgx5, -tags pgx5 and -tags 'pgx5'.
	migrateCLIPgx5Tag = regexp.MustCompile(`-tags[=\s]+['"]?` + migrateCLIBuildTag + `['"]?(\s|$)`)
)

// TestMigrateCLI_BuiltFromModuleWithPgx5 guards issue #45: every source that
// installs the migrate CLI builds it from this module's go.mod with the pgx5
// tag, so the dev image, CI, the Makefile and the shipped image all run the
// same migrate version with the same driver.
func TestMigrateCLI_BuiltFromModuleWithPgx5(t *testing.T) {
	root := mainModuleRoot(t)
	for _, rel := range migrateCLIBuildSources {
		t.Run(rel, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join(root, rel))
			require.NoError(t, err)

			cmds := shellCommands(string(content))
			var builds int
			for _, cmd := range cmds {
				assert.False(t, migrateReleaseDownload.MatchString(cmd),
					"%s downloads a prebuilt golang-migrate release; build %s from this module instead: %s",
					rel, migrateCLIPackage, cmd)
				if !migrateCLIBuildCmd.MatchString(cmd) || !strings.Contains(cmd, migrateCLIPackage) {
					continue
				}
				builds++
				assert.NotContains(t, cmd, migrateCLIPackage+"@",
					"%s pins migrate with an @version suffix, which resolves golang-migrate's own go.mod: %s",
					rel, cmd)
				assert.Regexp(t, migrateCLIPgx5Tag, cmd,
					"%s must build migrate with -tags=%s: %s", rel, migrateCLIBuildTag, cmd)
			}
			assert.NotZero(t, builds,
				"%s does not build %s with go build/install; update this guard if it no longer installs migrate",
				rel, migrateCLIPackage)
		})
	}
}

// shellCommands returns the non-comment lines of a Dockerfile, Makefile or
// workflow with backslash continuations joined, so a multi-line RUN reads as
// one command.
func shellCommands(text string) []string {
	var cmds []string
	var cur strings.Builder
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if cont, ok := strings.CutSuffix(line, `\`); ok {
			cur.WriteString(cont)
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(line)
		if cmd := strings.TrimSpace(cur.String()); cmd != "" {
			cmds = append(cmds, cmd)
		}
		cur.Reset()
	}
	if cmd := strings.TrimSpace(cur.String()); cmd != "" {
		cmds = append(cmds, cmd)
	}
	return cmds
}

// TestShellCommands_JoinsContinuationsAndDropsComments pins the parser the
// guard above relies on: a regression here would make it miss a split command.
func TestShellCommands_JoinsContinuationsAndDropsComments(t *testing.T) {
	text := "# go install x@v1\nRUN a && \\\n    # comment inside\n    go build \\\n      -tags=pgx5 pkg\nCMD b\n"
	assert.Equal(t, []string{"RUN a &&  go build  -tags=pgx5 pkg", "CMD b"}, shellCommands(text))
}
