package main

// generate-federation-iac_test.go drives scripts/generate-federation-iac.go
// end to end via `go run`, the same way an operator would invoke it. The
// script carries a `//go:build ignore` tag, so it is never compiled as part
// of this package (or any other) — the only way to exercise its actual
// --format=tfvars rendering path, and therefore the only way to catch drift
// between iacData and the tfvars templates it renders, is to run it as a
// subprocess. See #1709: iacData was missing three fields (ContactEmail,
// CUDlyAPIURL, SourceAccountID) referenced by every tfvars template, and
// nothing exercised this path so the break went unnoticed.

import (
	"archive/zip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// runViaGoRun invokes `go run generate-federation-iac.go <args>` from the
// repository root (the test binary's working directory is this package's
// directory, scripts/, so ".." is the repo root — matching how the script's
// own doc comment says to invoke it). Named distinctly from the
// generate_federation_iac_test.go file's own runGenerator (same package,
// different signature: that one runs a pre-built binary) to avoid a symbol
// collision between the two test files.
func runViaGoRun(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "scripts/generate-federation-iac.go"}, args...)...)
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestGenerateFederationIaC_TfvarsCombinations runs every --target/--source
// combination routed by singleFileTmpl through the real --format=tfvars
// path (the default format) and asserts a successful render plus the
// presence of the fields that data. Failing to add a field the templates
// reference is exactly the bug in #1709: text/template treats a missing
// struct field as a hard execution error, not an empty string.
func TestGenerateFederationIaC_TfvarsCombinations(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantContain []string
	}{
		{
			name: "aws target, azure source",
			args: []string{
				"--target", "aws", "--source", "azure",
				"--account-name", "Acme", "--account-id", "123456789012",
				"--tenant-id", "11111111-2222-3333-4444-555555555555",
				"--oidc-subject-claim", "11111111-2222-3333-4444-555555555555",
				"--contact-email", "ops@example.com", "--cudly-api-url", "https://cudly.example.com",
				"--output", "-",
			},
			wantContain: []string{
				`cudly_api_url = "https://cudly.example.com"`,
				`contact_email = "ops@example.com"`,
			},
		},
		{
			name: "aws target, gcp source",
			args: []string{
				"--target", "aws", "--source", "gcp",
				"--account-name", "Acme", "--account-id", "123456789012",
				"--oidc-subject-claim", "123456789012345678901",
				"--contact-email", "ops@example.com", "--cudly-api-url", "https://cudly.example.com",
				"--output", "-",
			},
			wantContain: []string{
				`cudly_api_url = "https://cudly.example.com"`,
				`contact_email = "ops@example.com"`,
			},
		},
		{
			name: "aws target, aws source (cross-account)",
			args: []string{
				"--target", "aws", "--source", "aws",
				"--account-name", "Acme", "--account-id", "999888777666",
				"--source-account-id", "111122223333",
				"--contact-email", "ops@example.com", "--cudly-api-url", "https://cudly.example.com",
				"--output", "-",
			},
			wantContain: []string{
				`source_account_id = "111122223333"`,
				`cudly_api_url = "https://cudly.example.com"`,
				`contact_email = "ops@example.com"`,
			},
		},
		{
			name: "azure target, aws source",
			args: []string{
				"--target", "azure", "--source", "aws",
				"--account-name", "Acme", "--account-id", "11111111-2222-3333-4444-555555555555",
				"--tenant-id", "11111111-2222-3333-4444-555555555555",
				"--contact-email", "ops@example.com", "--cudly-api-url", "https://cudly.example.com",
				"--output", "-",
			},
			wantContain: []string{
				`cudly_api_url = "https://cudly.example.com"`,
				`contact_email = "ops@example.com"`,
			},
		},
		{
			name: "azure target, gcp source",
			args: []string{
				"--target", "azure", "--source", "gcp",
				"--account-name", "Acme", "--account-id", "11111111-2222-3333-4444-555555555555",
				"--tenant-id", "11111111-2222-3333-4444-555555555555",
				"--contact-email", "ops@example.com", "--cudly-api-url", "https://cudly.example.com",
				"--output", "-",
			},
			wantContain: []string{
				`cudly_api_url = "https://cudly.example.com"`,
				`contact_email = "ops@example.com"`,
			},
		},
		{
			name: "gcp target, gcp source (sa impersonation)",
			args: []string{
				"--target", "gcp", "--source", "gcp",
				"--account-name", "Acme", "--account-id", "my-project",
				"--contact-email", "ops@example.com", "--cudly-api-url", "https://cudly.example.com",
				"--output", "-",
			},
			wantContain: []string{
				`cudly_api_url = "https://cudly.example.com"`,
				`contact_email = "ops@example.com"`,
			},
		},
		{
			name: "gcp target, aws source (WIF pool)",
			args: []string{
				"--target", "gcp", "--source", "aws",
				"--account-name", "Acme", "--account-id", "my-project",
				"--source-account-id", "111122223333",
				"--contact-email", "ops@example.com", "--cudly-api-url", "https://cudly.example.com",
				"--output", "-",
			},
			wantContain: []string{
				`aws_account_id = "111122223333"`,
				`cudly_api_url = "https://cudly.example.com"`,
				`contact_email = "ops@example.com"`,
			},
		},
		{
			name: "gcp target, azure source (WIF pool)",
			args: []string{
				"--target", "gcp", "--source", "azure",
				"--account-name", "Acme", "--account-id", "my-project",
				"--tenant-id", "11111111-2222-3333-4444-555555555555",
				"--contact-email", "ops@example.com", "--cudly-api-url", "https://cudly.example.com",
				"--output", "-",
			},
			wantContain: []string{
				`cudly_api_url = "https://cudly.example.com"`,
				`contact_email = "ops@example.com"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runViaGoRun(t, tt.args...)
			if err != nil {
				t.Fatalf("generator failed: %v\noutput:\n%s", err, out)
			}
			for _, want := range tt.wantContain {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\noutput:\n%s", want, out)
				}
			}
		})
	}
}

// TestGenerateFederationIaC_RequiresSourceAccountID guards the fail-loud
// behavior added alongside the #1709 fix: --source-account-id has no
// sensible default in the standalone script (unlike the server, which
// resolves it via STS), so target/source combinations that need it must
// error explicitly instead of silently rendering an empty or wrong account
// ID into the trust policy.
func TestGenerateFederationIaC_RequiresSourceAccountID(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "aws target, aws source, no source-account-id",
			args: []string{
				"--target", "aws", "--source", "aws",
				"--account-name", "Acme", "--account-id", "999888777666",
				"--output", "-",
			},
		},
		{
			name: "gcp target, aws source, no source-account-id",
			args: []string{
				"--target", "gcp", "--source", "aws",
				"--account-name", "Acme", "--account-id", "my-project",
				"--output", "-",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runViaGoRun(t, tt.args...)
			if err == nil {
				t.Fatalf("expected failure without --source-account-id, got success:\n%s", out)
			}
			if !strings.Contains(out, "--source-account-id is required") {
				t.Errorf("expected error naming --source-account-id, got:\n%s", out)
			}
		})
	}
}

// TestGenerateFederationIaC_RejectsMalformedSourceAccountID guards the CR
// finding on #1710: a non-empty --source-account-id is not necessarily a
// valid one. Before this check, a malformed value flowed straight into
// data.SourceAccountID and out into the rendered tfvars, where the problem
// would only surface later as a confusing Terraform or AWS error far from
// its actual cause. Covers both consumers of SourceAccountID: --target aws
// --source aws (aws-cross-account) and --target gcp --source aws (WIF pool).
func TestGenerateFederationIaC_RejectsMalformedSourceAccountID(t *testing.T) {
	tests := []struct {
		name            string
		target          string
		sourceAccountID string
	}{
		{name: "aws target, too short", target: "aws", sourceAccountID: "12345"},
		{name: "aws target, non-digit characters", target: "aws", sourceAccountID: "1111222233aa"},
		{name: "aws target, leading plus sign", target: "aws", sourceAccountID: "+11122223333"},
		{name: "aws target, whitespace padded", target: "aws", sourceAccountID: " 111122223333"},
		{name: "gcp target, too short", target: "gcp", sourceAccountID: "12345"},
		{name: "gcp target, non-digit characters", target: "gcp", sourceAccountID: "1111222233aa"},
		{name: "gcp target, leading plus sign", target: "gcp", sourceAccountID: "+11122223333"},
		{name: "gcp target, whitespace padded", target: "gcp", sourceAccountID: " 111122223333"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accountID := "999888777666"
			if tt.target == "gcp" {
				accountID = "my-project"
			}
			out, err := runViaGoRun(t,
				"--target", tt.target, "--source", "aws",
				"--account-name", "Acme", "--account-id", accountID,
				"--source-account-id", tt.sourceAccountID,
				"--output", "-",
			)
			if err == nil {
				t.Fatalf("expected failure for malformed --source-account-id %q, got success:\n%s", tt.sourceAccountID, out)
			}
			if !strings.Contains(out, "is not a valid AWS account ID") {
				t.Errorf("expected error naming the invalid --source-account-id, got:\n%s", out)
			}
			if !strings.Contains(out, tt.sourceAccountID) {
				t.Errorf("expected error to name the rejected value %q, got:\n%s", tt.sourceAccountID, out)
			}
		})
	}
}

func TestGenerator_AzureRejectsInvalidAPIURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		omit bool
	}{
		{name: "omitted", omit: true},
		{name: "empty"},
		{name: "http", url: "http://cudly.example.com"},
		{name: "missing host", url: "https://"},
		{name: "space", url: "https://cudly.example.com/a b"},
		{name: "newline", url: "https://cudly.example.com/a\nb"},
		{name: "quote", url: `https://cudly.example.com/a"b`},
		{name: "backslash", url: `https://cudly.example.com/a\b`},
		{name: "userinfo", url: "https://user@cudly.example.com"},
		{name: "query", url: "https://cudly.example.com/?q=1"},
		{name: "fragment", url: "https://cudly.example.com/#f"},
		{name: "trailing slash", url: "https://cudly.example.com/"},
		{name: "nonnumeric port", url: "https://cudly.example.com:https"},
	}
	for _, source := range []string{"aws", "gcp", "azure"} {
		for _, format := range []string{"tfvars", "bundle"} {
			for _, tc := range cases {
				t.Run(source+"/"+format+"/"+tc.name, func(t *testing.T) {
					output := filepath.Join(t.TempDir(), "output")
					args := []string{
						"--target", "azure", "--source", source,
						"--account-name", "Acme", "--account-id", "11111111-2222-3333-4444-555555555555",
						"--tenant-id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "--output", output,
					}
					if format == "bundle" {
						args = append(args, "--format", "bundle")
					}
					if !tc.omit {
						args = append(args, "--cudly-api-url", tc.url)
					}
					res := runGenerator(t, args...)
					require.NotZero(t, res.exitCode, "stdout: %s; stderr: %s", res.stdout, res.stderr)
					require.Contains(t, res.stderr, "--cudly-api-url")
					require.Empty(t, res.stdout)
					_, err := os.Stat(output)
					require.ErrorIs(t, err, os.ErrNotExist)
				})
			}
		}
	}
}

func TestGenerator_AzureMissingURLPreservesOutput(t *testing.T) {
	for _, format := range []string{"tfvars", "bundle"} {
		t.Run(format, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "existing-output")
			const sentinel = "existing operator data\n"
			require.NoError(t, os.WriteFile(output, []byte(sentinel), 0o600))
			args := []string{"--target", "azure", "--source", "aws", "--account-name", "Acme", "--account-id", "11111111-2222-3333-4444-555555555555"}
			if format == "bundle" {
				args = append(args, "--format", "bundle")
			}
			res := runGenerator(t, append(args, "--output", output)...)
			require.NotZero(t, res.exitCode, "stderr: %s", res.stderr)
			require.Contains(t, res.stderr, "--cudly-api-url")
			got, err := os.ReadFile(output)
			require.NoError(t, err)
			require.Equal(t, sentinel, string(got))
			if format == "tfvars" {
				res = runGenerator(t, append(args, "--output", "-")...)
				require.NotZero(t, res.exitCode)
				require.Empty(t, res.stdout)
				require.Contains(t, res.stderr, "--cudly-api-url")
			}
		})
	}
}

