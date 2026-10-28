package api

// Tests for the credential that may come with POST /api/register: it is
// checked when submitted and again when the registration is approved, with
// the same rules as POST /api/accounts/:id/credentials.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

const regCredRegID = "6f1c2a8e-3b4d-4e5f-8a9b-0c1d2e3f4a5b"

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// registrationBody builds a POST /api/register body; credential fields are
// left out when empty.
func registrationBody(t *testing.T, provider, credType, credPayload string) string {
	t.Helper()
	body := map[string]any{
		"provider": provider, "external_id": "test-project",
		"account_name": "Test", "contact_email": "owner@example.com",
	}
	if credType != "" {
		body["credential_type"] = credType
	}
	if credPayload != "" {
		body["credential_payload"] = credPayload
	}
	return mustJSON(t, body)
}

// submitWithCredential runs submitRegistration with a store that records the
// created registration. It returns the stored registration (nil when none
// was created) and the handler error.
func submitWithCredential(t *testing.T, h *Handler, body string) (*config.AccountRegistration, error) {
	t.Helper()
	ctx := context.Background()
	store := new(MockConfigStore)
	var created *config.AccountRegistration
	store.On("CreateAccountRegistration", ctx, mock.Anything).Run(func(args mock.Arguments) {
		created = args.Get(1).(*config.AccountRegistration)
	}).Return(nil).Maybe()
	h.config = store
	_, err := h.submitRegistration(ctx, &events.LambdaFunctionURLRequest{Body: body})
	return created, err
}

func TestSubmitRegistration_WithoutCredentialIsAccepted(t *testing.T) {
	reg, err := submitWithCredential(t, &Handler{credStore: &MockCredentialStore{}}, registrationBody(t, "gcp", "", ""))
	require.NoError(t, err)
	require.NotNil(t, reg)
	assert.Empty(t, reg.RegCredentialType)
	assert.Empty(t, reg.RegCredentialPayload)
}

