package api

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
)

// The read-triggered all-account collection kick needs the same unconstrained
// view:recommendations grant as the refresh endpoint. A holder that lacks it
// still reads the cache: no 403, just no kick.

func constrainedViewer(held *auth.PermissionConstraints) *MockAuthService {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.grantPermissionsScoped([]auth.Permission{
		{Action: auth.ActionView, Resource: auth.ResourceRecommendations, Constraints: held},
		{Action: auth.ActionView, Resource: auth.ResourcePurchases},
	}, nil)
	return mockAuth
}

func TestRecommendationsRead_ConstrainedGrantReadsWithoutKick(t *testing.T) {
	for name, held := range map[string]*auth.PermissionConstraints{
		"cap":      capOnly(),
		"account":  {AccountIDs: []string{scopedInAccount}},
		"provider": providerOnly(),
	} {
		for _, ep := range recsReadEndpoints {
			t.Run(name+"/"+ep.name, func(t *testing.T) {
				var allowed []bool
				h := &Handler{config: recsReadStore(), scheduler: capturingScheduler(t, &allowed), auth: constrainedViewer(held)}

				require.NoError(t, ep.call(h, context.Background(), scopedRequest("")))

				require.Len(t, allowed, 1)
				assert.False(t, allowed[0], "a constrained grant must not kick a collection")
			})
		}
	}
}

func TestRecommendationsRead_UnconstrainedGrantStillKicks(t *testing.T) {
	for _, ep := range recsReadEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			var allowed []bool
			h := &Handler{config: recsReadStore(), scheduler: capturingScheduler(t, &allowed), auth: constrainedViewer(nil)}

			require.NoError(t, ep.call(h, context.Background(), scopedRequest("")))

			require.Len(t, allowed, 1)
			assert.True(t, allowed[0])
		})
	}
}

func TestRecommendationsRead_ProbeErrorReadsWithoutKick(t *testing.T) {
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
	mockAuth.On("HasPermissionAPI", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(true, nil).Maybe()
	mockAuth.On("GetAllowedAccountsAPI", mock.Anything, mock.Anything).Return([]string(nil), nil)
	mockAuth.On("HasPermissionForConstraintsAPI", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(false, errors.New("lookup failed"))
	var allowed []bool
	h := &Handler{config: recsReadStore(), scheduler: capturingScheduler(t, &allowed), auth: mockAuth}

	_, err := h.getRecommendations(context.Background(), scopedRequest(""), map[string]string{})

	require.NoError(t, err)
	require.Len(t, allowed, 1)
	assert.False(t, allowed[0])
}

func TestRecommendationsRead_UserAPIKeyProbeUsesGlobalSetAndKeyID(t *testing.T) {
	for _, keyAllowed := range []bool{false, true} {
		mockAuth := new(MockAuthService)
		mockAuth.On("HasAPIKeyPermissionAPI", mock.Anything, "user-key", "view", "recommendations").Return("u2", "key-1", true, nil)
		mockAuth.On("GetAllowedAccountsAPI", mock.Anything, "u2").Return([]string(nil), nil)
		mockAuth.On("HasAPIKeyPermissionForConstraintsAPI", mock.Anything, "key-1", "u2", "view", "recommendations",
			mock.MatchedBy(isGlobalSet)).Return(keyAllowed, nil).Once()
		var allowed []bool
		h := &Handler{config: recsReadStore(), scheduler: capturingScheduler(t, &allowed), auth: mockAuth}
		ctx := contextWithPrincipal(context.Background(), &Principal{Kind: PrincipalUserAPIKey, UserID: "u2", APIKeyID: "key-1"})

		_, err := h.getRecommendations(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"x-api-key": "user-key"}}, map[string]string{})

		require.NoError(t, err)
		mockAuth.AssertExpectations(t)
		require.Len(t, allowed, 1)
		assert.Equal(t, keyAllowed, allowed[0])
	}
}