func TestGenerator_AzurePreservesValidAPIURL(t *testing.T) {
	for _, baseURL := range []string{"https://cudly.example.com", "https://CUDly.example.com:8443/a_b/~v1%20"} {
		for _, source := range []string{"aws", "gcp", "azure"} {
			for _, format := range []string{"tfvars", "bundle"} {
				t.Run(baseURL+"/"+source+"/"+format, func(t *testing.T) {
					args := []string{
						"--target", "azure", "--source", source,
						"--account-name", "Acme", "--account-id", "11111111-2222-3333-4444-555555555555",
						"--tenant-id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "--cudly-api-url", baseURL,
					}
					output := "-"
					if format == "bundle" {
						output = filepath.Join(t.TempDir(), "bundle.zip")
						args = append(args, "--format", "bundle")
					}
					res := runGenerator(t, append(args, "--output", output)...)
					require.Zero(t, res.exitCode, "stderr: %s", res.stderr)
					content := res.stdout
					if format == "bundle" {
						archive, err := zip.OpenReader(output)
						require.NoError(t, err)
						defer archive.Close()
						entry, err := archive.Open("terraform/acme-azure-wif.tfvars")
						require.NoError(t, err)
						data, err := io.ReadAll(entry)
						require.NoError(t, entry.Close())
						require.NoError(t, err)
						content = string(data)
					}
					require.Contains(t, content, `cudly_issuer_url = "`+baseURL+`/oidc"`)
					require.Contains(t, content, `cudly_api_url = "`+baseURL+`"`)
					require.Contains(t, content, `contact_email = ""`)
				})
			}
		}
	}
}

