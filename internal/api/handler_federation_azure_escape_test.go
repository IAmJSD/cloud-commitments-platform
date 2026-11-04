package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/iacfiles"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// azureDeployScriptFromZip returns deploy-azure.sh from a Bicep/ARM zip.
func azureDeployScriptFromZip(t *testing.T, zipBytes []byte) string {
	t.Helper()
	return azureZipEntry(t, zipBytes, "deploy-azure.sh")
}

// azureZipEntry returns the named entry from a Bicep/ARM zip.
func azureZipEntry(t *testing.T, zipBytes []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	require.NoError(t, err)
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		require.NoError(t, err)
		var buf bytes.Buffer
		_, err = buf.ReadFrom(rc)
		require.NoError(t, rc.Close())
		require.NoError(t, err)
		return buf.String()
	}
	t.Fatalf("zip must contain %s", name)
	return ""
}

// evalShellWord evaluates a snippet of shell in a fresh bash, in an empty
// temporary directory and with an environment of PATH plus env, and returns
// what it printed plus the directory, so callers can check that no command
// substitution created a file there.
func evalShellWord(t *testing.T, bashPath, script string, env ...string) (out, dir string) {
	t.Helper()
	dir = t.TempDir()
	cmd := exec.Command(bashPath, "--noprofile", "--norc", "-c", script)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	got, err := cmd.Output()
	require.NoError(t, err, "bash must evaluate %q", script)
	return string(got), dir
}

var (
	// registrationBlockRe matches a deploy script from the CONTACT_EMAIL
	// assignment through the end of the PAYLOAD heredoc: the part that
	// turns the pre-filled email into the registration JSON, stopping
	// before the curl call.
	registrationBlockRe = regexp.MustCompile(`(?ms)^CONTACT_EMAIL=.*?^JSON$`)
	registerURLWordRe   = regexp.MustCompile(`(?m)-X POST ("[^\n]*/api/register") \\$`)
)

// registration is what evalRegistration observed.
type registration struct {
	contactEmail string
	payload      map[string]string
	dir          string
}

// evalRegistration runs the registration block of deploy-azure.sh in the
// evalShellWord sandbox, with az stubbed by a shell function (no Azure
// CLI, no network; curl is past the end of the block). It returns the
// resulting CONTACT_EMAIL and the PAYLOAD heredoc parsed as JSON.
func evalRegistration(t *testing.T, bashPath, script string, env ...string) registration {
	t.Helper()
	return evalRegistrationWith(t, bashPath,
		"az() { printf '%s\\n' 00000000-0000-0000-0000-0000000000aa; }\n", script, env...)
}

// evalRegistrationWith is evalRegistration with the caller's prelude (stub
// functions and the variables the block reads) in place of the az stub, so
// the other generated scripts' registration blocks can run the same way.
func evalRegistrationWith(t *testing.T, bashPath, prelude, script string, env ...string) registration {
	t.Helper()
	block := registrationBlockRe.FindString(script)
	require.NotEmpty(t, block, "the script must build the registration payload")
	out, dir := evalShellWord(t, bashPath,
		"set -euo pipefail\n"+prelude+block+"\nprintf '%s\\0%s' \"$CONTACT_EMAIL\" \"$PAYLOAD\"", env...)
	email, payload, ok := strings.Cut(out, "\x00")
	require.True(t, ok, "sandbox output must hold CONTACT_EMAIL and PAYLOAD: %q", out)
	var fields map[string]string
	require.NoError(t, json.Unmarshal([]byte(payload), &fields), "PAYLOAD must be valid JSON: %s", payload)
	return registration{contactEmail: email, payload: fields, dir: dir}
}

// hostileContactEmails all pass mail.ParseAddress. They cover command
// substitution (#958), a "}" that would close a ${VAR:-default} early, and
// the characters JSON needs escaped ("\", '"', tab).
var hostileContactEmails = []string{
	"\"a`touch pwned`b\"@example.com",
	`"a$(touch pwned)b"@example.com`,
	"a`id`b@example.com",
	`"a\"b\\c$HOME"@example.com`,
	`a}b@example.com`,
	`"a}b"@example.com`,
	"\"a}$(touch pwned)`touch pwned`}b\"@example.com",
	`"{a}${b}"@example.com`,
	"\"a\tb\"@example.com",
}

// TestGetFederationIaC_AzureDeployScriptEscapesSessionEmail is the
// regression test for #958. deploy-azure.sh in the Bicep and ARM bundles
// embeds the session user's email inside a double-quoted bash string. The
// email only has to pass mail.ParseAddress, which accepts backticks,
// "$(...)", "}", quotes, backslashes and tabs. Running the registration
// block must leave CONTACT_EMAIL equal to the email verbatim, put it into
// valid JSON unchanged, and run nothing.
func TestGetFederationIaC_AzureDeployScriptEscapesSessionEmail(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	t.Setenv("CUDLY_SOURCE_CLOUD", "gcp")

	for _, email := range hostileContactEmails {
		_, err := mail.ParseAddress(email)
		require.NoError(t, err, "fixture %q must be an address the auth service accepts", email)

		mockAuth := new(MockAuthService)
		t.Cleanup(func() { mockAuth.AssertExpectations(t) })
		mockAuth.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{
			UserID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
			Email:  email,
		}, nil)
		mockAuth.grantAdmin()
		h := NewHandler(HandlerConfig{ConfigStore: new(MockConfigStore), AuthService: mockAuth})
		mockSourceIdentity(h, defaultTestSourceIdentity())

		for _, format := range []string{"bicep", "arm"} {
			t.Run(format+"/"+email, func(t *testing.T) {
				res, err := h.getFederationIaC(context.Background(), federationReqWithDomain(map[string]string{
					"target": "azure", "source": "gcp", "format": format,
				}))
				require.NoError(t, err)
				zipBytes, err := base64.StdEncoding.DecodeString(res.Content)
				require.NoError(t, err)
				script := azureDeployScriptFromZip(t, zipBytes)

				syntax := exec.Command(bashPath, "-n")
				syntax.Stdin = bytes.NewReader([]byte(script))
				out, err := syntax.CombinedOutput()
				require.NoError(t, err, "deploy-azure.sh must parse: %s", out)

				got := evalRegistration(t, bashPath, script)
				assert.Equal(t, email, got.contactEmail, "CONTACT_EMAIL must evaluate to the email verbatim")
				assert.Equal(t, email, got.payload["contact_email"], "the payload must carry the email verbatim")
				assert.NoFileExists(t, filepath.Join(got.dir, "pwned"),
					"a command substitution in the email must not run")
			})
		}
	}
}

