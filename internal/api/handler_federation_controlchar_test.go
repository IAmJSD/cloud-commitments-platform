package api

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/iacfiles"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Regression tests for #970: a line break in a value the federation IaC
// templates print on a "#" comment line ends the comment, and the rest of the
// value runs as shell (or HCL in a tfvars file) when the admin runs or
// applies the download.

// controlCharPayloads are the values that must never reach a rendered
// template. Each starts with ordinary text so the control character is not the
// first byte, and the line-break ones carry a marker command that creates a
// file when it runs.
var controlCharPayloads = map[string]string{
	"LF":   "x\ntouch PWNED\n#",
	"CRLF": "x\r\ntouch PWNED\r\n#",
	"CR":   "x\rtouch PWNED",
	"VT":   "x\vy",
	"FF":   "x\fy",
	"NUL":  "x\x00y",
	"ESC":  "x\x1b[2Jy",
	"DEL":  "x\x7fy",
	"NEL":  "x\u0085y",
}

// hclInjection is an LF payload for the tfvars templates: if it escaped its
// comment line it would add a top-level attribute.
const hclInjection = "x\ninjected_by_test = \"1\"\n#"

var hclInjectedRe = regexp.MustCompile(`(?m)^injected_by_test\s*=`)

// federationTemplatePaths lists every embedded federation template.
func federationTemplatePaths(t *testing.T) []string {
	t.Helper()
	paths, err := fs.Glob(iacfiles.Templates, "templates/*.tmpl")
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	return paths
}

// escapeForTemplate applies the escaping the handler uses for each kind of
// template: shell for scripts, HCL for tfvars, none for the JSON params.
func escapeForTemplate(path string, data federationIaCData) federationIaCData {
	switch {
	case strings.HasSuffix(path, ".sh.tmpl"):
		return shellEscapeData(data)
	case strings.HasSuffix(path, ".tfvars.tmpl"):
		return hclEscapeData(data)
	}
	return data
}

// benignFederationData fills every string field with a realistic value, so
// each {{if}} branch that tests a field is rendered.
func benignFederationData() federationIaCData {
	return federationIaCData{
		AccountName:         "Acme Production",
		AccountExternalID:   "acme-prod",
		AccountSlug:         "acme-prod",
		Source:              "azure",
		SourceAccountID:     "000000000000",
		OIDCIssuerURL:       "https://login.microsoftonline.com/00000000-0000-0000-0000-000000000002/v2.0",
		OIDCIssuerHost:      "login.microsoftonline.com/00000000-0000-0000-0000-000000000002/v2.0",
		OIDCAudience:        "api://AzureADTokenExchange",
		OIDCSubjectClaim:    "00000000-0000-0000-0000-000000000003",
		SubscriptionID:      "00000000-0000-0000-0000-000000000001",
		TenantID:            "00000000-0000-0000-0000-000000000002",
		ProjectID:           "acme-project",
		ServiceAccountEmail: "cudly@acme-project.iam.gserviceaccount.com",
		OIDCIssuerURI:       "https://login.microsoftonline.com/00000000-0000-0000-0000-000000000002/v2.0",
		CUDlyAPIURL:         "https://cudly.example.com",
		ContactEmail:        "ops@example.com",
	}
}

// withField returns a copy of data with the named string field set to value.
func withField(t *testing.T, data federationIaCData, field, value string) federationIaCData {
	t.Helper()
	f := reflect.ValueOf(&data).Elem().FieldByName(field)
	require.True(t, f.IsValid() && f.Kind() == reflect.String, "no string field %s", field)
	f.SetString(value)
	return data
}

