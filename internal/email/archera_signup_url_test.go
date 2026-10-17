package email

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #48: the email templates link to the same Archera signup URL as the
// CLI and the web frontend. The expected value is written out literally here
// on purpose rather than read from common.ArcheraSignupURL: if the shared
// constant is ever pointed at a wrong destination, these tests fail instead
// of following it.
const (
	wantArcheraSignupURL   = "https://www.archera.ai/cudly"
	legacyArcheraSignupURL = "https://archera.ai/signup?mode=cudly"

	// The two partnership disclosures that must stay visible wherever the
	// signup link is shown, in the wording the email templates use.
	archeraNonGatingDisclosure   = "Optional, set up on Archera's site"
	archeraSponsorshipDisclosure = "Archera sponsors CUDly's development"
)

func archeraEmailData() NotificationData {
	return NotificationData{
		DashboardURL:        "https://dashboard.example.com",
		ApprovalToken:       "tkn-48",
		ExecutionID:         "exec-48",
		TotalUpfrontCost:    500.00,
		TotalSavings:        50.00,
		ArcheraEducationURL: "https://dashboard.example.com/archera-insurance",
		Recommendations: []RecommendationSummary{{
			Service: "ec2", ResourceType: "m5.large", Region: "us-east-1",
			Count: 1, MonthlySavings: 50.00,
		}},
	}
}

// Every template family that embeds the Archera signup link: purchase
// confirmation (text only) and purchase approval request (text and HTML).
var archeraSignupRenderers = []struct {
	name    string
	render  func(NotificationData) (string, error)
	section string // heading that opens the Archera block in this template
	link    string // how the URL must appear in the rendered body
}{
	{
		name:    "purchase confirmation (text)",
		render:  RenderPurchaseConfirmationEmail,
		section: "ARCHERA INSURANCE",
		link:    "Sign up:   " + wantArcheraSignupURL + "\n",
	},
	{
		name:    "purchase approval request (text)",
		render:  RenderPurchaseApprovalRequestEmail,
		section: "ARCHERA INSURANCE",
		link:    "Sign up:   " + wantArcheraSignupURL + "\n",
	},
	{
		name:    "purchase approval request (HTML)",
		render:  RenderPurchaseApprovalRequestEmailHTML,
		section: ">Archera Insurance</h2>",
		link:    `href="` + wantArcheraSignupURL + `"`,
	},
}

func TestArcheraSignupURL_EmailTemplatesUseCanonicalURL(t *testing.T) {
	for _, tc := range archeraSignupRenderers {
		t.Run(tc.name, func(t *testing.T) {
			body, err := tc.render(archeraEmailData())
			require.NoError(t, err)

			assert.Contains(t, body, tc.link)
			assert.NotContains(t, body, legacyArcheraSignupURL)
			assert.NotContains(t, body, "archera.ai/signup")
		})
	}
}

// The signup link must never appear without both disclosures in the same
// Archera block.
func TestArcheraSignupURL_DisclosuresStayNextToLink(t *testing.T) {
	for _, tc := range archeraSignupRenderers {
		t.Run(tc.name, func(t *testing.T) {
			body, err := tc.render(archeraEmailData())
			require.NoError(t, err)

			start := strings.Index(body, tc.section)
			require.GreaterOrEqual(t, start, 0, "Archera block heading %q not found", tc.section)
			block := body[start:]

			assert.Contains(t, block, wantArcheraSignupURL)
			assert.Contains(t, block, archeraNonGatingDisclosure)
			assert.Contains(t, block, archeraSponsorshipDisclosure)
		})
	}
}

// Without an education URL the whole Archera block, signup link included, is
// left out.
func TestArcheraSignupURL_OmittedWithoutEducationURL(t *testing.T) {
	for _, tc := range archeraSignupRenderers {
		t.Run(tc.name, func(t *testing.T) {
			data := archeraEmailData()
			data.ArcheraEducationURL = ""
			body, err := tc.render(data)
			require.NoError(t, err)

			assert.NotContains(t, body, wantArcheraSignupURL)
		})
	}
}

// No email template source in this package may still carry the old link,
// including templates added later that the renderers above do not cover.
func TestArcheraSignupURL_NoTemplateSourceHasLegacyURL(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f) //nolint:gosec // fixed glob over this package's own source files
		require.NoError(t, err)
		assert.NotContains(t, string(src), legacyArcheraSignupURL, "%s still contains the legacy Archera signup URL", f)
		assert.NotContains(t, string(src), "archera.ai/signup", "%s still contains an archera.ai/signup link", f)
	}
}
