package api

// Tests for the endpoint URLs validateCredentialPayload accepts in a
// gcp_workload_identity_config payload.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const regCredSAEmail = "cudly@test-project.iam.gserviceaccount.com"

// gcloudWIFConfig is the external_account config the gcp-target IaC
// (iac/federation/gcp-target/terraform/registration.tf) and gcloud generate
// for an AWS-hosted deployment.
func gcloudWIFConfig() map[string]any {
	return map[string]any{
		"type":                              "external_account",
		"audience":                          "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/cudly/providers/cudly",
		"subject_token_type":                "urn:ietf:params:aws:token-type:aws4_request",
		"service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + regCredSAEmail + ":generateAccessToken",
		"token_url":                         "https://sts.googleapis.com/v1/token",
		"credential_source": map[string]any{
			"environment_id":                 "aws1",
			"region_url":                     "http://169.254.169.254/latest/meta-data/placement/availability-zone",
			"url":                            "http://169.254.169.254/latest/meta-data/iam/security-credentials",
			"regional_cred_verification_url": "https://sts.{region}.amazonaws.com?Action=GetCallerIdentity&Version=2011-06-15",
		},
	}
}

// gcloudWIFConfigWith returns gcloudWIFConfig with key set to value.
func gcloudWIFConfigWith(key string, value any) map[string]any {
	cfg := gcloudWIFConfig()
	cfg[key] = value
	return cfg
}

func require400(t *testing.T, err error, msg string) {
	t.Helper()
	ce, ok := IsClientError(err)
	require.True(t, ok, "want a client error, got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, ce.message, msg)
}

func TestValidateCredentialPayload_GCPWIFEndpoints(t *testing.T) {
	require.NoError(t, validateCredentialPayload("gcp_workload_identity_config", gcloudWIFConfig()))

	noImpersonation := gcloudWIFConfig()
	delete(noImpersonation, "service_account_impersonation_url")
	require.NoError(t, validateCredentialPayload("gcp_workload_identity_config", noImpersonation))

	cases := map[string]struct {
		key   string
		value any
		msg   string
	}{
		"token_url look-alike suffix":    {"token_url", "https://sts.googleapis.com.example.net/v1/token", "token_url must be an https URL on sts.googleapis.com"},
		"token_url look-alike prefix":    {"token_url", "https://my-sts.googleapis.com/v1/token", "token_url must be an https URL on sts.googleapis.com"},
		"token_url userinfo":             {"token_url", "https://sts.googleapis.com@token.example.net/v1/token", "token_url must be an https URL"},
		"token_url port":                 {"token_url", "https://sts.googleapis.com:8443/v1/token", "token_url must be an https URL"},
		"token_url http":                 {"token_url", "http://sts.googleapis.com/v1/token", "token_url must be an https URL"},
		"impersonation url other host":   {"service_account_impersonation_url", "https://token.example.net/v1/x", "service_account_impersonation_url must be an https URL"},
		"impersonation url not a string": {"service_account_impersonation_url", 42, `"service_account_impersonation_url" must be a non-empty string`},
		"impersonation url empty":        {"service_account_impersonation_url", " ", `"service_account_impersonation_url" must be a non-empty string`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateCredentialPayload("gcp_workload_identity_config", gcloudWIFConfigWith(tc.key, tc.value))
			require400(t, err, tc.msg)
		})
	}
}
