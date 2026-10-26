// check-azure-kv-access-policy parses Terraform files with the HCL native-syntax
// parser and reports every Azure Key Vault access-policy grant they declare.
//
// It is the parsing half of scripts/check-azure-kv-access-policy.sh, which
// selects the files, rejects the JSON encoding and explains the remediation.
// Run that script rather than this binary directly.
//
// Three spellings express the same grant, and all three are reported:
//
//   - a top-level `resource "azurerm_key_vault_access_policy"` block;
//   - an `access_policy { ... }` block directly inside an
//     `azurerm_key_vault` resource;
//   - a `dynamic "access_policy" { ... }` block directly inside an
//     `azurerm_key_vault` resource.
//
// The parse is what makes the nested forms decidable (issue #204). An
// `access_policy` block is only a Key Vault grant when the resource that
// encloses it is an azurerm_key_vault: other providers' resources
// (azurerm_storage_share's acl, for one) have an unrelated block of the same
// name. A line matcher has no enclosing type to check, and it cannot tell a
// comment or heredoc body from a declaration. The parser gives both directly.
//
// Output: one "FILE:LINE: HEADER" line per grant on stdout.
// Exit 0 = no grant found, 1 = at least one found, 2 = cannot check (usage
// error, unreadable file, JSON encoding, or a file that does not parse). A file
// that does not parse is never reported clean.
//
// Usage:
//
//	go run ./cmd/check-azure-kv-access-policy FILE...
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

const (
	bannedResource   = "azurerm_key_vault_access_policy"
	keyVaultResource = "azurerm_key_vault"
	nestedBlock      = "access_policy"
)

const (
	exitClean       = 0
	exitViolation   = 1
	exitCannotCheck = 2
)

// finding is one access-policy grant located in a parsed file.
type finding struct {
	File   string
	Header string
	// Inside names the enclosing resource for a nested grant; it is empty for
	// the top-level resource form.
	Inside string
	Line   int
}

// String renders the finding as FILE:LINE: HEADER, naming the enclosing
// resource for a nested grant.
func (f finding) String() string {
	s := fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Header)
	if f.Inside != "" {
		s += "  (inside " + f.Inside + ")"
	}
	return s
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run scans every file named in args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: check-azure-kv-access-policy FILE...")
		return exitCannotCheck
	}
	var all []finding
	for _, path := range args {
		found, err := scanFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "ERROR: %v\n", err)
			return exitCannotCheck
		}
		all = append(all, found...)
	}
	for _, f := range all {
		fmt.Fprintln(stdout, f) // #nosec G705 -- plain-text CI report on stdout, never rendered as HTML
	}
	if len(all) > 0 {
		return exitViolation
	}
	return exitClean
}

// scanFile reads and scans one file. Any file it cannot fully parse is an
// error, never an empty result.
func scanFile(path string) ([]finding, error) {
	if strings.HasSuffix(path, ".json") {
		return nil, fmt.Errorf("%s: the Terraform JSON encoding is not checked; refusing to report it clean", path)
	}
	// #nosec G304 G703 -- a local CI tool reading the files it was asked to
	// check; the paths come from its own argv, not from a remote caller.
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	found, diags := scanSource(src, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s does not parse as HCL, so it cannot be checked:\n%s", path, diags.Error())
	}
	return found, nil
}

// scanSource parses src as HCL native syntax and returns every grant in it.
func scanSource(src []byte, filename string) ([]finding, hcl.Diagnostics) {
	file, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("unexpected body type %T", file.Body),
		}}
	}
	lines := bytes.Split(src, []byte("\n"))
	var out []finding
	for _, block := range body.Blocks {
		out = append(out, scanTopLevel(block, filename, lines)...)
	}
	return out, diags
}

// scanTopLevel checks one top-level block. Only resource blocks can carry a
// grant: data sources expose access_policy as a read-only attribute.
func scanTopLevel(block *hclsyntax.Block, filename string, lines [][]byte) []finding {
	if block.Type != "resource" || len(block.Labels) == 0 {
		return nil
	}
	switch block.Labels[0] {
	case bannedResource:
		return []finding{newFinding(block, filename, lines, "")}
	case keyVaultResource:
		return scanKeyVault(block, filename, lines)
	}
	return nil
}

// scanKeyVault reports the access_policy blocks declared directly inside an
// azurerm_key_vault resource, static or dynamic.
func scanKeyVault(vault *hclsyntax.Block, filename string, lines [][]byte) []finding {
	inside := blockAddress(vault)
	var out []finding
	for _, child := range vault.Body.Blocks {
		if isAccessPolicyBlock(child) {
			out = append(out, newFinding(child, filename, lines, inside))
		}
	}
	return out
}

func isAccessPolicyBlock(b *hclsyntax.Block) bool {
	if b.Type == nestedBlock {
		return true
	}
	return b.Type == "dynamic" && len(b.Labels) == 1 && b.Labels[0] == nestedBlock
}

// blockAddress renders a block header as written in HCL, e.g.
// resource "azurerm_key_vault" "main".
func blockAddress(b *hclsyntax.Block) string {
	parts := make([]string, 0, len(b.Labels)+1)
	parts = append(parts, b.Type)
	for _, l := range b.Labels {
		parts = append(parts, strconv.Quote(l))
	}
	return strings.Join(parts, " ")
}

// newFinding records the block's header line, quoting the source text of that
// line so the report shows what the developer wrote.
func newFinding(b *hclsyntax.Block, filename string, lines [][]byte, inside string) finding {
	line := b.TypeRange.Start.Line
	header := blockAddress(b)
	if line >= 1 && line <= len(lines) {
		header = strings.TrimSpace(string(lines[line-1]))
	}
	return finding{File: filename, Line: line, Header: header, Inside: inside}
}
