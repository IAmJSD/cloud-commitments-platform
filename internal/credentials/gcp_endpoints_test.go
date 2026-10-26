package credentials

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testSTSURL           = "https://sts.googleapis.com/v1/token"
	testImpersonationURL = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/sa@test-project.iam.gserviceaccount.com:generateAccessToken"
)

// endpointCase is a token_url or service_account_impersonation_url value
// that ValidateGCPWIFEndpoints must refuse.
type endpointCase struct{ name, url string }

// badTokenURLs are token_url values that are not exactly
// https://sts.googleapis.com/...: other schemes, look-alike hosts, user info
// and explicit ports.
var badTokenURLs = []endpointCase{
	{"http scheme", "http://sts.googleapis.com/v1/token"},
	{"other scheme", "ftp://sts.googleapis.com/v1/token"},
	{"no scheme", "sts.googleapis.com/v1/token"},
	{"scheme-relative", "//sts.googleapis.com/v1/token"},
	{"suffix look-alike", "https://sts.googleapis.com.example.net/v1/token"},
	{"prefix look-alike", "https://my-sts.googleapis.com/v1/token"},
	{"other google host", "https://iamcredentials.googleapis.com/v1/token"},
	{"other host", "https://token.example.net/v1/token"},
	{"trailing dot", "https://sts.googleapis.com./v1/token"},
	{"userinfo before host", "https://sts.googleapis.com@token.example.net/v1/token"},
	{"userinfo on google host", "https://user:pass@sts.googleapis.com/v1/token"},
	{"explicit default port", "https://sts.googleapis.com:443/v1/token"},
	{"explicit other port", "https://sts.googleapis.com:8443/v1/token"},
	{"empty port", "https://sts.googleapis.com:/v1/token"},
	{"ip literal", "https://127.0.0.1/v1/token"},
	{"ipv6 literal", "https://[::1]/v1/token"},
	{"uppercase host", "https://STS.GOOGLEAPIS.COM/v1/token"},
	{"unparsable", "https://sts.googleapis.com/%zz"},
}

var badImpersonationURLs = []endpointCase{
	{"http scheme", "http://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/sa:generateAccessToken"},
	{"suffix look-alike", "https://iamcredentials.googleapis.com.example.net/v1/x"},
	{"prefix look-alike", "https://my-iamcredentials.googleapis.com/v1/x"},
	{"sts host", "https://sts.googleapis.com/v1/x"},
	{"userinfo before host", "https://iamcredentials.googleapis.com@token.example.net/v1/x"},
	{"explicit port", "https://iamcredentials.googleapis.com:443/v1/x"},
}

func TestValidateGCPWIFEndpoints_AcceptsGoogleEndpoints(t *testing.T) {
	require.NoError(t, ValidateGCPWIFEndpoints(testSTSURL, testImpersonationURL))
	require.NoError(t, ValidateGCPWIFEndpoints(testSTSURL, ""), "impersonation URL is optional")
	require.NoError(t, ValidateGCPWIFEndpoints("https://sts.googleapis.com/v1/token?x=1", ""))
}

func TestValidateGCPWIFEndpoints_RequiresTokenURL(t *testing.T) {
	err := ValidateGCPWIFEndpoints("", testImpersonationURL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_url is required")
}

func TestValidateGCPWIFEndpoints_RejectsBadTokenURL(t *testing.T) {
	for _, tc := range badTokenURLs {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGCPWIFEndpoints(tc.url, testImpersonationURL)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "token_url must be an https URL on sts.googleapis.com")
		})
	}
}

func TestValidateGCPWIFEndpoints_RejectsBadImpersonationURL(t *testing.T) {
	for _, tc := range badImpersonationURLs {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGCPWIFEndpoints(testSTSURL, tc.url)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "service_account_impersonation_url must be an https URL on iamcredentials.googleapis.com")
		})
	}
}

// wifConfigJSON returns a gcloud-style external_account config (the shape
// `gcloud iam workload-identity-pools create-cred-config` writes) with the
// given endpoints. An empty impersonation URL leaves the key out.
func wifConfigJSON(t *testing.T, tokenURL, impersonationURL string) []byte {
	t.Helper()
	cfg := map[string]any{
		"universe_domain":    "googleapis.com",
		"type":               "external_account",
		"audience":           "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url":          tokenURL,
		"credential_source": map[string]any{
			"file":   "/var/run/secrets/azure/tokens/azure-identity-token",
			"format": map[string]any{"type": "text"},
		},
	}
	if impersonationURL != "" {
		cfg["service_account_impersonation_url"] = impersonationURL
	}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	return raw
}

