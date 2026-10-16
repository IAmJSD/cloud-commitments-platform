package api

// Negative MaxPurchaseAmount fails closed at every money-path enforcement
// point (issue #83). The mock auth service answers constraint checks with the
// real auth.PermissionsAllowForConstraintSets, so these exercise the
// production matcher. Nothing here reaches a purchase manager or provider.

import (
	"context"
	"math/big"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func capRec(upfront float64) config.RecommendationRecord {
	acct := "123456789012"
	return config.RecommendationRecord{
		ID: "rec-1", Provider: "aws", Service: "ec2", Region: "us-east-1",
		CloudAccountID: &acct, Count: 1, Term: 1, Payment: "all-upfront", UpfrontCost: upfront,
	}
}

func handlerWithCap(action string, maxAmount float64) *Handler {
	mockAuth := new(MockAuthService)
	mockAuth.grantPermissions([]auth.Permission{{
		Action: action, Resource: auth.ResourcePurchases,
		Constraints: &auth.PermissionConstraints{MaxPurchaseAmount: maxAmount},
	}})
	return &Handler{auth: mockAuth}
}

func TestEnforcePurchaseConstraints_NegativeCapRefuses(t *testing.T) {
	ctx := context.Background()
	session := &Session{UserID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}
	// The execute, direct-execute and approval verbs all share this check.
	for _, action := range []string{auth.ActionExecute, auth.ActionExecuteAny, auth.ActionApproveAny} {
		t.Run(action+"/negative refused", func(t *testing.T) {
			h := handlerWithCap(action, -1)
			err := h.enforcePurchaseConstraints(ctx, session, action, []config.RecommendationRecord{capRec(1)})
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a clientError, got %v", err)
			assert.Equal(t, 403, ce.code)
		})
		t.Run(action+"/positive cap enforced as before", func(t *testing.T) {
			h := handlerWithCap(action, 1000)
			require.NoError(t, h.enforcePurchaseConstraints(ctx, session, action, []config.RecommendationRecord{capRec(500)}))
			err := h.enforcePurchaseConstraints(ctx, session, action, []config.RecommendationRecord{capRec(5000)})
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok)
			assert.Equal(t, 403, ce.code)
		})
		t.Run(action+"/zero is no cap", func(t *testing.T) {
			h := handlerWithCap(action, 0)
			require.NoError(t, h.enforcePurchaseConstraints(ctx, session, action, []config.RecommendationRecord{capRec(1_000_000)}))
		})
	}
}

func TestCheckAzureExecuteConstraints_NegativeCapRefuses(t *testing.T) {
	ctx := context.Background()
	session := &Session{UserID: "dddddddd-dddd-dddd-dddd-dddddddddddd"}
	grant := func(maxAmount float64) *Handler {
		mockAuth := new(MockAuthService)
		mockAuth.grantPermissions([]auth.Permission{{
			Action: auth.ActionExecute, Resource: auth.ResourceRIExchange,
			Constraints: &auth.PermissionConstraints{MaxPurchaseAmount: maxAmount},
		}})
		return &Handler{auth: mockAuth}
	}
	regions := []string{"eastus"}
	small := big.NewRat(1, 1)

	for _, currency := range []string{"USD", "EUR"} {
		t.Run(currency+"/negative refused", func(t *testing.T) {
			err := grant(-1).checkAzureExecuteConstraints(ctx, session,
				AzureExecuteExchangeRequestBody{Currency: currency}, "sub-1", small, regions)
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "expected a clientError, got %v", err)
			assert.Equal(t, 403, ce.code)
		})
	}
	t.Run("USD/positive cap enforced as before", func(t *testing.T) {
		h := grant(1000)
		require.NoError(t, h.checkAzureExecuteConstraints(ctx, session,
			AzureExecuteExchangeRequestBody{Currency: "USD"}, "sub-1", big.NewRat(500, 1), regions))
		require.Error(t, h.checkAzureExecuteConstraints(ctx, session,
			AzureExecuteExchangeRequestBody{Currency: "USD"}, "sub-1", big.NewRat(5000, 1), regions))
	})
}

func TestMapCreateAPIKeyError_InvalidPermissionIs400(t *testing.T) {
	err := mapCreateAPIKeyError(auth.ErrInvalidPermission)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a clientError, got %v", err)
	assert.Equal(t, 400, ce.code)
}
