package auth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaredPermissionConsts parses every non-test .go file in this package
// and returns the value of each package-level constant whose name is
// <prefix> followed by an upper-case letter (ActionView, ResourceAll, ...),
// keyed by value and mapped to the constant's name. It reads the source
// rather than a hand-kept list so that a constant added anywhere in the
// package is seen without anyone remembering to register it (issue #166).
func declaredPermissionConsts(t *testing.T, prefix string) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, err, "parse %s", path)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			collectConstSpecs(t, fset, gd, prefix, out)
		}
	}
	return out
}

// collectConstSpecs adds the matching specs of one const declaration to
// out. A spec with no value repeats the previous spec's expression, as the
// Go spec defines for parenthesized const blocks.
func collectConstSpecs(t *testing.T, fset *token.FileSet, gd *ast.GenDecl, prefix string, out map[string]string) {
	t.Helper()
	var prev []ast.Expr
	for _, spec := range gd.Specs {
		vs := spec.(*ast.ValueSpec)
		if len(vs.Values) > 0 {
			prev = vs.Values
		}
		for i, name := range vs.Names {
			if !hasPermissionPrefix(name.Name, prefix) {
				continue
			}
			pos := fset.Position(name.Pos())
			require.Less(t, i, len(prev), "%s: %s has no value", pos, name.Name)
			lit, ok := prev[i].(*ast.BasicLit)
			require.True(t, ok && lit.Kind == token.STRING,
				"%s: %s must be a string literal so the permission catalog test can read it", pos, name.Name)
			val, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			if other, dup := out[val]; dup {
				t.Fatalf("%s: %s and %s both have value %q", pos, other, name.Name, val)
			}
			out[val] = name.Name
		}
	}
}

func hasPermissionPrefix(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsUpper(r)
}

func assertCatalogMatches(t *testing.T, kind string, declared map[string]string, listed []string) {
	t.Helper()
	require.NotEmpty(t, declared, "found no %s constants; did the naming convention change?", kind)
	seen := map[string]bool{}
	for _, v := range listed {
		assert.False(t, seen[v], "%s %q is listed twice in the catalog", kind, v)
		seen[v] = true
		_, ok := declared[v]
		assert.True(t, ok, "%s %q is in the catalog but no %s* constant declares it", kind, v, kind)
	}
	for _, v := range slices.Sorted(maps.Keys(declared)) {
		assert.True(t, seen[v],
			"%s = %q is declared but missing from the catalog in permission_catalog.go", declared[v], v)
	}
}

// TestPermissionCatalogMatchesConstDecls is the exhaustiveness guarantee
// for AllActions / AllResources (issue #166): adding an Action* or
// Resource* constant without listing it, or listing a value no constant
// declares, fails here.
func TestPermissionCatalogMatchesConstDecls(t *testing.T) {
	assertCatalogMatches(t, "Action", declaredPermissionConsts(t, "Action"), AllActions())
	assertCatalogMatches(t, "Resource", declaredPermissionConsts(t, "Resource"), AllResources())
}

func TestPermissionCatalogReturnsCopies(t *testing.T) {
	a := AllActions()
	a[0] = "mutated"
	assert.Equal(t, ActionView, AllActions()[0])
	r := AllResources()
	r[0] = "mutated"
	assert.Equal(t, ResourceRecommendations, AllResources()[0])
}

// TestPermissionCatalogCoversSeededPermissions checks that every permission
// the package itself grants or carves out uses the enumerated vocabulary,
// so a verb spelled as a bare string literal cannot bypass the catalog.
func TestPermissionCatalogCoversSeededPermissions(t *testing.T) {
	actions, resources := AllActions(), AllResources()
	sets := map[string][]Permission{
		"DefaultAdminPermissions":     DefaultAdminPermissions(),
		"DefaultUserPermissions":      DefaultUserPermissions(),
		"DefaultReadOnlyPermissions":  DefaultReadOnlyPermissions(),
		"DefaultPurchaserPermissions": DefaultPurchaserPermissions(),
	}
	for pair := range adminCarvedOuts {
		sets["adminCarvedOuts"] = append(sets["adminCarvedOuts"], Permission{Action: pair[0], Resource: pair[1]})
	}
	for name, perms := range sets {
		for _, p := range perms {
			assert.Contains(t, actions, p.Action, "%s grants unknown action %q", name, p.Action)
			assert.Contains(t, resources, p.Resource, "%s grants unknown resource %q", name, p.Resource)
		}
	}
}