// loadStoredWIF resolves a WIF-mode account whose stored config is raw. With
// federated set, the options and audience would allow the secret-free path,
// which a stored config takes priority over.
func loadStoredWIF(t *testing.T, raw []byte, federated bool) error {
	t.Helper()
	store := newMockStore()
	store.data["acct1/"+CredTypeGCPWIFConfig] = raw
	account := &config.CloudAccount{ID: "acct1", GCPAuthMode: "workload_identity_federation"}
	opts := GCPResolveOptions{}
	if federated {
		account.GCPWIFAudience = "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/pool/providers/provider"
		opts = GCPResolveOptions{Signer: stubOIDCSigner{}, IssuerURL: "https://issuer.example.com"}
	}
	src, err := ResolveGCPTokenSourceWithOpts(context.Background(), account, store, opts)
	if err == nil {
		require.NotNil(t, src)
	} else {
		require.Nil(t, src)
	}
	return err
}

func TestLoadStoredGCPTokenSource_WIF_AcceptsGcloudConfig(t *testing.T) {
	require.NoError(t, loadStoredWIF(t, wifConfigJSON(t, testSTSURL, testImpersonationURL), false))
	require.NoError(t, loadStoredWIF(t, wifConfigJSON(t, testSTSURL, ""), false))
	require.NoError(t, loadStoredWIF(t, wifConfigJSON(t, testSTSURL, testImpersonationURL), true))
}

func TestLoadStoredGCPTokenSource_WIF_RejectsBadEndpoints(t *testing.T) {
	type stored struct{ name, tokenURL, impURL string }
	cases := make([]stored, 0, len(badTokenURLs)+len(badImpersonationURLs)+1)
	for _, tc := range badTokenURLs {
		cases = append(cases, stored{"token_url/" + tc.name, tc.url, testImpersonationURL})
	}
	for _, tc := range badImpersonationURLs {
		cases = append(cases, stored{"impersonation/" + tc.name, testSTSURL, tc.url})
	}
	cases = append(cases, stored{"token_url/missing", "", testImpersonationURL})
	for _, tc := range cases {
		for _, federated := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				err := loadStoredWIF(t, wifConfigJSON(t, tc.tokenURL, tc.impURL), federated)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "stored gcp workload identity config for account acct1 is not usable")
				assert.NotContains(t, err.Error(), "example.net", "the error must not echo the stored URL")
			})
		}
	}
}

// Decoding matches x/oauth2: JSON keys match struct tags case-insensitively
// and the last duplicate wins, so the checked value is the one it would use.
func TestLoadStoredGCPTokenSource_WIF_ChecksTheValueTheParserUses(t *testing.T) {
	cases := map[string]string{
		"case variant key": `{"type":"external_account","audience":"a","subject_token_type":"s",` +
			`"token_url":"` + testSTSURL + `","Token_URL":"https://token.example.net/v1/token","credential_source":{"file":"x"}}`,
		"duplicate key": `{"type":"external_account","audience":"a","subject_token_type":"s",` +
			`"token_url":"` + testSTSURL + `","token_url":"https://token.example.net/v1/token","credential_source":{"file":"x"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			err := loadStoredWIF(t, []byte(raw), false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "is not usable")
		})
	}
}

func TestLoadStoredGCPTokenSource_WIF_MalformedFailsWithoutPanic(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":             "not valid json",
		"token_url not text":   `{"type":"external_account","token_url":7}`,
		"type not text":        `{"type":["external_account"]}`,
		"top level not object": `["external_account"]`,
	} {
		t.Run(name, func(t *testing.T) {
			err := loadStoredWIF(t, []byte(raw), false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "parse gcp credentials")
		})
	}
}

// The service-account slot never needs an external_account config; if one
// is stored there it gets the same endpoint check before it is used.
func TestLoadStoredGCPTokenSource_ServiceAccountSlot_ChecksExternalAccountEndpoints(t *testing.T) {
	store := newMockStore()
	store.data["acct1/"+CredTypeGCPServiceAccount] = wifConfigJSON(t, "https://token.example.net/v1/token", "")
	account := &config.CloudAccount{ID: "acct1", GCPAuthMode: "service_account_key"}
	_, err := ResolveGCPTokenSource(context.Background(), account, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not usable")
}

func TestLoadStoredGCPTokenSource_WIF_EmptyStillReportsMissing(t *testing.T) {
	err := loadStoredWIF(t, nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no gcp credentials stored")
}
