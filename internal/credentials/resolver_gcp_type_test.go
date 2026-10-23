package credentials

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures for the stored-JSON GCP path (issue #80). None of them reaches the
// network: building a token source is lazy and no test calls Token().

func gcpServiceAccountFixture() []byte {
	return []byte(`{
		"type": "service_account",
		"project_id": "test-project",
		"private_key_id": "key-id",
		"private_key": "` + minimalRSAPEM() + `",
		"client_email": "test@test-project.iam.gserviceaccount.com",
		"client_id": "placeholder-client-id",
		"token_uri": "https://oauth2.googleapis.com/token"
	}`)
}

func gcpExternalAccountFixture() []byte {
	return []byte(`{
		"type": "external_account",
		"audience": "//iam.googleapis.com/projects/0/locations/global/workloadIdentityPools/pool/providers/provider",
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url": "https://sts.googleapis.com/v1/token",
		"credential_source": {"file": "/nonexistent/cudly-test-token"}
	}`)
}

// gcpOtherTypeFixtures are credential JSONs the deprecated
// google.CredentialsFromJSON parsed successfully but that neither stored GCP
// slot may hold (the API accepts only "service_account" into
// gcp_service_account and only "external_account" into
// gcp_workload_identity_config, see validateCredentialPayload).
func gcpOtherTypeFixtures() map[string][]byte {
	return map[string][]byte{
		"authorized_user": []byte(`{
			"type": "authorized_user",
			"client_id": "placeholder-client-id",
			"client_secret": "placeholder",
			"refresh_token": "placeholder"
		}`),
		"impersonated_service_account": []byte(`{
			"type": "impersonated_service_account",
			"service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/sa@test-project.iam.gserviceaccount.com:generateAccessToken",
			"source_credentials": {
				"type": "authorized_user",
				"client_id": "placeholder-client-id",
				"client_secret": "placeholder",
				"refresh_token": "placeholder"
			}
		}`),
		"external_account_authorized_user": []byte(`{
			"type": "external_account_authorized_user",
			"audience": "//iam.googleapis.com/locations/global/workforcePools/pool/providers/provider",
			"client_id": "placeholder-client-id",
			"client_secret": "placeholder",
			"refresh_token": "placeholder",
			"token_url": "https://sts.googleapis.com/v1/oauthtoken"
		}`),
		// An OAuth client config (client_credentials.json) carries no "type";
		// CredentialsFromJSON turned it into an interactive auth-code flow.
		"installed client config, no type": []byte(`{
			"installed": {
				"client_id": "placeholder-client-id",
				"client_secret": "placeholder",
				"auth_uri": "https://accounts.google.com/o/oauth2/auth",
				"token_uri": "https://oauth2.googleapis.com/token",
				"redirect_uris": ["http://localhost"]
			}
		}`),
	}
}

func resolveStoredGCP(t *testing.T, authMode, credType string, raw []byte) error {
	t.Helper()
	store := newMockStore()
	store.data["acct1/"+credType] = raw
	account := &config.CloudAccount{ID: "acct1", GCPAuthMode: authMode}
	src, err := ResolveGCPTokenSource(context.Background(), account, store)
	if err == nil {
		require.NotNil(t, src)
	}
	return err
}

func TestResolveGCPTokenSource_ServiceAccountSlot_AcceptsServiceAccountJSON(t *testing.T) {
	for _, mode := range []string{"", "service_account_key"} {
		err := resolveStoredGCP(t, mode, CredTypeGCPServiceAccount, gcpServiceAccountFixture())
		require.NoError(t, err, "auth mode %q", mode)
	}
}

func TestResolveGCPTokenSource_ServiceAccountSlot_RejectsOtherTypes(t *testing.T) {
	cases := gcpOtherTypeFixtures()
	cases["external_account"] = gcpExternalAccountFixture()
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			err := resolveStoredGCP(t, "service_account_key", CredTypeGCPServiceAccount, raw)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "parse gcp credentials")
			assert.Contains(t, err.Error(), `expected credential type "service_account"`)
		})
	}
}

func TestResolveGCPTokenSource_WIFSlot_AcceptsExternalAccountJSON(t *testing.T) {
	// No signer/audience, so the legacy stored-JSON branch is taken.
	err := resolveStoredGCP(t, "workload_identity_federation", CredTypeGCPWIFConfig, gcpExternalAccountFixture())
	require.NoError(t, err)
}

func TestResolveGCPTokenSource_WIFSlot_RejectsOtherTypes(t *testing.T) {
	cases := gcpOtherTypeFixtures()
	cases["service_account"] = gcpServiceAccountFixture()
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			err := resolveStoredGCP(t, "workload_identity_federation", CredTypeGCPWIFConfig, raw)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "parse gcp credentials")
			assert.Contains(t, err.Error(), `expected credential type "external_account"`)
		})
	}
}

func TestResolveGCPTokenSource_StoredSlots_RejectMalformedJSON(t *testing.T) {
	slots := map[string]string{
		"service_account_key":          CredTypeGCPServiceAccount,
		"workload_identity_federation": CredTypeGCPWIFConfig,
	}
	inputs := map[string][]byte{
		"not json":      []byte("not valid json"),
		"truncated":     []byte(`{"type": "service_account"`),
		"array":         []byte(`["service_account"]`),
		"type not text": []byte(`{"type": 1}`),
		"empty object":  []byte(`{}`),
	}
	for mode, credType := range slots {
		for name, raw := range inputs {
			t.Run(mode+"/"+name, func(t *testing.T) {
				err := resolveStoredGCP(t, mode, credType, raw)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "parse gcp credentials")
			})
		}
	}
}

func TestLoadStoredGCPTokenSource_RejectsUnknownSlot(t *testing.T) {
	// A credential slot without a pinned JSON type must fail closed rather
	// than fall back to accepting any type.
	store := newMockStore()
	store.data["acct1/aws_access_keys"] = gcpServiceAccountFixture()
	account := &config.CloudAccount{ID: "acct1"}
	_, err := loadStoredGCPTokenSource(context.Background(), account, store, CredTypeAWSAccessKeys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no gcp credential json type")
}
