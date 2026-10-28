package api

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// providerWiringFile is the one non-test file in this package allowed to
// import the concrete cloud-commitments-go provider packages (platform#41).
const providerWiringFile = "provider_wiring.go"

// concreteProviderPkgPrefix covers every concrete provider package
// (providers/aws, providers/aws/services/ec2, providers/azure/..., ...).
const concreteProviderPkgPrefix = "github.com/LeanerCloud/cloud-commitments-go/providers/"

// riExchangeForbiddenImportPrefixes are the imports handler_ri_exchange.go
// must not take: concrete provider packages, the Azure SDK, and the AWS SDK's
// config loader and service clients. The generic aws.Config value type and
// smithy's error interface stay allowed.
var riExchangeForbiddenImportPrefixes = []string{
	concreteProviderPkgPrefix,
	"github.com/Azure/azure-sdk-for-go/",
	"github.com/aws/aws-sdk-go-v2/config",
	"github.com/aws/aws-sdk-go-v2/service/",
}

// fileImports returns the import paths of the Go file at path.
func fileImports(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	require.NoError(t, err, path)
	out := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		require.NoError(t, err, path)
		out = append(out, p)
	}
	return out
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// The RI exchange HTTP handler reaches AWS and Azure only through the narrow
// client interfaces; the concrete constructors and types live in
// provider_wiring.go.
func TestRIExchangeHandlerHasNoConcreteProviderImports(t *testing.T) {
	imports := fileImports(t, "handler_ri_exchange.go")
	require.NotEmpty(t, imports, "the guard must actually read the handler's imports")
	var bad []string
	for _, p := range imports {
		if hasAnyPrefix(p, riExchangeForbiddenImportPrefixes) {
			bad = append(bad, p)
		}
	}
	assert.Empty(t, bad, "move concrete provider construction and types into %s", providerWiringFile)
}

// No non-test file in this package other than provider_wiring.go imports a
// concrete cloud-commitments-go provider package.
func TestOnlyProviderWiringImportsConcreteProviders(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var bad []string
	scanned := 0
	wiringImportsProviders := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		for _, p := range fileImports(t, filepath.Join(".", name)) {
			if !strings.HasPrefix(p, concreteProviderPkgPrefix) {
				continue
			}
			if name == providerWiringFile {
				wiringImportsProviders = true
				continue
			}
			bad = append(bad, name+": "+p)
		}
	}
	require.Greater(t, scanned, 50, "the scan must actually reach the package sources")
	assert.Empty(t, bad, "import concrete provider packages only from %s", providerWiringFile)
	assert.True(t, wiringImportsProviders, "%s must exist and hold the concrete provider imports", providerWiringFile)
}
