package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// deleteFromPurchaseExecutions matches a DELETE or TRUNCATE of the
// purchase_executions table, schema-qualified and/or double-quoted, and not
// of a table whose name merely starts with it.
var deleteFromPurchaseExecutions = regexp.MustCompile(`(?i)\b(?:DELETE\s+FROM|TRUNCATE(?:\s+TABLE)?)\s+(?:ONLY\s+)?(?:[\w".]+\s*,\s*)*(?:"?\w+"?\.)?"?purchase_executions"?(?:[^\w"]|$)`)

// TestPurchaseExecutionsDeletedOnlyByConfigStore guards #357. A second
// retention job, cmd/cleanup-lambda, deleted purchase_executions rows with its
// own predicate outside this package. It drifted from CleanupOldExecutions
// twice (no status guard at all until #1173; a TTL column nothing writes, so
// it never deleted a row) and was removed. Retention now lives only in this
// package, where CleanupOldExecutions keeps the live-status and health-score
// guards next to the rest of the execution state machine.
//
// This scans every non-test Go file under internal/ and cmd/ outside
// internal/config for a string literal that deletes from or truncates
// purchase_executions. A new deleter belongs in the store, behind the same
// guards; add it there rather than widening this test.
//
// It is a best-effort tripwire, like TestNoUnscopedRecommendationsDelete: it
// sees one string literal at a time, so SQL assembled from separate pieces
// ("DELETE FROM " + table) gets past it.
func TestPurchaseExecutionsDeletedOnlyByConfigStore(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	configDir := filepath.Dir(self)
	root := filepath.Join(configDir, "..", "..")
	fset := token.NewFileSet()
	scanned := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || path == configDir) {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			scanned++
			for _, lit := range purchaseExecutionsDeletes(f) {
				t.Errorf("%s: deletes from purchase_executions outside internal/config; put the delete in the store next to CleanupOldExecutions so it keeps the live-status guards",
					fset.Position(lit.Pos()))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files; the guard is not looking where it should")
	}
}

// TestPurchaseExecutionsDeletesMatcher pins what the scan above flags, so a
// broken matcher cannot turn the guard into a silent pass.
func TestPurchaseExecutionsDeletesMatcher(t *testing.T) {
	src := "package p\n" +
		"const cleanupLambda = \"DELETE FROM purchase_executions WHERE \"\n" +
		"const lower = `delete from  purchase_executions where expires_at < $1`\n" +
		"const qualified = `DELETE FROM ONLY \"public\".\"purchase_executions\" e WHERE e.status = $1`\n" +
		"const truncate = `TRUNCATE TABLE foo, purchase_executions CASCADE`\n" +
		"const commentOnly = `SELECT 1 -- DELETE FROM purchase_executions`\n" +
		"const read = `SELECT * FROM purchase_executions`\n" +
		"const otherTable = `DELETE FROM purchase_executions_archive`\n" +
		"const otherQuoted = `DELETE FROM \"purchase_executions_archive\"`\n"
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	flagged := purchaseExecutionsDeletes(f)
	got := make([]string, 0, len(flagged))
	for _, lit := range flagged {
		got = append(got, lit.Value)
	}
	want := []string{
		`"DELETE FROM purchase_executions WHERE "`,
		"`delete from  purchase_executions where expires_at < $1`",
		"`DELETE FROM ONLY \"public\".\"purchase_executions\" e WHERE e.status = $1`",
		"`TRUNCATE TABLE foo, purchase_executions CASCADE`",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("flagged %q, want %q", got, want)
	}
}

// purchaseExecutionsDeletes returns every string literal in f that deletes
// from or truncates purchase_executions outside SQL comments.
func purchaseExecutionsDeletes(f *ast.File) []*ast.BasicLit {
	var found []*ast.BasicLit
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		sql, err := strconv.Unquote(lit.Value)
		if err != nil {
			sql = lit.Value
		}
		if deleteFromPurchaseExecutions.MatchString(sqlComment.ReplaceAllString(sql, " ")) {
			found = append(found, lit)
		}
		return true
	})
	return found
}
