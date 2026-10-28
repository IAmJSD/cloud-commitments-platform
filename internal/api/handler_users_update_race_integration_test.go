//go:build integration

package api

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adminUserUpdateAuth adds the admin user-update entry point the shared
// marketplace fixture does not forward.
type adminUserUpdateAuth struct {
	marketplaceAuthFixture
}

func (a *adminUserUpdateAuth) UpdateUserAPI(ctx context.Context, actorUserID, userID string, req auth.APIUpdateUserRequest) (*auth.APIUser, error) {
	return a.service.UpdateUserAPI(ctx, actorUserID, userID, req)
}

// afterReadStore runs a hook once, right after the target user is read, to
// commit a concurrent write between the admin's read and its write.
type afterReadStore struct {
	auth.StoreInterface
	targetID  string
	afterRead func()
	once      sync.Once
}

func (s *afterReadStore) GetUserByID(ctx context.Context, id string) (*auth.User, error) {
	user, err := s.StoreInterface.GetUserByID(ctx, id)
	if err == nil && id == s.targetID && s.afterRead != nil {
		s.once.Do(s.afterRead)
	}
	return user, err
}

// Issue #493: PUT /api/users/{id} must not write a stale copy of the MFA
// columns it never meant to change.
func TestAdminUpdateUserHTTPKeepsConcurrentMFA(t *testing.T) {
	ctx := t.Context()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	authStore := auth.NewPostgresStore(pg.DB)
	csrfKey := []byte(strings.Repeat("u", 32))
	view := []auth.Permission{{Action: auth.ActionView, Resource: "recommendations"}}

	enrollMFA := func(db *database.Connection, userID string) {
		_, execErr := db.Pool().Exec(ctx, `UPDATE users SET mfa_enabled = true, mfa_secret = 'enrolled-secret',
			mfa_recovery_codes = ARRAY['recovery-hash'] WHERE id = $1`, userID)
		require.NoError(t, execErr)
	}

	setup := func(t *testing.T, afterRead func(victimID string)) (victimID string, put func(body string) *events.LambdaFunctionURLResponse) {
		t.Helper()
		victimID, _, _ = marketplaceSession(t, authStore, view, []string{"*"}, csrfKey)
		adminPerms := []auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}
		_, token, csrf := marketplaceSession(t, authStore, adminPerms, []string{"*"}, csrfKey)
		racing := &afterReadStore{StoreInterface: authStore, targetID: victimID}
		if afterRead != nil {
			racing.afterRead = func() { afterRead(victimID) }
		}
		service := auth.NewService(auth.ServiceConfig{Store: racing, CSRFKey: csrfKey})
		h := NewHandler(HandlerConfig{ConfigStore: config.NewPostgresStore(pg.DB), AuthService: &adminUserUpdateAuth{marketplaceAuthFixture{service: service}}})
		return victimID, func(body string) *events.LambdaFunctionURLResponse {
			resp, handleErr := h.HandleRequest(ctx, &events.LambdaFunctionURLRequest{
				Body:    body,
				Headers: map[string]string{"authorization": "Bearer " + token, "x-csrf-token": csrf, "content-type": "application/json"},
				RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
					Method: "PUT", Path: "/api/users/" + victimID,
				}},
			})
			require.NoError(t, handleErr)
			return resp
		}
	}

	t.Run("uncontended-update-keeps-mfa-and-applies-change", func(t *testing.T) {
		victimID, put := setup(t, nil)
		enrollMFA(pg.DB, victimID)
		resp := put(`{"active": false}`)
		require.Equal(t, 200, resp.StatusCode, resp.Body)
		stored, err := authStore.GetUserByID(ctx, victimID)
		require.NoError(t, err)
		assert.False(t, stored.Active)
		assert.NotNil(t, stored.DeactivatedAt)
		assert.True(t, stored.MFAEnabled)
		assert.Equal(t, "enrolled-secret", stored.MFASecret)
		assert.Equal(t, []string{"recovery-hash"}, stored.MFARecoveryCodes)
	})

	t.Run("concurrent-mfa-enrollment-survives", func(t *testing.T) {
		victimID, put := setup(t, func(id string) { enrollMFA(pg.DB, id) })
		resp := put(`{"active": false}`)
		assert.Equal(t, 200, resp.StatusCode, "a change to columns the edit does not touch must not conflict")
		stored, err := authStore.GetUserByID(ctx, victimID)
		require.NoError(t, err)
		assert.True(t, stored.MFAEnabled, "stale admin save erased the concurrent MFA enrollment")
		assert.Equal(t, "enrolled-secret", stored.MFASecret)
		assert.Equal(t, []string{"recovery-hash"}, stored.MFARecoveryCodes)
		assert.False(t, stored.Active)
	})

	t.Run("edited-column-changed-after-the-read-is-409", func(t *testing.T) {
		victimID, put := setup(t, func(id string) {
			_, execErr := pg.DB.Pool().Exec(ctx, `UPDATE users SET email = 'renamed-' || email WHERE id = $1`, id)
			require.NoError(t, execErr)
		})
		before, err := authStore.GetUserByID(ctx, victimID)
		require.NoError(t, err)
		resp := put(`{"active": false}`)
		assert.Equal(t, 409, resp.StatusCode, resp.Body)
		stored, err := authStore.GetUserByID(ctx, victimID)
		require.NoError(t, err)
		assert.True(t, stored.Active, "a conflicting write must not land")
		assert.Equal(t, "renamed-"+before.Email, stored.Email)
	})

	t.Run("active-flipped-after-the-read-is-409", func(t *testing.T) {
		victimID, put := setup(t, func(id string) {
			_, execErr := pg.DB.Pool().Exec(ctx, `UPDATE users SET active = false WHERE id = $1`, id)
			require.NoError(t, execErr)
		})
		resp := put(`{"active": false}`)
		assert.Equal(t, 409, resp.StatusCode, resp.Body)
		stored, err := authStore.GetUserByID(ctx, victimID)
		require.NoError(t, err)
		assert.False(t, stored.Active)
		assert.Nil(t, stored.DeactivatedAt, "a conflicting write must not stamp deactivation")
	})

	t.Run("groups-changed-after-the-read-is-409", func(t *testing.T) {
		other := &auth.Group{Name: "other-" + t.Name(), Permissions: view, AllowedAccounts: []string{"*"}}
		require.NoError(t, authStore.CreateGroup(ctx, other))
		victimID, put := setup(t, func(id string) {
			_, execErr := pg.DB.Pool().Exec(ctx, `UPDATE users SET group_ids = ARRAY[$2]::uuid[] WHERE id = $1`, id, other.ID)
			require.NoError(t, execErr)
		})
		resp := put(`{"active": false}`)
		assert.Equal(t, 409, resp.StatusCode, resp.Body)
		stored, err := authStore.GetUserByID(ctx, victimID)
		require.NoError(t, err)
		assert.Equal(t, []string{other.ID}, stored.GroupIDs)
		assert.True(t, stored.Active, "a conflicting write must not land")
	})
}
