package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var (
	deleteFromRecommendations = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+recommendations\b`)
	sqlWhere                  = regexp.MustCompile(`(?i)\bWHERE\b`)
)

// TestNoUnscopedRecommendationsDelete guards #159. ReplaceRecommendations ran
// `DELETE FROM recommendations` with no WHERE, so any caller handing it a
// partial or empty snapshot would have wiped every account's rows, not only
// its own. It had no production caller and was removed. The only production
// delete left on the table is UpsertRecommendations' eviction, which is
// scoped to the (provider, account) pairs that collected successfully.
//
// This scans every non-test Go file under internal/ and cmd/ for a string
// literal that deletes from recommendations without a WHERE in the same
// literal. Keep the WHERE in the same literal as the DELETE so the check can
// see it.
func TestNoUnscopedRecommendationsDelete(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	root := filepath.Join(filepath.Dir(self), "..", "..")
	fset := token.NewFileSet()
	scanned := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
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
			for _, lit := range unscopedRecommendationsDeletes(f) {
				t.Errorf("%s: unscoped DELETE FROM recommendations; scope it to the accounts being written (see UpsertRecommendations)",
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

// TestUnscopedRecommendationsDeletesMatcher pins what the scan above flags,
// so a broken matcher cannot turn the guard into a silent pass.
func TestUnscopedRecommendationsDeletesMatcher(t *testing.T) {
	src := "package p\n" +
		"const unscoped = `DELETE FROM recommendations`\n" +
		"const unscopedLower = \"delete from  recommendations;\"\n" +
		"const scoped = `DELETE FROM recommendations WHERE collected_at < $1`\n" +
		"const otherTable = `DELETE FROM recommendations_state`\n"
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	flagged := unscopedRecommendationsDeletes(f)
	got := make([]string, 0, len(flagged))
	for _, lit := range flagged {
		got = append(got, lit.Value)
	}
	want := []string{"`DELETE FROM recommendations`", `"delete from  recommendations;"`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("flagged %q, want %q", got, want)
	}
}

// unscopedRecommendationsDeletes returns every string literal in f that
// deletes from the recommendations table without a WHERE clause.
func unscopedRecommendationsDeletes(f *ast.File) []*ast.BasicLit {
	var found []*ast.BasicLit
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if deleteFromRecommendations.MatchString(lit.Value) && !sqlWhere.MatchString(lit.Value) {
			found = append(found, lit)
		}
		return true
	})
	return found
}
