package mocks

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMocks_NoBarePanicOutsideTestReporter keeps #353 closed for code not yet
// written. A mock that panics on a test-authoring error (a mistyped .Return,
// say) ends the whole test binary: the remaining tests go unrun, TestMain
// teardown is skipped, and the output is a stack trace instead of a failure
// filed under the test's name. Such errors go through testReporter (see
// report.go), whose fail method is the one place allowed to panic, and only
// when no test is registered to report to.
//
// The check parses the package's non-test files, so a panic added to any
// mock later fails here without editing this test.
func TestMocks_NoBarePanicOutsideTestReporter(t *testing.T) {
	const allowed = "testReporter.fail"

	sites := barePanicSites(t)
	var offending []string
	foundAllowed := false
	for _, s := range sites {
		if s.owner == allowed {
			foundAllowed = true
			continue
		}
		offending = append(offending, s.String())
	}

	// Self-check: if the scan stops seeing the one panic it must allow, it
	// has broken and would pass vacuously.
	require.True(t, foundAllowed,
		"the source scan did not find the panic in %s; it has broken (or that panic moved)", allowed)
	assert.Empty(t, offending,
		"bare panic( in internal/mocks: route test-authoring errors through m.reporter "+
			"(wrongReturnType or fail) so they fail the registered test instead of ending the test binary:\n%s",
		strings.Join(offending, "\n"))
}

// panicSite is one call to the panic builtin and the declaration enclosing it.
type panicSite struct {
	pos   token.Position
	owner string // "Recv.Method", "Func", or "package scope"
}

func (s panicSite) String() string {
	return s.pos.String() + " in " + s.owner
}

// barePanicSites returns every panic(...) call in the package's non-test files.
func barePanicSites(t *testing.T) []panicSite {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var sites []panicSite
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			owner := declOwner(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				if isPanicCall(n) {
					sites = append(sites, panicSite{pos: fset.Position(n.Pos()), owner: owner})
				}
				return true
			})
		}
	}
	return sites
}

func isPanicCall(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "panic"
}

// declOwner names a top-level declaration: "Recv.Method" for a method,
// the function name for a function, "package scope" for anything else.
func declOwner(decl ast.Decl) string {
	fd, ok := decl.(*ast.FuncDecl)
	if !ok {
		return "package scope"
	}
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}