func TestSubmitRegistration_AcceptsValidCredential(t *testing.T) {
	cases := map[string]struct{ provider, credType, payload string }{
		"gcp wif from gcloud": {"gcp", "gcp_workload_identity_config", mustJSON(t, gcloudWIFConfig())},
		"gcp wif with file source": {"gcp", "gcp_workload_identity_config", mustJSON(t, gcloudWIFConfigWith(
			"credential_source", map[string]any{"file": "/var/run/secrets/azure/tokens/azure-identity-token"}))},
		"aws access keys": {"aws", "aws_access_keys", `{"access_key_id":"AKIAEXAMPLE","secret_access_key":"placeholder"}`},
		"azure secret":    {"azure", "azure_client_secret", `{"client_secret":"placeholder"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reg, err := submitWithCredential(t, &Handler{credStore: &MockCredentialStore{}},
				registrationBody(t, tc.provider, tc.credType, tc.payload))
			require.NoError(t, err)
			require.NotNil(t, reg)
			assert.Equal(t, tc.credType, reg.RegCredentialType)
			// MockCredentialStore "encrypts" to the plaintext.
			assert.JSONEq(t, tc.payload, reg.RegCredentialPayload)
		})
	}
}

func TestSubmitRegistration_RejectsInvalidCredential(t *testing.T) {
	wif := "gcp_workload_identity_config"
	cases := map[string]struct{ provider, credType, payload, msg string }{
		"type without payload":       {"gcp", wif, "", "must be provided together"},
		"payload without type":       {"gcp", "", mustJSON(t, gcloudWIFConfig()), "must be provided together"},
		"unknown type":               {"gcp", "gcp_oauth_client", `{"a":"b"}`, "credential_type must be one of"},
		"gcp type for aws account":   {"aws", wif, mustJSON(t, gcloudWIFConfig()), `cannot be used with provider "aws"`},
		"aws type for gcp account":   {"gcp", "aws_access_keys", `{"access_key_id":"a","secret_access_key":"b"}`, `cannot be used with provider "gcp"`},
		"azure type for aws account": {"aws", "azure_client_secret", `{"client_secret":"x"}`, `cannot be used with provider "aws"`},
		"junk payload":               {"gcp", wif, "not json at all", "must be a JSON object"},
		"truncated payload":          {"gcp", wif, `{"type":"external_account"`, "must be a JSON object"},
		"array payload":              {"gcp", wif, `["external_account"]`, "must be a JSON object"},
		"null payload":               {"gcp", wif, "null", "must be a JSON object"},
		"string payload":             {"gcp", wif, `"external_account"`, "must be a JSON object"},
		"empty object":               {"gcp", wif, "{}", "must not be empty"},
		"missing token_url":          {"gcp", wif, mustJSON(t, gcloudWIFConfigWith("token_url", "")), `"token_url" must be a non-empty string`},
		"unknown key":                {"gcp", wif, mustJSON(t, gcloudWIFConfigWith("token_info_url", "https://sts.googleapis.com/v1/introspect")), `unknown key "token_info_url"`},
		"wrong wif type":             {"gcp", wif, mustJSON(t, gcloudWIFConfigWith("type", "service_account")), `type="external_account"`},
		"non-google token_url":       {"gcp", wif, mustJSON(t, gcloudWIFConfigWith("token_url", "https://token.example.net/v1/token")), "token_url must be an https URL on sts.googleapis.com"},
		"http token_url":             {"gcp", wif, mustJSON(t, gcloudWIFConfigWith("token_url", "http://sts.googleapis.com/v1/token")), "token_url must be an https URL"},
		"non-google impersonation": {"gcp", wif, mustJSON(t, gcloudWIFConfigWith("service_account_impersonation_url",
			"https://iamcredentials.googleapis.com.example.net/v1/x")), "service_account_impersonation_url must be an https URL"},
		"service account missing key": {"gcp", "gcp_service_account", `{"type":"service_account","project_id":"p","client_email":"e"}`, `missing required key "private_key"`},
		"aws keys missing secret":     {"aws", "aws_access_keys", `{"access_key_id":"a"}`, `missing required key "secret_access_key"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reg, err := submitWithCredential(t, &Handler{credStore: &MockCredentialStore{}},
				registrationBody(t, tc.provider, tc.credType, tc.payload))
			require400(t, err, tc.msg)
			assert.Nil(t, reg, "an invalid credential must not create a registration")
		})
	}
}

// Without a credential store nothing can be kept, so neither field is
// recorded on the registration (the payload was already dropped before).
func TestSubmitRegistration_NoCredentialStoreRecordsNoCredential(t *testing.T) {
	reg, err := submitWithCredential(t, &Handler{},
		registrationBody(t, "gcp", "gcp_workload_identity_config", mustJSON(t, gcloudWIFConfig())))
	require.NoError(t, err)
	require.NotNil(t, reg)
	assert.Empty(t, reg.RegCredentialType)
	assert.Empty(t, reg.RegCredentialPayload)
}

// recordingCredStore records SaveCredential calls.
type recordingCredStore struct {
	MockCredentialStore
	saved map[string][]byte
}

func (r *recordingCredStore) SaveCredential(_ context.Context, accountID, credType string, payload []byte) error {
	if r.saved == nil {
		r.saved = map[string][]byte{}
	}
	r.saved[accountID+"/"+credType] = payload
	return nil
}

const gcpApprovalBody = `{"name":"Test","provider":"gcp","external_id":"test-project",` +
	`"gcp_auth_mode":"workload_identity_federation","gcp_client_email":"` + regCredSAEmail + `"}`

