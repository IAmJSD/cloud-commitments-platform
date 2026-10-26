package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"io/fs"
	"net/mail"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/iacfiles"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1436: the #958 fix (CONTACT_EMAIL default on its own line, JSON
// escaping of the registration payload) ported to the other eight generated
// scripts that self-register with CUDly.

// registrationScriptPrelude stubs the cloud CLIs and sets the variables the
// registration blocks read, so each block runs under set -u with no CLI, no
// cloud and no network (curl is past the end of the block).
const registrationScriptPrelude = `aws() { printf '%s\n' stub-aws-output; }
az() { printf '%s\n' 00000000-0000-0000-0000-0000000000aa; }
PROFILE_ARG= REGION=us-east-1 STACK_NAME=stub-stack ROLE_ARN=stub-role EXTERNAL_ID=stub-external-id
SUBSCRIPTION_ID=00000000-0000-0000-0000-0000000000aa TENANT_ID=00000000-0000-0000-0000-0000000000bb APP_ID=00000000-0000-0000-0000-0000000000cc
PROJECT_ID=stub-project SERVICE_ACCOUNT_EMAIL=stub-sa@stub-project.iam.gserviceaccount.com WIF_AUDIENCE=stub-audience
`

// registrationScript is one generated script that self-registers with CUDly,
// and how a download produces it.
type registrationScript struct {
	template               string // under internal/iacfiles/templates
	target, source, format string // the GET /api/federation/iac query
	zipEntry               string // the script's name in a zip download
}

// registrationScripts lists every shell template with a registration block
// except the Bicep/ARM deploy script, which #958 covers. Each is fetched
// through getFederationIaC, the real download path, except
// azure-wif-deploy.sh.tmpl: no download format ships it today, so it is
// rendered the way every shell template is (shellEscapeData, then
// renderTemplate).
var registrationScripts = []registrationScript{
	{template: "aws-wif-cli.sh.tmpl", target: "aws", source: "gcp", format: "cli"},
	{template: "aws-cross-account-cli.sh.tmpl", target: "aws", source: "aws", format: "cli"},
	{template: "azure-wif-cli.sh.tmpl", target: "azure", source: "gcp", format: "cli"},
	{template: "gcp-wif-cli.sh.tmpl", target: "gcp", source: "aws", format: "cli"},
	{template: "gcp-sa-impersonation-cli.sh.tmpl", target: "gcp", source: "gcp", format: "cli"},
	{template: "aws-cfn-deploy.sh.tmpl", target: "aws", source: "gcp", format: "cfn", zipEntry: "cloudformation/deploy-cfn.sh"},
	{template: "aws-cross-account-cfn-deploy.sh.tmpl", target: "aws", source: "aws", format: "cfn", zipEntry: "cloudformation/deploy-cfn.sh"},
	{template: "azure-wif-deploy.sh.tmpl", target: "azure", source: "gcp"},
}

// renderRegistrationScript returns the script sc produces for a session whose
// email is email.
func renderRegistrationScript(t *testing.T, sc registrationScript, email string) string {
	t.Helper()
	if sc.format == "" {
		data := buildGenericIaCData(sc.target, sc.source, "https://cudly.example.com")
		data.ContactEmail = email
		script, err := renderTemplate("templates/"+sc.template, shellEscapeData(data))
		require.NoError(t, err)
		return script
	}
	// Self-sourced targets need CUDly deployed on that cloud (#42).
	if sc.target == sc.source {
		t.Setenv("CUDLY_SOURCE_CLOUD", sc.source)
	} else {
		t.Setenv("CUDLY_SOURCE_CLOUD", "gcp")
	}
	h := federationHandlerWithEmail(t, email)
	res, err := h.getFederationIaC(context.Background(), federationReqWithDomain(map[string]string{
		"target": sc.target, "source": sc.source, "format": sc.format,
	}))
	require.NoError(t, err)
	if sc.zipEntry == "" {
		return res.Content
	}
	zipBytes, err := base64.StdEncoding.DecodeString(res.Content)
	require.NoError(t, err)
	return azureZipEntry(t, zipBytes, sc.zipEntry)
}

// TestRegistrationScripts_KeepContactEmailAndPayloadIntact is the regression
// test for #1436. For every hostile email (a "}", quotes, backslashes, a tab,
// command substitutions; all accepted by mail.ParseAddress) each script must
// parse, its registration block must leave CONTACT_EMAIL equal to the email,
// put it into valid JSON unchanged, and run nothing.
func TestRegistrationScripts_KeepContactEmailAndPayloadIntact(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for _, sc := range registrationScripts {
		for _, email := range hostileContactEmails {
			_, err := mail.ParseAddress(email)
			require.NoError(t, err, "fixture %q must be an address the auth service accepts", email)
			t.Run(sc.template+"/"+email, func(t *testing.T) {
				script := renderRegistrationScript(t, sc, email)

				syntax := exec.Command(bashPath, "-n")
				syntax.Stdin = bytes.NewReader([]byte(script))
				out, err := syntax.CombinedOutput()
				require.NoError(t, err, "%s must parse: %s", sc.template, out)

				got := evalRegistrationWith(t, bashPath, registrationScriptPrelude, script)
				assert.Equal(t, email, got.contactEmail, "CONTACT_EMAIL must evaluate to the email verbatim")
				assert.Equal(t, email, got.payload["contact_email"], "the payload must carry the email verbatim")
				assert.NoFileExists(t, filepath.Join(got.dir, "pwned"),
					"a command substitution in the email must not run")
			})
		}
	}
}

