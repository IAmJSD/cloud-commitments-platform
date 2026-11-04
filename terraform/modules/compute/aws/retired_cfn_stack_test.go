// Guards for issue #1432. The CloudFormation install stack still created
// DynamoDB tables and set no DB_HOST, so the Postgres-only server it deployed
// failed at startup on every request. The stack was retired in favor of the
// Terraform installs; these tests keep it from coming back, whole or as a
// stale pointer in the docs, scripts or tests.
package aws_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dynamoDBTable matches a DynamoDB table resource in CloudFormation or
// Terraform. The server keeps all state in Postgres, so a template that
// provisions one is deploying storage the server never reads.
var dynamoDBTable = regexp.MustCompile(`AWS::DynamoDB::Table\b|resource\s+"aws_dynamodb_table"`)

// iacRoots hold every template the repo ships for deploying CUDly or for a
// customer to deploy.
var iacRoots = []string{
	"terraform",
	"cloudformation",
	"iac",
	"arm",
	filepath.Join("internal", "iacfiles", "templates"),
}

var iacExtensions = map[string]bool{".tf": true, ".yaml": true, ".yml": true, ".json": true, ".tmpl": true}

// retiredStackRef matches the retired stack's directory but not its
// CUDly-CrossAccount sibling, which stays. Split so this file does not match.
var retiredStackRef = regexp.MustCompile(`cloudformation/stacks/` + `CUDly(?:[^-\w]|$)`)

// historicalPaths record past state on purpose and keep the old path.
var historicalPaths = []string{
	filepath.Join("docs", "audits"),
	"known_issues",
	"CHANGELOG.md",
}

// skippedDirs are not part of the shipped source this guard reads. frontend/
// has no IaC references and is large.
var skippedDirs = map[string]bool{".git": true, "node_modules": true, ".terraform": true, "frontend": true}

const maxTextFileSize = 4 << 20

var textExtensions = map[string]bool{
	".go": true, ".md": true, ".sh": true, ".tf": true, ".hcl": true, ".tmpl": true,
	".yaml": true, ".yml": true, ".json": true, ".txt": true, ".mk": true, "": true,
}

// walkText calls visit with the repo-relative path and content of every file
// under dir (repo-relative) whose extension keep accepts.
func walkText(t *testing.T, dir string, keep map[string]bool, visit func(rel, content string)) {
	t.Helper()
	root := repoRoot(t)
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !keep[filepath.Ext(path)] {
			return nil
		}
		// Extension-less files include locally built binaries; source and
		// docs are far below this size.
		if info, infoErr := d.Info(); infoErr != nil || info.Size() > maxTextFileSize {
			return infoErr
		}
		data, readErr := os.ReadFile(path) // #nosec G304 -- paths come from walking the repo
		if readErr != nil {
			return readErr
		}
		visit(rel, string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
}

func TestNoIaCProvisionsDynamoDB(t *testing.T) {
	for _, dir := range iacRoots {
		files := 0
		walkText(t, dir, iacExtensions, func(rel, content string) {
			files++
			if loc := dynamoDBTable.FindStringIndex(content); loc != nil {
				line := strings.Count(content[:loc[0]], "\n") + 1
				t.Errorf("%s:%d provisions a DynamoDB table; the server stores everything in Postgres (DB_HOST), so this table is never read and its stack cannot start the server (#1432)", rel, line)
			}
		})
		if files == 0 {
			t.Errorf("%s: no IaC files read; the guard would pass by looking at nothing", dir)
		}
	}
}

func TestRetiredCloudFormationStackIsNotReferenced(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "cloudformation", "stacks", "CUDly")); !os.IsNotExist(err) {
		t.Errorf("the CloudFormation install stack directory exists again (stat err: %v); it was retired in #1432 because it cannot start the Postgres-only server. Use the Terraform installs", err)
	}
	files := 0
	walkText(t, ".", textExtensions, func(rel, content string) {
		for _, h := range historicalPaths {
			if rel == h || strings.HasPrefix(rel, h+string(filepath.Separator)) {
				return
			}
		}
		files++
		if loc := retiredStackRef.FindStringIndex(content); loc != nil {
			line := strings.Count(content[:loc[0]], "\n") + 1
			t.Errorf("%s:%d still points at the retired CloudFormation install stack (#1432); point it at the Terraform installs instead", rel, line)
		}
	})
	if files < 100 {
		t.Errorf("read only %d files; the walk is not reaching the repository", files)
	}
}