func TestGenerator_NonAzureDoesNotRequireAPIURL(t *testing.T) {
	for _, target := range []string{"aws", "gcp"} {
		t.Run(target, func(t *testing.T) {
			res := runGenerator(t,
				"--target", target, "--source", "aws", "--account-name", "Acme", "--account-id", validAccountIDFor(target),
				"--source-account-id", "111122223333", "--output", "-",
			)
			require.Zero(t, res.exitCode, "stderr: %s", res.stderr)
			require.Contains(t, res.stdout, `cudly_api_url = ""`)
		})
	}
}

// accountIDArgs is a complete, otherwise-valid invocation for target with
// --source aws, so the only thing that can make it fail is --account-id.
func accountIDArgs(target, accountID, output string) []string {
	args := []string{
		"--target", target, "--source", "aws",
		"--account-name", "Acme", "--account-id", accountID,
		"--output", output,
	}
	switch target {
	case "aws", "gcp":
		args = append(args, "--source-account-id", "111122223333")
	case "azure":
		args = append(args,
			"--tenant-id", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"--cudly-api-url", "https://cudly.example.com")
	}
	return args
}

// TestGenerator_RejectsMalformedAccountID is the regression test for #164:
// --account-id was checked for presence but never for format, so a malformed
// value flowed into AccountExternalID (and from there SubscriptionID or
// ProjectID) and out into the rendered tfvars, surfacing later as a confusing
// Terraform or provider error far from its cause. The flag means a different
// thing per --target, so each target arm gets its own malformed shapes,
// including values a normalizer would have "fixed" (padding, case, braces):
// the generator must reject those rather than rewrite them into validity.
func TestGenerator_RejectsMalformedAccountID(t *testing.T) {
	cases := []struct {
		name      string
		target    string
		accountID string
		wantShape string
	}{
		{name: "aws too short", target: "aws", accountID: "12345", wantShape: "AWS account ID"},
		{name: "aws too long", target: "aws", accountID: "9998887776661", wantShape: "AWS account ID"},
		{name: "aws non-digit characters", target: "aws", accountID: "99988877766a", wantShape: "AWS account ID"},
		{name: "aws leading plus sign", target: "aws", accountID: "+99888777666", wantShape: "AWS account ID"},
		{name: "aws whitespace padded", target: "aws", accountID: " 999888777666", wantShape: "AWS account ID"},
		{name: "azure not a GUID", target: "azure", accountID: "sub-1234", wantShape: "Azure subscription ID"},
		{name: "azure GUID without hyphens", target: "azure", accountID: "11111111222233334444555555555555", wantShape: "Azure subscription ID"},
		{name: "azure GUID in braces", target: "azure", accountID: "{11111111-2222-3333-4444-555555555555}", wantShape: "Azure subscription ID"},
		{name: "azure whitespace padded", target: "azure", accountID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee ", wantShape: "Azure subscription ID"},
		{name: "azure non-hex digit", target: "azure", accountID: "1111111g-2222-3333-4444-555555555555", wantShape: "Azure subscription ID"},
		{name: "gcp uppercase", target: "gcp", accountID: "My-Project", wantShape: "GCP project ID"},
		{name: "gcp digit first", target: "gcp", accountID: "1my-project", wantShape: "GCP project ID"},
		{name: "gcp trailing hyphen", target: "gcp", accountID: "my-project-", wantShape: "GCP project ID"},
		{name: "gcp too short", target: "gcp", accountID: "abcde", wantShape: "GCP project ID"},
		{name: "gcp too long", target: "gcp", accountID: "a" + strings.Repeat("b", 30), wantShape: "GCP project ID"},
		{name: "gcp underscore", target: "gcp", accountID: "my_project", wantShape: "GCP project ID"},
		{name: "gcp whitespace padded", target: "gcp", accountID: " my-project", wantShape: "GCP project ID"},
		{name: "gcp HCL quote injection", target: "gcp", accountID: "my-project\"\nevil = \"x", wantShape: "GCP project ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			res := runGenerator(t, accountIDArgs(tc.target, tc.accountID, output)...)
			require.NotZero(t, res.exitCode, "stdout: %s; stderr: %s", res.stdout, res.stderr)
			require.Contains(t, res.stderr, "--account-id")
			require.Contains(t, res.stderr, strconv.Quote(tc.accountID), "error must name the rejected value")
			require.Contains(t, res.stderr, "is not a valid "+tc.wantShape)
			require.Contains(t, res.stderr, "--target "+tc.target)
			require.Empty(t, res.stdout)
			_, err := os.Stat(output)
			require.ErrorIs(t, err, os.ErrNotExist, "no artifact may be written for a rejected --account-id")
		})
	}
}

