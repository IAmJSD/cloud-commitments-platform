package iac

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests cover #148: the provider-reuse path of setup-gcp-wif.sh validated
// who may federate (issuer or account, condition, mapping) but never which token
// audiences the provider accepts, so an existing OIDC provider whose
// allowedAudiences had been widened to another relying party's audience was
// reused silently.

// gcloudReuseProviderStub reports an existing provider that matches everything
// setup-gcp-wif.sh checks except, depending on STUB_AUDIENCES, its allowed
// audience list. Each describe field it is asked for is logged, so a test can
// tell which fields the script queried.
const gcloudReuseProviderStub = `#!/usr/bin/env bash
printf '%s\n' "gcloud $*" >> "${STUB_LOG}"
case "$*" in
  *"projects describe"*) echo "000000000000" ;;
  *"providers describe"*"attributeCondition"*) printf '%s\n' "${STUB_CONDITION}" ;;
  *"providers describe"*"oidc.issuerUri"*) echo "https://cudly.example.com/oidc" ;;
  *"providers describe"*"aws.accountId"*) echo "123456789012" ;;
  *"providers describe"*"attributeMapping"*) printf '%s\n' "${STUB_MAPPING}" ;;
  *"providers describe"*"oidc.allowedAudiences"*) printf '%s\n' "${STUB_AUDIENCES}" ;;
  *"get-iam-policy"*) ;;
esac
exit 0
`

// expectedProviderResource is the resource name of the provider the OIDC and
// AWS argument sets below configure: project number 000000000000 (from the
// stub) and the script's default pool and provider IDs.
const expectedProviderResource = "projects/000000000000/locations/global/workloadIdentityPools/cudly-pool/providers/cudly-provider"

// runReuseScript runs setup-gcp-wif.sh against gcloudReuseProviderStub with the
// given existing-provider fields.
func runReuseScript(t *testing.T, condition, mapping, audiences string, args []string) (exitCode int, stdout, stderr string, calls []string) {
	t.Helper()

	bashPath, lookErr := exec.LookPath("bash")
	if lookErr != nil {
		t.Fatalf("bash is required to execute %s: %v", setupScript, lookErr)
	}
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubDir, 0o700); err != nil {
		t.Fatalf("create stub dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "gcloud"), []byte(gcloudReuseProviderStub), 0o755); err != nil {
		t.Fatalf("write gcloud stub: %v", err)
	}
	logPath := filepath.Join(dir, "gcloud.log")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bashPath, append([]string{setupScript}, args...)...)
	cmd.Env = []string{
		"PATH=" + stubDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG=" + logPath,
		"STUB_CONDITION=" + condition,
		"STUB_MAPPING=" + mapping,
		"STUB_AUDIENCES=" + audiences,
	}
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	var exitErr *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run %s: %v (stderr: %s)", setupScript, err, errBuf.String())
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gcloud invocation log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(logBytes)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return cmd.ProcessState.ExitCode(), outBuf.String(), errBuf.String(), calls
}

// runOIDCReuse runs the OIDC reuse path with a condition and mapping that match
// what the script expects, varying only the allowed audience list.
func runOIDCReuse(t *testing.T, audiences string) (exitCode int, stdout, stderr string, calls []string) {
	t.Helper()
	return runReuseScript(t, "google.subject == 'cudly-controller'", "google.subject=assertion.sub", audiences, setupScriptOIDCArgs())
}

func grantCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.Contains(c, "add-iam-policy-binding") {
			out = append(out, c)
		}
	}
	return out
}

// TestSetupScriptReusesOIDCProviderWithOwnAudience covers the values a correctly
// scoped provider can carry: unset (GCP's default, what this script creates) or
// a single entry naming the provider itself, with or without the https: prefix
// GCP's default also accepts.
func TestSetupScriptReusesOIDCProviderWithOwnAudience(t *testing.T) {
	cases := map[string]string{
		"unset (GCP default)":     "",
		"own resource name":       "//iam.googleapis.com/" + expectedProviderResource,
		"own https resource name": "https://iam.googleapis.com/" + expectedProviderResource,
	}
	for name, audiences := range cases {
		t.Run(name, func(t *testing.T) {
			exitCode, stdout, stderr, calls := runOIDCReuse(t, audiences)
			if exitCode != 0 {
				t.Fatalf("exit code = %d, want 0\nstderr:\n%s", exitCode, stderr)
			}
			if !strings.Contains(stdout, "Reusing existing provider 'cudly-provider'") {
				t.Errorf("stdout must report the provider as reused, got:\n%s", stdout)
			}
			if len(grantCalls(calls)) != 1 {
				t.Errorf("want exactly one impersonation grant, calls:\n%s", strings.Join(calls, "\n"))
			}
		})
	}
}

