package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEveryServiceTypeHasAFrontendLabel guards issues #1649 and #1683: every
// service slug the pinned library defines (every common.ServiceType constant)
// must have a display name in frontend/src/lib/service-labels.ts, so no page
// falls back to the raw slug. It reads the constants from the library source,
// so a constant added by a future bump fails here until it gets a label.
func TestEveryServiceTypeHasAFrontendLabel(t *testing.T) {
	slugs := libraryServiceTypes(t)
	require.Contains(t, slugs, "data-warehouse", "sanity check: the parser must find the library constants")

	src, err := os.ReadFile("../../frontend/src/lib/service-labels.ts")
	require.NoError(t, err)
	block := regexp.MustCompile(`(?s)const SERVICE_LABELS[^{]*\{(.*?)\n\};`).FindSubmatch(src)
	require.NotNil(t, block, "SERVICE_LABELS map not found in service-labels.ts")
	labeled := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*'?([a-z0-9-]+)'?\s*:`).FindAllSubmatch(block[1], -1) {
		labeled[string(m[1])] = true
	}

	for _, slug := range slugs {
		require.Truef(t, labeled[slug], "service slug %q has no label in frontend/src/lib/service-labels.ts", slug)
	}
}

// libraryServiceTypes returns the values of every ServiceType constant in the
// pinned cloud-commitments-go common package.
func libraryServiceTypes(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/LeanerCloud/cloud-commitments-go/pkg").Output()
	require.NoError(t, err, "locate the pinned cloud-commitments-go/pkg module")
	dir := filepath.Join(strings.TrimSpace(string(out)), "common")

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)

	var slugs []string
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		slugs = append(slugs, serviceTypeConsts(t, file)...)
	}
	require.NotEmpty(t, slugs)
	return slugs
}

// serviceTypeConsts returns the string values of the ServiceType constants
// declared in file.
func serviceTypeConsts(t *testing.T, file *ast.File) []string {
	t.Helper()
	var slugs []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "ServiceType" {
				continue
			}
			for _, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					slugs = append(slugs, s)
				}
			}
		}
	}
	return slugs
}
