package api

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localModuleSourceRe matches a module source that is a local path. Registry
// and provider sources never start with "./" or "../".
var localModuleSourceRe = regexp.MustCompile(`(?m)^\s*source\s*=\s*"(\.\.?/[^"]+)"`)

// unresolvedLocalModules returns "file: source" for every local module source
// in a bundle's .tf files that does not point at a directory of .tf files
// inside the bundle. terraform init fails on each of them with "Unreadable
// module directory" (#1431).
func unresolvedLocalModules(files map[string][]byte) []string {
	tfDirs := map[string]bool{}
	for name := range files {
		if strings.HasSuffix(name, ".tf") {
			tfDirs[path.Dir(name)] = true
		}
	}
	var bad []string
	for name, content := range files {
		if !strings.HasSuffix(name, ".tf") {
			continue
		}
		for _, m := range localModuleSourceRe.FindAllSubmatch(content, -1) {
			if !tfDirs[path.Join(path.Dir(name), string(m[1]))] {
				bad = append(bad, name+": "+string(m[1]))
			}
		}
	}
	sort.Strings(bad)
	return bad
}

func TestFederationBundles_LocalModuleSourcesResolveInsideZip(t *testing.T) {
	for _, c := range federationBundleCombos {
		t.Run(c.module, func(t *testing.T) {
			files := renderBundleFiles(t, c.target, c.source, c.sourceCloud)
			assert.Empty(t, unresolvedLocalModules(files),
				"every local module source in the %s bundle must resolve to a directory inside the zip", c.module)
		})
	}
}

// TestUnresolvedLocalModules_DetectsEscape proves the guard above can fail: a
// source that leaves the bundle, or names a directory the zip lacks, is
// reported, and one that resolves inside the zip is not.
func TestUnresolvedLocalModules_DetectsEscape(t *testing.T) {
	files := map[string][]byte{
		"terraform/main.tf": []byte(`module "a" {
  source = "../../../../terraform/modules/iam/azure/cudly-reservation-role"
}
module "b" {
  source = "./modules/missing"
}
module "c" {
  source = "./modules/present"
}
terraform {
  required_providers {
    azurerm = {
      source = "hashicorp/azurerm"
    }
  }
}
`),
		"terraform/modules/present/main.tf": []byte(`resource "null_resource" "x" {}`),
	}
	assert.Equal(t, []string{
		"terraform/main.tf: ../../../../terraform/modules/iam/azure/cudly-reservation-role",
		"terraform/main.tf: ./modules/missing",
	}, unresolvedLocalModules(files))
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
