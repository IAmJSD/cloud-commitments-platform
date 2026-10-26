package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each case asserts the exact rendered findings, file and line included: an
// exit-code-only assertion would accept a guard that fires on the wrong line.
func TestScanSource(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "resource form",
			src: `resource "azurerm_key_vault_access_policy" "workload" {
  key_vault_id = var.key_vault_id
}
`,
			want: []string{`f.tf:1: resource "azurerm_key_vault_access_policy" "workload" {`},
		},
		{
			name: "resource form without separators or quotes",
			src: `resource"azurerm_key_vault_access_policy""a"{
}
resource azurerm_key_vault_access_policy b {
}
`,
			want: []string{
				`f.tf:1: resource"azurerm_key_vault_access_policy""a"{`,
				`f.tf:3: resource azurerm_key_vault_access_policy b {`,
			},
		},
		{
			name: "nested static and dynamic forms at any indentation",
			src: `resource "azurerm_key_vault" "main" {
  name = "kv"
access_policy {
    object_id = "x"
  }
        dynamic "access_policy" {
    for_each = var.policies
    content {
      object_id = access_policy.value
    }
  }
  network_acls {
    default_action = "Deny"
  }
  dynamic "network_acls" {
    for_each = []
    content {}
  }
  dynamic {
  }
}
`,
			want: []string{
				`f.tf:3: access_policy {  (inside resource "azurerm_key_vault" "main")`,
				`f.tf:6: dynamic "access_policy" {  (inside resource "azurerm_key_vault" "main")`,
			},
		},
		{
			name: "access_policy block in another resource is not a vault grant",
			src: `resource "azurerm_storage_share" "share" {
  acl {
    id = "x"
    access_policy {
      permissions = "r"
    }
  }
}
resource "azurerm_container_app" "app" {
  access_policy {
    mode = "metadata"
  }
  dynamic "access_policy" {
    for_each = []
    content {}
  }
}
`,
		},
		{
			name: "prefix-named and same-named attributes",
			src: `resource "azurerm_key_vault" "main" {
  access_policy_enabled = false
}
resource "azurerm_other" "x" {
  access_policy = "metadata"
}
`,
		},
		{
			name: "access_policy deeper than a direct child of the vault",
			src: `resource "azurerm_key_vault" "main" {
  network_acls {
    access_policy {}
  }
}
`,
		},
		{
			name: "data source and other block types",
			src: `data "azurerm_key_vault" "existing" {
  name = "kv"
}
module "azurerm_key_vault_access_policy" {
  source = "./x"
}
locals {
  access_policy = {}
}
`,
		},
		{
			name: "comments",
			src: `# resource "azurerm_key_vault_access_policy" "c" {
// resource "azurerm_key_vault_access_policy" "c" {
/*
resource "azurerm_key_vault_access_policy" "c" {
}
*/
resource "azurerm_key_vault" "main" {
  # access_policy {
  /* dynamic "access_policy" { */
}
`,
		},
		{
			name: "heredoc text is ignored and an adjacent declaration is caught",
			src: `locals {
  doc = <<-EOT
    resource "azurerm_key_vault_access_policy" "in_heredoc" {
    }
    access_policy {
    }
  EOT
}
resource "azurerm_key_vault_access_policy" "real" {
}
`,
			want: []string{`f.tf:9: resource "azurerm_key_vault_access_policy" "real" {`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, diags := scanSource([]byte(tc.src), "f.tf")
			require.False(t, diags.HasErrors(), diags.Error())
			got := make([]string, 0, len(found))
			for _, f := range found {
				got = append(got, f.String())
			}
			assert.Equal(t, tc.want, emptyAsNil(got))
		})
	}
}

func emptyAsNil(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func TestRunExitCodes(t *testing.T) {
	dir := t.TempDir()
	clean := writeFile(t, dir, "clean.tf", "resource \"azurerm_key_vault\" \"main\" {\n}\n")
	bad := writeFile(t, dir, "bad.tf", "resource \"azurerm_key_vault\" \"main\" {\n  access_policy {\n  }\n}\n")
	broken := writeFile(t, dir, "broken.tf", "resource \"azurerm_key_vault\" \"main\" {\n")
	jsonFile := writeFile(t, dir, "x.tf.json", "{}\n")

	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "clean", args: []string{clean}, wantCode: exitClean},
		{name: "violation", args: []string{clean, bad}, wantCode: exitViolation,
			wantStdout: bad + `:2: access_policy {  (inside resource "azurerm_key_vault" "main")`},
		{name: "no args", wantCode: exitCannotCheck, wantStderr: "usage:"},
		{name: "missing file", args: []string{filepath.Join(dir, "nope.tf")}, wantCode: exitCannotCheck, wantStderr: "nope.tf"},
		// A file that does not parse must not be reported clean, even next to
		// one with a real finding.
		{name: "parse error", args: []string{bad, broken}, wantCode: exitCannotCheck, wantStderr: "does not parse as HCL"},
		{name: "json encoding", args: []string{jsonFile}, wantCode: exitCannotCheck, wantStderr: "JSON encoding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantStdout, strings.TrimSpace(stdout.String()))
			assert.Contains(t, stderr.String(), tc.wantStderr)
		})
	}
}
