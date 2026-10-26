package providerfactory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	azureProviderPkg = "github.com/LeanerCloud/cloud-commitments-go/providers/azure"
	gcpProviderPkg   = "github.com/LeanerCloud/cloud-commitments-go/providers/gcp"
	providerPkg      = "github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// bypassConstructors are the concrete constructors that skip the provider
// registry. Production code builds credentialed providers through this
// package instead (platform#41).
var bypassConstructors = map[string]map[string]bool{
	azureProviderPkg: {"NewAzureProvider": true},
	gcpProviderPkg:   {"NewProvider": true, "NewProviderWithProject": true, "NewProviderWithCredentials": true},
}

// No non-test file under cmd/ or internal/ may call the concrete Azure/GCP
// provider constructors or set the deprecated ProviderConfig.Profile field.
func TestNoRegistryBypassInProductionCode(t *testing.T) {
	root := filepath.Join("..", "..")
	var violations []string
	scanned := 0
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			violations = append(violations, registryBypasses(t, path)...)
			return nil
		})
		require.NoError(t, err)
	}
	require.Greater(t, scanned, 100, "the walk must actually reach the production sources")
	assert.Empty(t, violations,
		"build credentialed Azure/GCP providers with providerfactory.NewAzure/NewGCP and use the typed ProviderConfig fields")
}

// registryBypasses returns one entry per bypassing call or deprecated
// Profile key in the file at path.
func registryBypasses(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoError(t, err, path)

	imports := map[string]string{} // local name -> import path
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		require.NoError(t, err)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imports[name] = p
	}

	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if pkg := selectorPkg(node, imports); bypassConstructors[pkg][node.Sel.Name] {
				out = append(out, fset.Position(node.Pos()).String()+": "+node.Sel.Name)
			}
		case *ast.CompositeLit:
			if sel, ok := node.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "ProviderConfig" && selectorPkg(sel, imports) == providerPkg {
				out = append(out, profileKeys(fset, node)...)
			}
		}
		return true
	})
	return out
}

func selectorPkg(sel *ast.SelectorExpr, imports map[string]string) string {
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return imports[id.Name]
}

func profileKeys(fset *token.FileSet, lit *ast.CompositeLit) []string {
	var out []string
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Profile" {
			out = append(out, fset.Position(kv.Pos()).String()+": ProviderConfig.Profile")
		}
	}
	return out
}