// federationStringFields lists every string field of federationIaCData.
func federationStringFields() []string {
	var names []string
	typ := reflect.TypeOf(federationIaCData{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type.Kind() == reflect.String {
			names = append(names, typ.Field(i).Name)
		}
	}
	return names
}

// sandboxRun runs script with bash in an empty working directory, with HOME
// pointing there and PATH holding only a symlink to touch, so the marker
// command can run but no cloud CLI, curl or other tool is reachable (the
// script stops at its first missing command). It reports whether bash -n
// accepted the script and the names of any files the run created.
func sandboxRun(t *testing.T, bashPath, script string) (syntaxOK bool, created []string) {
	t.Helper()
	touchPath, err := exec.LookPath("touch")
	require.NoError(t, err)
	root := t.TempDir()
	bin, work := filepath.Join(root, "bin"), filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(bin, 0o700))
	require.NoError(t, os.Mkdir(work, 0o700))
	require.NoError(t, os.Symlink(touchPath, filepath.Join(bin, "touch")))
	scriptPath := filepath.Join(root, "script.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o600))

	syntaxOK = exec.Command(bashPath, "-n", scriptPath).Run() == nil

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bashPath, "--noprofile", "--norc", scriptPath)
	cmd.Dir = work
	cmd.Env = []string{"PATH=" + bin, "HOME=" + work}
	_ = cmd.Run() // the script is expected to fail once it needs a real tool
	entries, err := os.ReadDir(work)
	require.NoError(t, err)
	for _, e := range entries {
		created = append(created, e.Name())
	}
	return syntaxOK, created
}

// TestRenderTemplate_RefusesControlCharsInEveryField renders every federation
// template with each control-character payload in each string field, with the
// handler's escaping applied, and requires renderTemplate to refuse it and
// name the field.
func TestRenderTemplate_RefusesControlCharsInEveryField(t *testing.T) {
	for _, path := range federationTemplatePaths(t) {
		for _, field := range federationStringFields() {
			for name, payload := range controlCharPayloads {
				data := escapeForTemplate(path, withField(t, benignFederationData(), field, payload))
				out, err := renderTemplate(path, data)
				if assert.Error(t, err, "%s: %s with %s must be refused", path, field, name) {
					assert.Contains(t, err.Error(), "field "+field)
				}
				assert.Empty(t, out)
			}
		}
	}
}

// TestRenderTemplate_UnguardedInjectionIsCaughtByOracle shows what the guard
// prevents, and that the sandbox oracle detects it: without the guard
// (executeTemplate), an LF in a comment-line field ends the comment and the
// marker runs, although bash -n still accepts the script. Every such
// combination must be refused by renderTemplate.
func TestRenderTemplate_UnguardedInjectionIsCaughtByOracle(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	injected := map[string]bool{}
	for _, path := range federationTemplatePaths(t) {
		if !strings.HasSuffix(path, ".sh.tmpl") {
			continue
		}
		for _, field := range federationStringFields() {
			data := shellEscapeData(withField(t, benignFederationData(), field, controlCharPayloads["LF"]))
			out, err := executeTemplate(path, data)
			require.NoError(t, err)
			syntaxOK, created := sandboxRun(t, bashPath, out)
			if len(created) == 0 {
				continue
			}
			injected[filepath.Base(path)+"/"+field] = true
			assert.True(t, syntaxOK, "%s/%s: bash -n alone does not catch the injection", path, field)
			_, err = renderTemplate(path, data)
			assert.Error(t, err, "%s/%s injects a command and must be refused", path, field)
		}
	}
	// The comment-line fields #970 lists must all be detected; if one is not,
	// the oracle (not the templates) is broken.
	for _, want := range []string{
		"aws-cfn-deploy.sh.tmpl/AccountName",
		"aws-cfn-deploy.sh.tmpl/AccountExternalID",
		"aws-cfn-deploy.sh.tmpl/Source",
		"aws-cross-account-cfn-deploy.sh.tmpl/AccountName",
		"aws-cross-account-cli.sh.tmpl/AccountExternalID",
		"aws-wif-cli.sh.tmpl/AccountSlug",
		"azure-wif-cli.sh.tmpl/AccountName",
		"azure-wif-cli.sh.tmpl/CUDlyAPIURL",
	} {
		assert.True(t, injected[want], "oracle must detect the injection via %s", want)
	}
}

