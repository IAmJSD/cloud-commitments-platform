package api

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/iacbundle"
)

func TestFederationBundles_LocalModuleSourcesResolveInsideZip(t *testing.T) {
	for _, c := range federationBundleCombos {
		t.Run(c.module, func(t *testing.T) {
			files := renderBundleFiles(t, c.target, c.source, c.sourceCloud)
			assert.Empty(t, iacbundle.UnresolvedLocalModules(files),
				"every local module source in the %s bundle must resolve to a directory inside the zip", c.module)
		})
	}
}

// TestAzureBundle_RoleModuleMatchesSharedModule keeps the role module shipped
// in the Azure bundle identical to terraform/modules/iam/azure/
// cudly-reservation-role, which the host side and scripts/check-azure-role-
// parity.sh use, so the customer role cannot drift from the audited one.
func TestAzureBundle_RoleModuleMatchesSharedModule(t *testing.T) {
	const shared = "../../terraform/modules/iam/azure/cudly-reservation-role"
	files := renderBundleFiles(t, "azure", "gcp", "gcp")

	want, err := filepath.Glob(filepath.Join(shared, "*.tf"))
	require.NoError(t, err)
	require.NotEmpty(t, want, "no .tf files in %s", shared)

	var got []string
	for name := range files {
		if strings.HasPrefix(name, "terraform/modules/cudly-reservation-role/") {
			got = append(got, path.Base(name))
		}
	}
	sort.Strings(got)
	wantNames := make([]string, 0, len(want))
	for _, f := range want {
		wantNames = append(wantNames, filepath.Base(f))
	}
	require.Equal(t, wantNames, got, "bundle must ship exactly the shared module's .tf files")

	for _, f := range want {
		disk, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.Equal(t, string(disk), string(files["terraform/modules/cudly-reservation-role/"+filepath.Base(f)]),
			"bundled %s differs from %s", filepath.Base(f), f)
	}
}
