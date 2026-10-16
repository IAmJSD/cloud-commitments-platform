package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// evalShellWord evaluates one line of shell in a fresh bash, in an empty
// temporary directory and with an empty environment, and returns what it
// printed plus the directory, so callers can check that no command
// substitution created a file there. Only printf and variable assignments run.
func evalShellWord(t *testing.T, bashPath, script string) (out, dir string) {
	t.Helper()
	dir = t.TempDir()
	cmd := exec.Command(bashPath, "--noprofile", "--norc", "-c", script)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	got, err := cmd.Output()
	require.NoError(t, err, "bash must evaluate %q", script)
	return string(got), dir
}

var (
	contactEmailLineRe = regexp.MustCompile(`(?m)^CONTACT_EMAIL=.*$`)
	registerURLWordRe  = regexp.MustCompile(`(?m)-X POST ("[^\n]*/api/register") \\$`)
)

// TestGetFederationIaC_AzureDeployScriptEscapesSessionEmail is the
// regression test for #958. deploy-azure.sh in the Bicep and ARM bundles
// embeds the session user's email inside a double-quoted bash string. The
// email only has to pass mail.ParseAddress, which accepts backticks and
// "$(...)", so the script must carry it shell-escaped: running the
// CONTACT_EMAIL assignment must yield the email verbatim and run nothing.
func TestGetFederationIaC_AzureDeployScriptEscapesSessionEmail(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	t.Setenv("CUDLY_SOURCE_CLOUD", "gcp")

	emails := []string{
		"\"a`touch pwned`b\"@example.com",
		`"a$(touch pwned)b"@example.com`,
		"a`id`b@example.com",
		`"a\"b\\c$HOME"@example.com`,
	}
	for _, email := range emails {
		_, err := mail.ParseAddress(email)
		require.NoError(t, err, "fixture %q must be an address the auth service accepts", email)

		mockAuth := new(MockAuthService)
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

				assign := contactEmailLineRe.FindString(script)
				require.NotEmpty(t, assign, "deploy-azure.sh must assign CONTACT_EMAIL")
				got, dir := evalShellWord(t, bashPath, assign+`; printf '%s' "$CONTACT_EMAIL"`)
				assert.Equal(t, email, got, "CONTACT_EMAIL must evaluate to the email verbatim")
				assert.NoFileExists(t, filepath.Join(dir, "pwned"),
					"a command substitution in the email must not run")
			})
		}
	}
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
	require.Contains(t, want, `CONTACT_EMAIL="${CUDLY_CONTACT_EMAIL:-admin@example.com}"`)

	for _, format := range []string{"bicep", "arm"} {
		t.Run(format, func(t *testing.T) {
			zipBytes, _, err := buildAzureTemplateZip(format, data, "azure", "target")
			require.NoError(t, err)
			assert.Equal(t, want, azureDeployScriptFromZip(t, zipBytes))
		})
	}
}
