package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
)

// Issue #1098: update-any Constraints on pause/resume follow the API key, not
// its owner. The keyScopeExec row is aws/ec2/us-east-1, created by another
// user, and the owner's group holds an unbounded update-any.
func TestPauseResumePlanned_APIKeyUpdateAnyFollowsKeyConstraints(t *testing.T) {
	keyPerms := func(provider string) []auth.Permission {
		return []auth.Permission{
			{Action: auth.ActionUpdate, Resource: auth.ResourcePurchases},
			revokePerm(auth.ActionUpdateAny, &auth.PermissionConstraints{Providers: []string{provider}}),
		}
	}
	for _, rt := range keyScopeRoutes {
		if rt.name != "pause" && rt.name != "resume" {
			continue
		}
		t.Run(rt.name+"/key bounded to another provider is refused", func(t *testing.T) {
			code, reached := serveKeyScope(t, rt, newKeyScopeAuth(keyPerms("azure")), keyScopeVictim, keyHeaders())
			assert.Equal(t, http.StatusForbidden, code)
			assert.False(t, reached, "a refused request must not reach the mutation")
		})
		t.Run(rt.name+"/key bounded to the row's provider is allowed", func(t *testing.T) {
			_, reached := serveKeyScope(t, rt, newKeyScopeAuth(keyPerms("aws")), keyScopeVictim, keyHeaders())
			assert.True(t, reached)
		})
	}
}

// An error evaluating update-any's Constraints is not a denial: it must not
// fall back to the creator branch (the session created the row, so that branch
// would allow) and must not succeed.
func TestPauseResumePlanned_UpdateAnyConstraintErrorDoesNotFallBack(t *testing.T) {
	t.Parallel()
	boom := errors.New("constraint store down")
	for _, op := range pauseResumeOps {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			owner := revokeConstraintsUser
			exec := cancelRouteExec(op.status)
			exec.CreatedByUserID = &owner
			store := new(MockConfigStore)
			store.On("GetExecutionByID", mock.Anything, cancelExecID).Return(exec, nil)
			store.On("TransitionExecutionStatus", mock.Anything, cancelExecID, mock.Anything, mock.Anything, mock.Anything).
				Return(nil, errKeyScopeReached).Maybe()
			m := new(MockAuthService)
			m.On("ValidateSession", mock.Anything, "sess-tok").Return(&Session{UserID: owner}, nil)
			m.On("GetAllowedAccountsAPI", mock.Anything, owner).Return([]string{}, nil)
			m.On("HasPermissionAPI", mock.Anything, owner, mock.Anything, auth.ResourcePurchases).Return(true, nil)
			m.On("HasPermissionForConstraintsAPI", mock.Anything, owner, auth.ActionUpdateAny, auth.ResourcePurchases, mock.Anything).Return(false, boom)

			h := &Handler{config: store, auth: m}
			resp, err := op.run(h)
			require.ErrorIs(t, err, boom)
			assert.Nil(t, resp)
			requireNotTransitioned(t, store)
		})
	}
}
