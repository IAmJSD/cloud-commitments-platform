package api

// exchange_helpers_test.go — tests for federation IaC helpers.

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// checkDailyCap
// ---------------------------------------------------------------------------

func TestHandler_getFederationIaC_MissingTarget(t *testing.T) {
	ctx := context.Background()
	h := federationHandler(t)
	req := federationReq(map[string]string{
		"source":     "aws",
		"account_id": "11111111-1111-1111-1111-111111111111",
	})
	_, err := h.getFederationIaC(ctx, req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "target")
}

func TestHandler_getFederationIaC_SuccessWithoutAccountID(t *testing.T) {
	ctx := context.Background()
	h := federationHandler(t)
	req := federationReq(map[string]string{
		"target": "aws",
		"source": "gcp",
		"format": "cli",
	})
	res, err := h.getFederationIaC(ctx, req)
	require.NoError(t, err)
	assert.Contains(t, res.Filename, "aws-wif-cli.sh")
}

func TestRouter_getFederationIaCHandler(t *testing.T) {
	ctx := context.Background()
	h := &Handler{}
	r := newTestRouter(h)

	req := &events.LambdaFunctionURLRequest{
		QueryStringParameters: map[string]string{
			"source": "aws",
		},
	}
	// Missing target → error (but the handler is exercised)
	_, err := r.getFederationIaCHandler(ctx, req, nil)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// slugify
// ---------------------------------------------------------------------------

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"My Account Name", "my-account-name"},
		{"account_123", "account-123"},
		{"  spaces  ", "spaces"},
		{"", ""},
		{"UPPER-CASE", "upper-case"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, slugify(tt.input))
		})
	}
}

// ---------------------------------------------------------------------------
// awsOIDCIssuer and gcpOIDCIssuerURI helpers
// ---------------------------------------------------------------------------

func TestAwsOIDCIssuer(t *testing.T) {
	assert.Contains(t, awsOIDCIssuer("azure", "tenant-id"), "login.microsoftonline.com/tenant-id")
	assert.Contains(t, awsOIDCIssuer("azure", ""), "AZURE_TENANT_ID")
	assert.Equal(t, "https://accounts.google.com", awsOIDCIssuer("gcp", ""))
	assert.Equal(t, "", awsOIDCIssuer("unknown", ""))
}

func TestGcpOIDCIssuerURI(t *testing.T) {
	assert.Contains(t, gcpOIDCIssuerURI("azure", "my-tenant"), "my-tenant")
	assert.Contains(t, gcpOIDCIssuerURI("azure", ""), "AZURE_TENANT_ID")
	assert.Equal(t, "", gcpOIDCIssuerURI("gcp", ""))
}