// approveWithCredential approves a pending registration whose stored
// credential is credType/payload (MockCredentialStore "decrypts" to the
// stored text).
func approveWithCredential(t *testing.T, credType, payload, approvalBody string) (*MockConfigStore, *recordingCredStore, any, error) {
	t.Helper()
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("GetAccountRegistration", mock.Anything, regCredRegID).Return(&config.AccountRegistration{
		ID: regCredRegID, Status: "pending", Provider: "gcp", ExternalID: "test-project",
		RegCredentialType: credType, RegCredentialPayload: payload,
	}, nil)
	store.On("ApproveAccountRegistration", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	store.On("TransitionRegistrationStatus", mock.Anything, mock.Anything, "pending", mock.Anything).Return(nil).Maybe()
	store.On("UpdateAccountRegistration", mock.Anything, mock.Anything).Return(nil).Maybe()
	mockAuth := new(MockAuthService)
	setupAdminAuth(ctx, mockAuth)
	creds := &recordingCredStore{}
	h := &Handler{auth: mockAuth, config: store, credStore: creds}
	result, err := h.approveRegistration(ctx, &events.LambdaFunctionURLRequest{Body: approvalBody}, regCredRegID)
	return store, creds, result, err
}

func TestApproveRegistration_SavesCheckedCredential(t *testing.T) {
	payload := mustJSON(t, gcloudWIFConfig())
	store, creds, result, err := approveWithCredential(t, "gcp_workload_identity_config", payload, gcpApprovalBody)
	require.NoError(t, err)
	account, ok := result.(*config.CloudAccount)
	require.True(t, ok)
	assert.True(t, account.Enabled)
	store.AssertCalled(t, "ApproveAccountRegistration", mock.Anything, mock.Anything,
		mock.MatchedBy(func(a *config.CloudAccount) bool { return a.ID == account.ID }), mock.Anything)
	assertNoPiecemealApproval(t, store)
	require.Contains(t, creds.saved, account.ID+"/gcp_workload_identity_config")
	assert.JSONEq(t, payload, string(creds.saved[account.ID+"/gcp_workload_identity_config"]))
}

func TestApproveRegistration_WithoutCredentialSavesNothing(t *testing.T) {
	_, creds, _, err := approveWithCredential(t, "", "", gcpApprovalBody)
	require.NoError(t, err)
	assert.Empty(t, creds.saved)
}

// A registration stored before submit-time checks existed can still hold a
// credential that fails them. Approval refuses it with a 400 and leaves the
// registration pending: nothing is transitioned, created or saved.
func TestApproveRegistration_RefusesInvalidStoredCredential(t *testing.T) {
	wif := "gcp_workload_identity_config"
	cases := map[string]struct{ credType, payload, body, msg string }{
		"non-google token_url": {wif, mustJSON(t, gcloudWIFConfigWith("token_url", "https://token.example.net/v1/token")),
			gcpApprovalBody, "token_url must be an https URL on sts.googleapis.com"},
		"non-google impersonation url": {wif, mustJSON(t, gcloudWIFConfigWith("service_account_impersonation_url", "https://my-iamcredentials.googleapis.com/v1/x")),
			gcpApprovalBody, "service_account_impersonation_url must be an https URL"},
		"junk payload":           {wif, "not json", gcpApprovalBody, "must be a JSON object"},
		"payload without type":   {"", mustJSON(t, gcloudWIFConfig()), gcpApprovalBody, "must be provided together"},
		"unknown type":           {"gcp_oauth_client", `{"a":"b"}`, gcpApprovalBody, "credential_type must be one of"},
		"wrong type in wif slot": {wif, `{"type":"service_account","project_id":"p","private_key":"k","client_email":"e"}`, gcpApprovalBody, "unknown key"},
		"type does not match approved provider": {wif, mustJSON(t, gcloudWIFConfig()),
			`{"name":"Test","provider":"azure","external_id":"00000000-0000-0000-0000-000000000000","azure_auth_mode":"managed_identity"}`,
			`cannot be used with provider "azure"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store, creds, result, err := approveWithCredential(t, tc.credType, tc.payload, tc.body)
			assert.Nil(t, result)
			require400(t, err, "the credentials submitted with this registration are not valid")
			require400(t, err, tc.msg)
			store.AssertNotCalled(t, "ApproveAccountRegistration", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "TransitionRegistrationStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			store.AssertNotCalled(t, "CreateCloudAccount", mock.Anything, mock.Anything)
			assert.Empty(t, creds.saved)
		})
	}
}
