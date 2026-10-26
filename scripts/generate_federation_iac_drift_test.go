package main

// Drift guards for #153. generate-federation-iac.go renders the same templates
// as the server's GET /api/federation/iac, from its own copy of the template
// data struct (iacData, a copy of internal/api's federationIaCData). A comment
// asking people to keep the two in sync did not: iacData went without
// OIDCIssuerHost, which aws-cfn-deploy.sh.tmpl references, so
// `--format bundle` failed for every AWS target with a non-AWS source.
//
// The script carries //go:build ignore and internal/api is a large package, so
// neither struct is imported here. Both are read from source with go/parser,
// and every template is parsed with text/template/parse, so a field added to
// one side or used by a new template fails these tests without coupling the
// script to the server package.

import (
	"archive/zip"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"text/template"
	"text/template/parse"

	"github.com/stretchr/testify/require"
)

// structFieldNames returns the field names of the struct type typeName
// declared in the Go source file at path, sorted.
func structFieldNames(t *testing.T, path, typeName string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	var names []string
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != typeName {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		require.True(t, ok, "%s in %s is not a struct type", typeName, path)
		found = true
		for _, field := range st.Fields.List {
			require.NotEmpty(t, field.Names, "embedded field in %s is not supported by this check", typeName)
			for _, name := range field.Names {
				names = append(names, name.Name)
			}
		}
		return false
	})
	require.True(t, found, "type %s not found in %s", typeName, path)
	sort.Strings(names)
	return names
}

// templateFieldRefs adds to refs every top-level field that node reads from
// the template's data, mapped to the template that reads it. The templates
// never rebind dot, so .X always names a field of the data struct; a with or
// range block would break that assumption, so it fails the test instead of
// being guessed at.
func templateFieldRefs(t *testing.T, tmpl string, node parse.Node, refs map[string]string) {
	t.Helper()
	switch n := node.(type) {
	case nil:
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, child := range n.Nodes {
			templateFieldRefs(t, tmpl, child, refs)
		}
	case *parse.ActionNode:
		templateFieldRefs(t, tmpl, n.Pipe, refs)
	case *parse.IfNode:
		templateFieldRefs(t, tmpl, n.Pipe, refs)
		templateFieldRefs(t, tmpl, n.List, refs)
		templateFieldRefs(t, tmpl, n.ElseList, refs)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, cmd := range n.Cmds {
			for _, arg := range cmd.Args {
				templateFieldRefs(t, tmpl, arg, refs)
			}
		}
	case *parse.FieldNode:
		refs[n.Ident[0]] = tmpl
	case *parse.VariableNode:
		if n.Ident[0] == "$" && len(n.Ident) > 1 {
			refs[n.Ident[1]] = tmpl
		}
	case *parse.TextNode, *parse.CommentNode, *parse.StringNode, *parse.NumberNode,
		*parse.BoolNode, *parse.NilNode, *parse.DotNode, *parse.IdentifierNode:
	default:
		t.Fatalf("%s: template node %T (%s) is not handled by this check; extend templateFieldRefs", tmpl, node, node)
	}
}

// iacTemplateFieldRefs parses every template in internal/iacfiles/templates
// and returns the data fields they reference, each mapped to one template
// that uses it.
func iacTemplateFieldRefs(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repoRoot, "internal", "iacfiles", "templates", "*.tmpl"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no templates found")
	refs := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		// Parsed the way the script and the server parse them, so the
		// builtin functions such as eq are defined.
		tmpl, err := template.New(filepath.Base(path)).Parse(string(raw))
		require.NoError(t, err, "parse %s", path)
		for _, tt := range tmpl.Templates() {
			templateFieldRefs(t, filepath.Base(path), tt.Root, refs)
		}
	}
	return refs
}

// TestIaCData_MatchesServerTemplateData fails when iacData and the server's
// federationIaCData stop having the same fields. Templates are shared, so a
// field the server fills is one a template can start using at any time.
func TestIaCData_MatchesServerTemplateData(t *testing.T) {
	script := structFieldNames(t, scriptFile, "iacData")
	server := structFieldNames(t, filepath.Join(repoRoot, "internal", "api", "handler_federation.go"), "federationIaCData")
	require.Equal(t, server, script,
		"iacData in scripts/%s and federationIaCData in internal/api/handler_federation.go render the same "+
			"templates and must have the same fields (#153)", scriptFile)
}

// TestIaCData_CoversEveryTemplateField fails as soon as any template in
// internal/iacfiles/templates references a field iacData lacks. text/template
// treats a missing struct field as an execution error, so such a template
// cannot be rendered by the script at all.
func TestIaCData_CoversEveryTemplateField(t *testing.T) {
	have := map[string]bool{}
	for _, name := range structFieldNames(t, scriptFile, "iacData") {
		have[name] = true
	}
	var missing []string
	for field, tmpl := range iacTemplateFieldRefs(t) {
		if !have[field] {
			missing = append(missing, field+" (used by "+tmpl+")")
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing, "iacData lacks fields that the templates reference: %s", strings.Join(missing, ", "))
}

// TestIaCData_FieldWalkerSeesKnownFields is the positive control for the two
// tests above: a parser that found no fields would make both pass vacuously.
func TestIaCData_FieldWalkerSeesKnownFields(t *testing.T) {
	refs := iacTemplateFieldRefs(t)
	for _, field := range []string{"AccountName", "CUDlyAPIURL", "OIDCIssuerHost", "SourceAccountID"} {
		require.Contains(t, refs, field)
	}
	require.Contains(t, structFieldNames(t, scriptFile, "iacData"), "AccountName")
}

// TestGenerator_AWSWIFBundleRendersDeployScript runs the real
// aws-cfn-deploy.sh.tmpl through --format bundle for each non-AWS source. It
// only reads the rendered script; running it would call the AWS CLI.
func TestGenerator_AWSWIFBundleRendersDeployScript(t *testing.T) {
	const tenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	cases := []struct {
		source, wantHost string
		extra            []string
	}{
		{"gcp", "accounts.google.com", nil},
		{"azure", "login.microsoftonline.com/" + tenant + "/v2.0", []string{"--tenant-id", tenant}},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			bundle := filepath.Join(t.TempDir(), "bundle.zip")
			args := append([]string{
				"--target", "aws", "--source", tc.source, "--format", "bundle", "--output", bundle,
				"--account-name", "prod", "--account-id", validAccountIDFor("aws"),
				"--oidc-subject-claim", "123456789012345678901",
			}, tc.extra...)
			res := runGenerator(t, args...)
			require.Zero(t, res.exitCode, "stderr: %s", res.stderr)
			script := readZipEntry(t, bundle, "cloudformation/deploy-cfn.sh")
			require.Contains(t, script, `"OIDCIssuerHost=`+tc.wantHost+`"`)
		})
	}
}

// readZipEntry returns the contents of the named entry in the zip at path.
func readZipEntry(t *testing.T, path, name string) string {
	t.Helper()
	archive, err := zip.OpenReader(path)
	require.NoError(t, err)
	defer archive.Close()
	entry, err := archive.Open(name)
	require.NoError(t, err)
	defer entry.Close()
	b, err := io.ReadAll(entry)
	require.NoError(t, err)
	return string(b)
}
