package main

// Regression tests for #952: flag values used to reach the tfvars, JSON and
// Bash templates unescaped, so a quote, "${" or a newline in a value such as
// --account-name could change the structure of the generated file. These live
// in their own file because the two existing test files for the script are at
// the repository's 500-line limit.

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Hostile values for the flags that have no field-specific validation. Each
// one carries the metacharacters of all three output grammars: quotes and
// backslashes (HCL, JSON, Bash), "${" and "%{" (HCL interpolation and
// directives) and "$(...)" plus backticks (Bash command substitution).
const (
	hostileName  = `prod" ${x} %{y} \z $(printf INJECTED) ` + "`printf INJECTED`"
	hostileEmail = `"o\"q${x}%{y}"@example.com`
	hostileURL   = `https://cudly.example.com/a"b${x}`
	hostileSlug  = `slug"x`
	hostileSA    = `sa"x${y}@example.com`
	// hostileShellEmail adds Bash command substitutions to hostileEmail. It
	// keeps the '}' (once left out because it closed the template's
	// "${CUDLY_CONTACT_EMAIL:-default}" early): the deploy scripts now assign
	// the pre-filled email on its own line (#1436), and the probe below does
	// the same.
	hostileShellEmail = `"o\"q${x}%{y} $(printf INJECTED) ` + "`printf INJECTED`" + `"@example.com`
)

// hclAttrRE matches one `name = "value"` tfvars line, optionally followed by a
// trailing # comment. The value is a complete HCL quoted string: any run of
// non-quote, non-backslash characters or backslash escapes.
var hclAttrRE = regexp.MustCompile(`^([a-z_]+)\s*=\s*"((?:[^"\\]|\\.)*)"\s*(#.*)?$`)