// TestRenderTemplate_UnguardedTfvarsInjection is the HCL counterpart: without
// the guard an LF on a tfvars comment line adds a top-level attribute, and
// renderTemplate must refuse it.
func TestRenderTemplate_UnguardedTfvarsInjection(t *testing.T) {
	for _, path := range federationTemplatePaths(t) {
		if !strings.HasSuffix(path, ".tfvars.tmpl") {
			continue
		}
		data := hclEscapeData(withField(t, benignFederationData(), "AccountName", hclInjection))
		out, err := executeTemplate(path, data)
		require.NoError(t, err)
		assert.Regexp(t, hclInjectedRe, out, "%s: oracle must see the injected attribute", path)
		_, err = renderTemplate(path, data)
		assert.Error(t, err, "%s must refuse an AccountName with a line break", path)
	}
}

// TestRenderTemplate_BenignDataUnchanged checks the guard changes nothing for
// ordinary values, including a tab in the session email (allowed since #992):
// renderTemplate returns exactly what the unguarded render does, and every
// rendered script passes bash -n.
func TestRenderTemplate_BenignDataUnchanged(t *testing.T) {
	bashPath, bashErr := exec.LookPath("bash")
	for _, email := range []string{"ops@example.com", "\"a\tb\"@example.com"} {
		benign := benignFederationData()
		benign.ContactEmail = email
		for _, path := range federationTemplatePaths(t) {
			data := escapeForTemplate(path, benign)
			got, err := renderTemplate(path, data)
			require.NoError(t, err, path)
			want, err := executeTemplate(path, data)
			require.NoError(t, err, path)
			assert.Equal(t, want, got, "%s must render byte-identically", path)
			assert.NotRegexp(t, hclInjectedRe, got)
			if bashErr == nil && strings.HasSuffix(path, ".sh.tmpl") {
				syntax := exec.Command(bashPath, "-n")
				syntax.Stdin = strings.NewReader(got)
				assert.NoError(t, syntax.Run(), "%s must pass bash -n", path)
			}
		}
	}
}

// federationHandlerWithEmail is federationHandler with a chosen session email,
// standing in for a stored user record that predates or bypassed validation.
// Like federationHandler, the ValidateSession stub is a hard expectation.
func federationHandlerWithEmail(t *testing.T, email string) *Handler {
	t.Helper()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{
		UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Email:  email,
	}, nil)
	return newFederationHandler(mockAuth)
}

// TestGetFederationIaC_FailsClosedOnControlChar drives the real handler for
// every valid target/source/format combination: a benign request succeeds,
// and the same request with a line break in a value that reaches the
// templates (the session email) fails with a server error instead of
// returning a script or bundle.
func TestGetFederationIaC_FailsClosedOnControlChar(t *testing.T) {
	clouds := []string{"aws", "azure", "gcp"}
	formats := []string{"cli", "bundle", "cfn", "bicep", "arm"}
	checked := 0
	for _, source := range clouds {
		t.Setenv("CUDLY_SOURCE_CLOUD", source)
		for _, target := range clouds {
			for _, format := range formats {
				params := map[string]string{"target": target, "source": source, "format": format}
				_, err := federationHandlerWithEmail(t, "ops@example.com").getFederationIaC(
					context.Background(), federationReqWithDomain(params))
				if _, isClient := IsClientError(err); isClient {
					continue // combination not offered
				}
				require.NoError(t, err, "%v benign", params)
				checked++
				res, err := federationHandlerWithEmail(t, "ops@example.com\ntouch PWNED").getFederationIaC(
					context.Background(), federationReqWithDomain(params))
				assert.Nil(t, res, "%v must not return content", params)
				if assert.Error(t, err, "%v must fail closed", params) {
					_, isClient := IsClientError(err)
					assert.False(t, isClient, "%v: stored data is a server-side fault, not a 4xx", params)
					assert.Contains(t, err.Error(), "field ContactEmail")
				}
			}
		}
	}
	assert.GreaterOrEqual(t, checked, 10, "most combinations must be exercised")
}
