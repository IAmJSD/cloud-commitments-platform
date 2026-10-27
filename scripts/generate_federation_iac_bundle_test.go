package main

import (
	"archive/zip"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/iacbundle"
)

// TestGenerator_BundleLocalModulesResolveInsideZip is the regression test for
// #1442: the script's own --format bundle path zipped only the target's
// Terraform directory, so the Azure bundle's main.tf still called
// ../../../../terraform/modules/iam/azure/cudly-reservation-role and
// `terraform init` on the unzipped bundle failed with "Unreadable module
// directory". It runs every bundle the script can build through the same check
// the API's bundles go through (internal/api, #1431).
func TestGenerator_BundleLocalModulesResolveInsideZip(t *testing.T) {
	combos := []struct {
		name string
		args []string
	}{
		{"aws-cross-account", []string{"--target", "aws", "--source", "aws",
			"--account-id", "123456789012", "--source-account-id", "111122223333"}},
		{"aws-target", []string{"--target", "aws", "--source", "gcp",
			"--account-id", "123456789012", "--oidc-subject-claim", "123456789012345678901"}},
		{"azure-target", []string{"--target", "azure", "--source", "gcp",
			"--account-id", "11111111-2222-3333-4444-555555555555",
			"--tenant-id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"--cudly-api-url", "https://cudly.example.com"}},
		{"gcp-sa-impersonation", []string{"--target", "gcp", "--source", "gcp", "--account-id", "my-project"}},
		{"gcp-target", []string{"--target", "gcp", "--source", "aws",
			"--account-id", "my-project", "--source-account-id", "111122223333"}},
	}
	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "bundle.zip")
			args := append([]string{"--account-name", "Acme", "--format", "bundle", "--output", out}, c.args...)
			res := runGenerator(t, args...)
			require.Zero(t, res.exitCode, "stderr: %s", res.stderr)

			files := readZip(t, out)
			assert.Empty(t, iacbundle.UnresolvedLocalModules(files),
				"every local module source in the %s bundle must resolve to a directory inside the zip", c.name)
			for name := range files {
				assert.NotRegexp(t, `(^|/)\.`, name, "hidden file shipped in the %s bundle", c.name)
			}
		})
	}
}

// readZip returns the files in the zip at path, keyed by name.
func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	require.NoError(t, err)
	defer zr.Close()
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		require.NoError(t, rc.Close())
		require.NoError(t, err)
		files[f.Name] = data
	}
	return files
}