// TestRegistrationScripts_EnvOverridesAndAccountName pins the env-override
// semantics of the restructured CONTACT_EMAIL assignment (a set, non-empty
// CUDLY_CONTACT_EMAIL wins; unset or empty falls back to the pre-filled
// email, as ${VAR:-default} did) and that an operator-supplied
// CUDLY_ACCOUNT_NAME holding '"', '\' and a tab reaches the payload intact.
func TestRegistrationScripts_EnvOverridesAndAccountName(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	const prefilled = `"pre}filled"@example.com`
	const name = "Ops \"prod\" \\ team\tB"
	for _, sc := range registrationScripts {
		t.Run(sc.template, func(t *testing.T) {
			script := renderRegistrationScript(t, sc, prefilled)
			for _, tc := range []struct {
				name, wantEmail string
				env             []string
			}{
				{"unset", prefilled, nil},
				{"empty", prefilled, []string{"CUDLY_CONTACT_EMAIL="}},
				{"set", `"o}p\s"@example.com`, []string{`CUDLY_CONTACT_EMAIL="o}p\s"@example.com`}},
			} {
				got := evalRegistrationWith(t, bashPath, registrationScriptPrelude, script, tc.env...)
				assert.Equal(t, tc.wantEmail, got.contactEmail, tc.name)
				assert.Equal(t, tc.wantEmail, got.payload["contact_email"], tc.name)
			}
			got := evalRegistrationWith(t, bashPath, registrationScriptPrelude, script, "CUDLY_ACCOUNT_NAME="+name)
			assert.Equal(t, name, got.payload["account_name"], "account_name must be carried verbatim as JSON")
		})
	}
}

var (
	// contactEmailInDefaultRe matches the pre-#958 shape: the pre-filled
	// email as the default of a ${VAR:-...} expansion, where a "}" in it
	// closes the expansion early.
	contactEmailInDefaultRe = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:-[^}\n]*\{\{\s*\.ContactEmail\s*\}\}`)
	// rawPayloadFieldRe matches a payload line that puts CONTACT_EMAIL or
	// ACCOUNT_NAME into the JSON without json_escape.
	rawPayloadFieldRe = regexp.MustCompile(`"(contact_email|account_name)": "\$\{?(CONTACT_EMAIL|ACCOUNT_NAME)\}?"`)
	// jsonEscapeFuncRe matches the json_escape helper definition.
	jsonEscapeFuncRe = regexp.MustCompile(`(?m)^json_escape\(\) \{\n.*\n\}$`)
)

// TestRegistrationTemplates_NoDrift keeps the shell templates from drifting
// back: every one that registers with CUDly must be covered by the tests
// above (or be the Bicep script #958 covers), must not put the contact email
// inside a ${VAR:-default}, must JSON-escape the email and account name, and
// must carry the same json_escape helper as the Bicep script, so a fix to one
// copy cannot miss the others. A new registering template fails here until it
// is added to registrationScripts.
func TestRegistrationTemplates_NoDrift(t *testing.T) {
	covered := map[string]bool{"azure-wif-bicep-deploy.sh.tmpl": true}
	for _, sc := range registrationScripts {
		covered[sc.template] = true
	}

	bicep, err := iacfiles.Templates.ReadFile("templates/azure-wif-bicep-deploy.sh.tmpl")
	require.NoError(t, err)
	wantHelper := jsonEscapeFuncRe.FindString(string(bicep))
	require.NotEmpty(t, wantHelper, "the Bicep deploy script must define json_escape")

	names, err := fs.Glob(iacfiles.Templates, "templates/*.sh.tmpl")
	require.NoError(t, err)
	sort.Strings(names)
	var registering []string
	for _, name := range names {
		b, err := iacfiles.Templates.ReadFile(name)
		require.NoError(t, err)
		src, base := string(b), path.Base(name)
		if !strings.Contains(src, "/api/register") {
			continue
		}
		registering = append(registering, base)
		assert.True(t, covered[base], "%s registers with CUDly but is not in registrationScripts", base)
		assert.NotRegexp(t, contactEmailInDefaultRe, src, "%s puts the contact email inside a ${VAR:-default}", base)
		assert.NotRegexp(t, rawPayloadFieldRe, src, "%s puts an unescaped value into the registration JSON", base)
		assert.Contains(t, src, `"contact_email": "${CONTACT_EMAIL_JSON}"`, base)
		assert.Contains(t, src, `"account_name": "${ACCOUNT_NAME_JSON}"`, base)
		assert.Equal(t, wantHelper, jsonEscapeFuncRe.FindString(src), "%s must carry the Bicep script's json_escape", base)
	}
	assert.Len(t, registering, len(covered), "registering templates: %v", registering)
}