// hclUnquote decodes the body of an HCL quoted string, failing on anything
// HCL would read as syntax rather than text: an unescaped "${" or "%{".
func hclUnquote(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		rest := s[i:]
		switch {
		case c == '\\':
			require.Less(t, i+1, len(s), "dangling backslash in %q", s)
			i++
			switch s[i] {
			case '\\', '"':
				b.WriteByte(s[i])
			default:
				t.Fatalf("unexpected escape \\%c in %q", s[i], s)
			}
		case strings.HasPrefix(rest, "$${"), strings.HasPrefix(rest, "%%{"):
			b.WriteString(rest[1:3])
			i += 2
		case strings.HasPrefix(rest, "${"), strings.HasPrefix(rest, "%{"):
			t.Fatalf("unescaped HCL template sequence at %q in %q", rest, s)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// parseTfvars returns the attributes of a generated tfvars file, failing if
// any line is neither blank, a # comment, nor a single quoted attribute.
func parseTfvars(t *testing.T, content string) map[string]string {
	t.Helper()
	attrs := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := hclAttrRE.FindStringSubmatch(trimmed)
		require.NotNil(t, m, "line is not a single quoted HCL attribute: %q\nfile:\n%s", line, content)
		_, dup := attrs[m[1]]
		require.False(t, dup, "attribute %s set twice\nfile:\n%s", m[1], content)
		attrs[m[1]] = hclUnquote(t, m[2])
	}
	return attrs
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// tfvarsCase is one single-file tfvars invocation. base holds the
// target-specific flags; hostile and benign hold the values for the flags
// under test, and want maps a tfvars attribute to the hostile value it must
// decode back to.
type tfvarsCase struct {
	name    string
	base    []string
	hostile []string
	benign  []string
	want    map[string]string
}

func tfvarsCases() []tfvarsCase {
	common := []string{"--account-name", hostileName, "--contact-email", hostileEmail}
	commonBenign := []string{"--account-name", "Acme", "--contact-email", "ops@example.com"}
	commonWant := map[string]string{"account_name": hostileName, "contact_email": hostileEmail}
	withWant := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range commonWant {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	return []tfvarsCase{
		{
			name: "aws-wif",
			base: []string{"--target", "aws", "--source", "azure", "--account-id", validAccountIDFor("aws"),
				"--oidc-subject-claim", "11111111-2222-3333-4444-555555555555"},
			hostile: append([]string{"--tenant-id", `t"x${y}`, "--cudly-api-url", hostileURL, "--account-slug", hostileSlug}, common...),
			benign:  append([]string{"--tenant-id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "--cudly-api-url", "https://cudly.example.com", "--account-slug", "acme"}, commonBenign...),
			want: withWant(map[string]string{
				"oidc_issuer_url": "https://login.microsoftonline.com/" + `t"x${y}` + "/v2.0",
				"cudly_api_url":   hostileURL,
				"role_name":       "CUDly-" + hostileSlug,
			}),
		},
		{
			name: "aws-cross-account",
			base: []string{"--target", "aws", "--source", "aws", "--account-id", validAccountIDFor("aws"),
				"--source-account-id", "111122223333"},
			hostile: append([]string{"--cudly-api-url", hostileURL}, common...),
			benign:  append([]string{"--cudly-api-url", "https://cudly.example.com"}, commonBenign...),
			want:    withWant(map[string]string{"cudly_api_url": hostileURL}),
		},
		{
			name: "azure-wif",
			base: []string{"--target", "azure", "--source", "aws", "--account-id", validAccountIDFor("azure"),
				"--cudly-api-url", "https://cudly.example.com"},
			hostile: append([]string{"--tenant-id", `t"x${y}`}, common...),
			benign:  append([]string{"--tenant-id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}, commonBenign...),
			want:    withWant(map[string]string{"tenant_id": `t"x${y}`}),
		},
		{
			name:    "gcp-sa-impersonation",
			base:    []string{"--target", "gcp", "--source", "gcp", "--account-id", validAccountIDFor("gcp")},
			hostile: append([]string{"--project-id", `p"x%{y}`, "--service-account-email", hostileSA}, common...),
			benign:  append([]string{"--project-id", "my-project", "--service-account-email", "cudly@my-project.iam.gserviceaccount.com"}, commonBenign...),
			want:    withWant(map[string]string{"project_id": `p"x%{y}`, "service_account_email": hostileSA}),
		},
		{
			name: "gcp-wif",
			base: []string{"--target", "gcp", "--source", "azure", "--account-id", validAccountIDFor("gcp"),
				"--tenant-id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
			hostile: append([]string{"--project-id", `p"x%{y}`, "--service-account-email", hostileSA, "--cudly-api-url", hostileURL}, common...),
			benign:  append([]string{"--project-id", "my-project", "--service-account-email", "cudly@my-project.iam.gserviceaccount.com", "--cudly-api-url", "https://cudly.example.com"}, commonBenign...),
			want:    withWant(map[string]string{"project": `p"x%{y}`, "service_account_email": hostileSA, "cudly_api_url": hostileURL}),
		},
	}
}

// TestGenerator_TfvarsValuesCannotChangeStructure is the #952 regression test
// for the tfvars output: with hostile values in every free-form flag, the file
// must hold exactly the attributes a benign run produces, and each value must
// decode back to the flag value it came from, so the input was carried as data
// and never read as HCL.
func TestGenerator_TfvarsValuesCannotChangeStructure(t *testing.T) {
	for _, tc := range tfvarsCases() {
		t.Run(tc.name, func(t *testing.T) {
			benign := runGenerator(t, append(append(append([]string{}, tc.base...), tc.benign...), "--output", "-")...)
			require.Zero(t, benign.exitCode, "stderr: %s", benign.stderr)
			wantKeys := sortedKeys(parseTfvars(t, benign.stdout))

			res := runGenerator(t, append(append(append([]string{}, tc.base...), tc.hostile...), "--output", "-")...)
			require.Zero(t, res.exitCode, "stderr: %s", res.stderr)
			got := parseTfvars(t, res.stdout)
			require.Equal(t, wantKeys, sortedKeys(got), "hostile input changed the set of attributes")
			for attr, want := range tc.want {
				require.Equal(t, want, got[attr], "attribute %s", attr)
			}
		})
	}
}

// TestGenerator_CFParamsValuesCannotChangeStructure covers the JSON output:
// a --tenant-id that tries to close the string and open a new parameter must
// stay inside the OIDCIssuerURL value.
func TestGenerator_CFParamsValuesCannotChangeStructure(t *testing.T) {
	tenant := `t"},{"ParameterKey":"Injected","ParameterValue":"x${y}%{z}\`
	res := runGenerator(t,
		"--target", "aws", "--source", "azure", "--format", "cf-params",
		"--account-name", hostileName, "--account-slug", hostileSlug,
		"--account-id", validAccountIDFor("aws"), "--tenant-id", tenant,
		"--oidc-subject-claim", "11111111-2222-3333-4444-555555555555", "--output", "-")
	require.Zero(t, res.exitCode, "stderr: %s", res.stderr)

	var params []map[string]string
	require.NoError(t, json.Unmarshal([]byte(res.stdout), &params), "output must be valid JSON:\n%s", res.stdout)
	got := map[string]string{}
	for _, p := range params {
		require.Len(t, p, 2, "unexpected parameter shape %v", p)
		got[p["ParameterKey"]] = p["ParameterValue"]
	}
	require.Equal(t, map[string]string{
		"OIDCIssuerURL":    "https://login.microsoftonline.com/" + tenant + "/v2.0",
		"OIDCAudience":     "api://AzureADTokenExchange",
		"OIDCSubjectClaim": "11111111-2222-3333-4444-555555555555",
		"RoleName":         "CUDly-" + hostileSlug,
	}, got)
}

// probeDeployScript stands in for aws-cfn-deploy.sh.tmpl. Running the real
// script would call the AWS CLI, so this probe exercises the same .sh.tmpl
// escaping path with the quoting shapes the real Bash templates use, and
// prints what Bash made of each value instead of calling any cloud CLI.
// TestGenerator_AWSWIFBundleRendersDeployScript renders the real template.
const probeDeployScript = `#!/usr/bin/env bash
# Target: {{.AccountName}} ({{.AccountExternalID}})
set -euo pipefail
: "${STACK_NAME:=CUDly-{{.AccountSlug}}-WIF}"
NAME="{{.AccountName}}"
CONTACT_EMAIL="${CUDLY_CONTACT_EMAIL:-}"
[ -n "$CONTACT_EMAIL" ] || CONTACT_EMAIL="{{.ContactEmail}}"
URL="{{.CUDlyAPIURL}}/api/register"
printf '%s\n' "$STACK_NAME" "$NAME" "$CONTACT_EMAIL" "$URL"
`

// TestGenerator_ShellTemplatesEscapeValues covers the Bash output: the bundle's
// deploy script is rendered through the .sh.tmpl path and then run, and Bash
// must see each hostile value as the literal string that was passed.
func TestGenerator_ShellTemplatesEscapeValues(t *testing.T) {
	bash, lookErr := exec.LookPath("bash")
	if lookErr != nil {
		t.Skip("bash not available")
	}
	templates := t.TempDir()
	for _, name := range []string{"aws-wif.tfvars.tmpl", "aws-wif-cf-params.json.tmpl"} {
		b, err := os.ReadFile(filepath.Join("..", "internal", "iacfiles", "templates", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(templates, name), b, 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(templates, "aws-cfn-deploy.sh.tmpl"), []byte(probeDeployScript), 0o600))

	bundle := filepath.Join(t.TempDir(), "bundle.zip")
	res := runGenerator(t,
		"--target", "aws", "--source", "gcp", "--format", "bundle",
		"--templates-dir", templates, "--output", bundle,
		"--account-name", hostileName, "--account-slug", hostileSlug,
		"--account-id", validAccountIDFor("aws"),
		"--oidc-subject-claim", "123456789012345678901",
		"--contact-email", hostileShellEmail, "--cudly-api-url", hostileURL)
	require.Zero(t, res.exitCode, "stderr: %s", res.stderr)

	archive, err := zip.OpenReader(bundle)
	require.NoError(t, err)
	defer archive.Close()
	entry, err := archive.Open("cloudformation/deploy-cfn.sh")
	require.NoError(t, err)
	script, err := io.ReadAll(entry)
	require.NoError(t, entry.Close())
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "deploy-cfn.sh")
	require.NoError(t, os.WriteFile(path, script, 0o600))
	cmd := exec.Command(bash, path)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "rendered script failed:\n%s\nscript:\n%s", out, script)
	require.Equal(t, strings.Join([]string{
		"CUDly-" + hostileSlug + "-WIF",
		hostileName,
		hostileShellEmail,
		hostileURL + "/api/register",
	}, "\n")+"\n", string(out), "script:\n%s", script)
}

// TestGenerator_RejectsControlCharacters covers the one shape escaping cannot
// neutralize: values are also rendered into # comment lines, where a newline
// starts a new line of HCL or Bash. The issue's own example is the first case.
// Each flag here has no field-specific validator, so this check is the only
// thing between it and the template.
func TestGenerator_RejectsControlCharacters(t *testing.T) {
	gcpBase := []string{"--target", "gcp", "--source", "gcp", "--account-name", "Acme", "--account-id", validAccountIDFor("gcp")}
	cases := []struct {
		name string
		flag string
		val  string
	}{
		{name: "account-name newline (issue example)", flag: "account-name", val: "prod\"\nevil = \"x"},
		{name: "account-name carriage return", flag: "account-name", val: "prod\rx"},
		{name: "account-name tab", flag: "account-name", val: "prod\tx"},
		{name: "account-name escape sequence", flag: "account-name", val: "prod\x1b[2Jx"},
		{name: "account-slug newline", flag: "account-slug", val: "acme\nx"},
		{name: "contact-email newline", flag: "contact-email", val: "ops@example.com\nevil = \"x\""},
		{name: "cudly-api-url newline", flag: "cudly-api-url", val: "https://cudly.example.com\nx"},
		{name: "project-id newline", flag: "project-id", val: "my-project\nx"},
		{name: "service-account-email newline", flag: "service-account-email", val: "sa@example.com\nx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			args := append(append([]string{}, gcpBase...), "--output", output, "--"+tc.flag+"="+tc.val)
			res := runGenerator(t, args...)
			require.NotZero(t, res.exitCode, "stdout: %s; stderr: %s", res.stdout, res.stderr)
			require.Contains(t, res.stderr, "--"+tc.flag+" ")
			require.Contains(t, res.stderr, "contains a control character")
			require.Empty(t, res.stdout)
			_, err := os.Stat(output)
			require.ErrorIs(t, err, os.ErrNotExist, "no artifact may be written for a rejected value")
		})
	}

	t.Run("tenant-id newline", func(t *testing.T) {
		res := runGenerator(t,
			"--target", "azure", "--source", "aws", "--account-name", "Acme",
			"--account-id", validAccountIDFor("azure"), "--cudly-api-url", "https://cudly.example.com",
			"--tenant-id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\nx", "--output", "-")
		require.NotZero(t, res.exitCode, "stdout: %s", res.stdout)
		require.Contains(t, res.stderr, "--tenant-id ")
		require.Contains(t, res.stderr, "contains a control character")
		require.Empty(t, res.stdout)
	})
}

// TestGenerator_PlainValuesRenderVerbatim is the positive control: a value with
// no metacharacters must come out byte for byte, so escaping never alters
// ordinary input (including '+' and '-', which have no meaning in any of the
// three grammars).
func TestGenerator_PlainValuesRenderVerbatim(t *testing.T) {
	res := runGenerator(t,
		"--target", "gcp", "--source", "gcp", "--account-name", "Acme Prod-1",
		"--account-id", validAccountIDFor("gcp"), "--contact-email", "user+tag@example.com",
		"--cudly-api-url", "https://cudly.example.com:8443/base", "--output", "-")
	require.Zero(t, res.exitCode, "stderr: %s", res.stderr)
	require.Contains(t, res.stdout, `account_name  = "Acme Prod-1"`)
	require.Contains(t, res.stdout, `contact_email = "user+tag@example.com"`)
	require.Contains(t, res.stdout, `cudly_api_url = "https://cudly.example.com:8443/base"`)
	require.Contains(t, res.stdout, "# Target GCP project : Acme Prod-1 (my-project)")
}
