package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// perTestContainerCalls start a PostgreSQL container of their own for each
// caller. In this package that cost added up to about 15 of the CI integration
// job's 20 minutes (issue #982), so its tests take a database from
// testhelpers.RequireMigratedDatabase instead: one shared container, migrated
// once, with a fresh copy of the migrated database per test.
var perTestContainerCalls = map[string]bool{
	"RequirePostgresContainer": true,
	"SetupPostgresContainer":   true,
}

// TestIntegrationTestsShareOnePostgresContainer keeps the package's integration
// tests on the shared container. It parses rather than compiles, so it sees the
// files behind //go:build integration from a plain `go test` run too.
//
// A test that genuinely needs a server of its own (one that changes server-wide
// settings, say) can still have one: it belongs in a different package, or this
// guard gets an explicit, reviewed exception.
func TestIntegrationTestsShareOnePostgresContainer(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob test files: %v", err)
	}

	fset := token.NewFileSet()
	var offenders []string
	shared := 0
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "testhelpers" {
				return true
			}
			switch {
			case perTestContainerCalls[sel.Sel.Name]:
				offenders = append(offenders, fset.Position(sel.Pos()).String()+": testhelpers."+sel.Sel.Name)
			case sel.Sel.Name == "RequireMigratedDatabase":
				shared++
			}
			return true
		})
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d call(s) start a PostgreSQL container per test; use "+
			"testhelpers.RequireMigratedDatabase so the package keeps sharing one (issue #982):\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}
	// "Found nothing" must not be confused with "looked at nothing".
	if shared == 0 {
		t.Errorf("no call to testhelpers.RequireMigratedDatabase in %d test file(s): the guard is "+
			"not looking where the integration tests are", len(files))
	}
}
