package api

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Regression tests for issue #1102 item 3: PUT /api/accounts/{id} validated
// the request's provider and echoed it back, but UpdateCloudAccount never
// writes the provider column, so the change was silently dropped. A provider
// change is now refused with a 400 before anything is saved.

func updateAccountRequest(body, id string) *events.LambdaFunctionURLRequest {
	req := adminRequest(body)
	req.RequestContext.HTTP.Method = "PUT"
	req.RequestContext.HTTP.Path = "/api/accounts/" + id
	return req
}

func TestUpdateAccount_ProviderChangeIsRejected(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	setupAdminAuth(ctx, mockAuth)

	existing := sampleAccount() // provider aws
	store := new(MockConfigStore)
	store.On("GetCloudAccount", ctx, existing.ID).Return(&existing, nil)
	// Registered so the pre-fix handler reaches the save and returns the
	// echoed account instead of panicking on an unexpected call.
	store.On("UpdateCloudAccount", ctx, mock.Anything).Return(nil).Maybe()
	handler := &Handler{auth: mockAuth, config: store}

	body := `{"name":"Test Account","provider":"gcp","external_id":"123456789012","gcp_auth_mode":"application_default"}`
	result, err := handler.updateAccount(ctx, updateAccountRequest(body, existing.ID), existing.ID)

	assert.Nil(t, result, "a provider that is never saved must not be echoed back")
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "want a client error, got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, ce.message, "provider cannot be changed")
	assert.Contains(t, ce.message, "aws")
	store.AssertNotCalled(t, "UpdateCloudAccount", mock.Anything, mock.Anything)
}

func TestUpdateAccount_SameProviderIsSaved(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	setupAdminAuth(ctx, mockAuth)

	existing := sampleAccount()
	store := new(MockConfigStore)
	store.On("GetCloudAccount", ctx, existing.ID).Return(&existing, nil)
	store.On("UpdateCloudAccount", ctx, mock.MatchedBy(func(a *config.CloudAccount) bool {
		return a.ID == existing.ID && a.Provider == "aws" && a.Name == "Renamed"
	})).Return(nil).Once()
	handler := &Handler{auth: mockAuth, config: store}

	body := `{"name":"Renamed","provider":"aws","external_id":"123456789012"}`
	result, err := handler.updateAccount(ctx, updateAccountRequest(body, existing.ID), existing.ID)
	require.NoError(t, err)
	saved, ok := result.(*config.CloudAccount)
	require.True(t, ok)
	assert.Equal(t, "aws", saved.Provider)
	assert.Equal(t, "Renamed", saved.Name)
	store.AssertExpectations(t)
}
