package iacbundle

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unzip returns the files in a zip archive, keyed by name.
func unzip(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
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

const roleCall = `module "role" {
  source = "../../../../terraform/modules/iam/azure/cudly-reservation-role"
}
`

// TestAddTerraformDir_ShipsAndRewritesModule builds the Azure bundle's
// Terraform from a disk-like tree, the way scripts/generate-federation-iac.go
// does: the role module ships under terraform/modules/, the call points at
// it, hidden files stay out, and every local source resolves in the zip.
func TestAddTerraformDir_ShipsAndRewritesModule(t *testing.T) {
	src := fstest.MapFS{
		"azure-target/terraform/main.tf":                         {Data: []byte(roleCall)},
		"azure-target/terraform/variables.tf":                    {Data: []byte(`variable "x" {}`)},
		"azure-target/terraform/tests/guards.tftest.hcl":         {Data: []byte(`run "x" {}`)},
		"azure-target/terraform/.terraform.lock.hcl":             {Data: []byte(`lock`)},
		"azure-target/terraform/.terraform/modules/modules.json": {Data: []byte(`{}`)},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	require.NoError(t, AddTerraformDir(zw, src, "azure-target/terraform", ExtraModules("azure")))
	require.NoError(t, zw.Close())
	files := unzip(t, buf.Bytes())

	assert.Contains(t, string(files["terraform/main.tf"]), `source = "./modules/cudly-reservation-role"`)
	assert.Contains(t, files, "terraform/variables.tf")
	assert.Contains(t, files, "terraform/tests/guards.tftest.hcl")
	assert.Contains(t, files, "terraform/modules/cudly-reservation-role/main.tf")
	assert.NotContains(t, files, "terraform/.terraform.lock.hcl")
	assert.NotContains(t, files, "terraform/.terraform/modules/modules.json")
	assert.Empty(t, UnresolvedLocalModules(files))
}

// TestAddTerraformDir_UncalledModuleIsAnError keeps the rewrite honest: a
// module nothing calls means the call moved and the rewrite stopped applying.
func TestAddTerraformDir_UncalledModuleIsAnError(t *testing.T) {
	src := fstest.MapFS{"azure-target/terraform/main.tf": {Data: []byte(`variable "x" {}`)}}
	zw := zip.NewWriter(io.Discard)
	err := AddTerraformDir(zw, src, "azure-target/terraform", ExtraModules("azure"))
	require.ErrorContains(t, err, "cudly-reservation-role")
}

func TestExtraModules_OnlyAzure(t *testing.T) {
	for _, target := range []string{"aws", "gcp", ""} {
		assert.Empty(t, ExtraModules(target), target)
	}
	assert.Len(t, ExtraModules("azure"), 1)
}

// TestUnresolvedLocalModules_DetectsEscape proves the guard can fail: a
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
	}, UnresolvedLocalModules(files))
}