// TestSetupScriptRejectsOIDCProviderWithForeignAudience is the regression test
// for #148: any audience list other than the provider's own must abort before
// the impersonation grant, with the found/expected values and the delete
// remediation.
func TestSetupScriptRejectsOIDCProviderWithForeignAudience(t *testing.T) {
	own := "//iam.googleapis.com/" + expectedProviderResource
	cases := map[string]string{
		"widened with another relying party": own + ";https://other-rp.example.com",
		"replaced by another relying party":  "https://other-rp.example.com",
		"another provider in the same pool":  "//iam.googleapis.com/projects/000000000000/locations/global/workloadIdentityPools/cudly-pool/providers/other-provider",
		"own name with a suffix":             own + "-evil",
		"own name with a prefix":             "https://evil.example.com/" + own,
		"own name listed twice":              own + ";" + own,
	}
	for name, audiences := range cases {
		t.Run(name, func(t *testing.T) {
			exitCode, _, stderr, calls := runOIDCReuse(t, audiences)
			if exitCode == 0 {
				t.Fatalf("exit code = 0, want nonzero for allowed audiences %q", audiences)
			}
			for _, want := range []string{
				"different allowed audience list",
				"found   : " + audiences,
				"expected: (none, GCP's default) or exactly " + own,
				"gcloud iam workload-identity-pools providers delete cudly-provider",
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr must contain %q, got:\n%s", want, stderr)
				}
			}
			if g := grantCalls(calls); len(g) != 0 {
				t.Errorf("no impersonation grant may be made for a provider with foreign audiences, got:\n%s", strings.Join(g, "\n"))
			}
		})
	}
}

// TestSetupScriptOIDCReuseStillChecksCondition pins that the audience check did
// not displace the existing ones: a matching audience list does not excuse a
// widened attribute condition.
func TestSetupScriptOIDCReuseStillChecksCondition(t *testing.T) {
	exitCode, _, stderr, calls := runReuseScript(t, "true", "google.subject=assertion.sub", "", setupScriptOIDCArgs())
	if exitCode == 0 || !strings.Contains(stderr, "different attribute condition") {
		t.Fatalf("a widened condition must still be rejected (exit %d), stderr:\n%s", exitCode, stderr)
	}
	if len(grantCalls(calls)) != 0 {
		t.Errorf("no impersonation grant may be made, calls:\n%s", strings.Join(calls, "\n"))
	}
}

// TestSetupScriptAWSReuseDoesNotQueryAudiences covers the AWS branch: an AWS
// provider has no allowed audience list, so the script must neither query nor
// gate on one there, and an otherwise matching provider is reused.
func TestSetupScriptAWSReuseDoesNotQueryAudiences(t *testing.T) {
	args := append(setupScriptCommonArgs(),
		"--provider-type", "aws",
		"--aws-account-id", "123456789012",
		"--aws-role-name", "CUDly-Execution",
	)
	condition := "attribute.aws_role == 'arn:aws:sts::123456789012:assumed-role/CUDly-Execution'"
	// value() renders the map sorted by key and ';'-joined.
	mapping := "attribute.aws_role=assertion.arn.contains('assumed-role') ? assertion.arn.extract('{account_arn}assumed-role/') + 'assumed-role/' + assertion.arn.extract('assumed-role/{role_name}/') : assertion.arn;google.subject=assertion.arn"
	exitCode, stdout, stderr, calls := runReuseScript(t, condition, mapping, "https://other-rp.example.com", args)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Reusing existing provider 'cudly-provider'") {
		t.Errorf("stdout must report the provider as reused, got:\n%s", stdout)
	}
	for _, c := range calls {
		if strings.Contains(c, "allowedAudiences") {
			t.Errorf("an AWS provider has no audience list to query, got call: %s", c)
		}
	}
}

// TestSetupScriptCreatesOIDCProviderWithDefaultAudience pins the create side of
// #148: create-oidc is called without --allowed-audiences, so GCP applies its
// default (the provider's own resource name), which is exactly what the reuse
// check accepts. Passing a list here would be the widening the check refuses.
func TestSetupScriptCreatesOIDCProviderWithDefaultAudience(t *testing.T) {
	exitCode, _, stderr, calls := runSetupScript(t, setupScriptOIDCArgs()...)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", exitCode, stderr)
	}
	var creates []string
	for _, c := range calls {
		if strings.Contains(c, "providers create-oidc") {
			creates = append(creates, c)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("want exactly one create-oidc call, got %d:\n%s", len(creates), strings.Join(calls, "\n"))
	}
	if strings.Contains(creates[0], "--allowed-audiences") {
		t.Errorf("create-oidc must leave allowed audiences at GCP's default, got: %s", creates[0])
	}
}