// TestBuildAzureTemplateZip_RegistrationEnvOverrides pins the env-override
// semantics of the restructured CONTACT_EMAIL assignment (a set, non-empty
// CUDLY_CONTACT_EMAIL wins; unset or empty falls back to the pre-filled
// email, as ${VAR:-default} did) and that an operator-supplied
// CUDLY_ACCOUNT_NAME is JSON-escaped too.
func TestBuildAzureTemplateZip_RegistrationEnvOverrides(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	const prefilled = `"pre}filled"@example.com`
	data := buildGenericIaCData("azure", "gcp", "https://cudly.example.com")
	data.ContactEmail = prefilled
	zipBytes, _, err := buildAzureTemplateZip("bicep", data, "azure", "target")
	require.NoError(t, err)
	script := azureDeployScriptFromZip(t, zipBytes)

	cases := []struct {
		name, wantEmail string
		env             []string
	}{
		{"unset", prefilled, nil},
		{"empty", prefilled, []string{"CUDLY_CONTACT_EMAIL="}},
		{"set", `"o}p\s"@example.com`, []string{`CUDLY_CONTACT_EMAIL="o}p\s"@example.com`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalRegistration(t, bashPath, script, tc.env...)
			assert.Equal(t, tc.wantEmail, got.contactEmail)
			assert.Equal(t, tc.wantEmail, got.payload["contact_email"])
			assert.Equal(t, "Azure 00000000-0000-0000-0000-0000000000aa", got.payload["account_name"])
		})
	}

	const name = "Ops \"prod\" \\ team\tB"
	got := evalRegistration(t, bashPath, script, "CUDLY_ACCOUNT_NAME="+name)
	assert.Equal(t, name, got.payload["account_name"], "account_name must be carried verbatim as JSON")
}

// TestBuildAzureTemplateZip_DeployScriptEscapesEveryField covers the other
// interpolation in deploy-azure.sh, the CUDly API URL in the registration
// curl call, with the builder called directly on hostile data.
func TestBuildAzureTemplateZip_DeployScriptEscapesEveryField(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	const apiURL = "https://cudly.example.com/$(touch pwned)`touch pwned`\"x"
	data := buildGenericIaCData("azure", "gcp", apiURL)
	data.ContactEmail = `"a$(touch pwned)b"@example.com`

	for _, format := range []string{"bicep", "arm"} {
		t.Run(format, func(t *testing.T) {
			zipBytes, _, err := buildAzureTemplateZip(format, data, "azure", "target")
			require.NoError(t, err)
			script := azureDeployScriptFromZip(t, zipBytes)

			m := registerURLWordRe.FindStringSubmatch(script)
			require.Len(t, m, 2, "deploy-azure.sh must POST to the registration URL")
			got, dir := evalShellWord(t, bashPath, `printf '%s' `+m[1])
			assert.Equal(t, apiURL+"/api/register", got, "the registration URL must evaluate verbatim")
			assert.NoFileExists(t, filepath.Join(dir, "pwned"),
				"a command substitution in the API URL must not run")

			// The params file interpolates no fields, so it gets no shell
			// escaping. Pin that: if a field is ever added to it, this fails
			// and the new value needs JSON escaping, not shell escaping.
			paramsTmpl, err := iacfiles.Templates.ReadFile("templates/azure-wif-bicep-params.json.tmpl")
			require.NoError(t, err)
			assert.Equal(t, string(paramsTmpl), azureZipEntry(t, zipBytes, "azure-wif-bicep-params.json"))
		})
	}
}

// TestBuildAzureTemplateZip_BenignDataRendersUnchanged pins that escaping
// is a no-op for ordinary values: with a plain email and URL the deploy
// script is byte-identical to the unescaped render.
func TestBuildAzureTemplateZip_BenignDataRendersUnchanged(t *testing.T) {
	data := buildGenericIaCData("azure", "gcp", "https://cudly.example.com")
	data.ContactEmail = "admin@example.com"
	want, err := renderTemplate("templates/azure-wif-bicep-deploy.sh.tmpl", data)
	require.NoError(t, err)
	require.Contains(t, want, `CONTACT_EMAIL="admin@example.com"`)

	for _, format := range []string{"bicep", "arm"} {
		t.Run(format, func(t *testing.T) {
			zipBytes, _, err := buildAzureTemplateZip(format, data, "azure", "target")
			require.NoError(t, err)
			assert.Equal(t, want, azureDeployScriptFromZip(t, zipBytes))
		})
	}
}
