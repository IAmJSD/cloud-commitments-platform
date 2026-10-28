package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prebuiltMigrateSources are the places a prebuilt golang-migrate binary comes
// from: upstream's release tarballs and its Docker Hub image (via FROM or
// COPY --from). A prebuilt binary carries whatever Go toolchain and dependency
// pins upstream built it with, so no base-image or go.mod bump in this repo
// reaches it (issues #1833 and #203).
var prebuiltMigrateSources = []string{
	"golang-migrate/migrate/releases",
	"migrate/migrate:",
	"migrate/migrate@",
}

// TestDockerfilesBuildMigrateIdentically pins the development image's
// golang-migrate binary to the production one. Both Dockerfiles must build it
// from source with one identical RUN instruction, in a stage based on the same
// digest-pinned Go image, and neither may download a prebuilt release.
//
// The two images are separate Dockerfiles, so they cannot share a build stage;
// this test is what stops them drifting apart again. Dockerfile.dev downloaded
// the v4.17.0 release tarball long after Dockerfile switched to building
// v4.19.1 from this module (issue #203).
func TestDockerfilesBuildMigrateIdentically(t *testing.T) {
	root := mainModuleRoot(t)
	prod := parseDockerfile(t, filepath.Join(root, "Dockerfile"))
	dev := parseDockerfile(t, filepath.Join(root, "Dockerfile.dev"))

	for name, instrs := range map[string][]string{"Dockerfile": prod, "Dockerfile.dev": dev} {
		for _, instr := range instrs {
			for _, src := range prebuiltMigrateSources {
				assert.NotContains(t, instr, src,
					"%s pulls in a prebuilt golang-migrate binary; build it from this module "+
						"instead, as the RUN instruction in Dockerfile does", name)
			}
		}
	}

	prodRun, prodBase := migrateBuildInstruction(t, "Dockerfile", prod)
	devRun, devBase := migrateBuildInstruction(t, "Dockerfile.dev", dev)

	assert.Contains(t, prodRun, " go build ",
		"Dockerfile must build migrate with `go build` as a package of this module")
	assert.NotContains(t, prodRun, migrateCLIPackage+"@",
		"an @version suffix resolves golang-migrate's own go.mod instead of this module's")
	assert.Contains(t, prodRun, "-tags="+migrateCLIBuildTag,
		"Dockerfile must build migrate with -tags=%s so lib/pq is not linked", migrateCLIBuildTag)

	assert.Equal(t, prodRun, devRun,
		"Dockerfile.dev must build golang-migrate with exactly the RUN instruction Dockerfile uses")
	assert.Contains(t, prodBase, "@sha256:", "Dockerfile's migrate build stage must be digest-pinned")
	assert.Equal(t, prodBase, devBase,
		"Dockerfile.dev must build golang-migrate on the same pinned Go image as Dockerfile")
}

// migrateBuildInstruction returns the single RUN instruction in instrs that
// builds the migrate CLI, and the base image of the stage it runs in.
func migrateBuildInstruction(t *testing.T, name string, instrs []string) (run, base string) {
	t.Helper()

	var runs []string
	stageBase := ""
	for _, instr := range instrs {
		keyword, args, _ := strings.Cut(instr, " ")
		switch strings.ToUpper(keyword) {
		case "FROM":
			stageBase = stageBaseImage(args)
		case "RUN":
			if strings.Contains(args, migrateCLIPackage) {
				runs = append(runs, instr)
				run, base = instr, stageBase
			}
		}
	}

	require.Lenf(t, runs, 1, "%s must contain exactly one RUN instruction that builds %s, found %d: %q",
		name, migrateCLIPackage, len(runs), runs)
	return run, base
}

// stageBaseImage returns the image reference of a FROM instruction's
// arguments, skipping flags such as --platform and dropping any `AS name`.
func stageBaseImage(args string) string {
	for _, field := range strings.Fields(args) {
		if !strings.HasPrefix(field, "--") {
			return field
		}
	}
	return ""
}

// parseDockerfile returns the file's instructions with line continuations
// joined, comment and blank lines dropped (Docker strips both, including inside
// a continued instruction), and runs of whitespace collapsed to one space so
// that indentation differences do not count as drift. Neither Dockerfile sets
// an escape directive, so the continuation character is the default backslash.
func parseDockerfile(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoErrorf(t, err, "reading %s", path)

	var instrs []string
	var cur strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		body, continued := strings.CutSuffix(trimmed, `\`)
		cur.WriteString(body)
		cur.WriteString(" ")
		if continued {
			continue
		}
		instrs = append(instrs, strings.Join(strings.Fields(cur.String()), " "))
		cur.Reset()
	}
	require.Emptyf(t, strings.TrimSpace(cur.String()), "%s ends inside a continued instruction", path)
	require.NotEmptyf(t, instrs, "%s has no instructions", path)

	return instrs
}