// TestGenerator_AcceptsValidAccountID is the positive control for
// TestGenerator_RejectsMalformedAccountID: well-formed values for each target,
// including the boundaries of each rule, still render and appear verbatim in
// the output.
func TestGenerator_AcceptsValidAccountID(t *testing.T) {
	cases := []struct {
		name      string
		target    string
		accountID string
	}{
		// The AWS rows are multi-line so each 12-digit value sits alone on an
		// accountID line, the shape .gitallowed exempts for synthetic fixtures.
		{
			name:      "aws 12 digits",
			target:    "aws",
			accountID: "123456789012",
		},
		{
			name:      "aws leading zeros",
			target:    "aws",
			accountID: "000011112222",
		},
		{name: "azure lowercase GUID", target: "azure", accountID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
		{name: "azure uppercase GUID", target: "azure", accountID: "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"},
		{name: "gcp typical", target: "gcp", accountID: "my-project"},
		{name: "gcp minimum length", target: "gcp", accountID: "abcdef"},
		{name: "gcp maximum length", target: "gcp", accountID: "a" + strings.Repeat("b", 28) + "9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runGenerator(t, accountIDArgs(tc.target, tc.accountID, "-")...)
			require.Zero(t, res.exitCode, "stderr: %s", res.stderr)
			require.Contains(t, res.stdout, tc.accountID)
		})
	}
}
