//go:build integration

package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigPermissionConstraintsHTTP(t *testing.T) {
	ctx := context.Background()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store, authStore := config.NewPostgresStore(pg.DB), auth.NewPostgresStore(pg.DB)
	csrfKey := []byte(strings.Repeat("c", 32))
	authSvc := auth.NewService(auth.ServiceConfig{Store: authStore, CSRFKey: csrfKey})
	handler := api.NewHandler(api.HandlerConfig{ConfigStore: store, AuthService: newAuthServiceAdapter(authSvc)})
	app := &Application{API: handler, DB: pg.DB, dbConnected: true}
	server := httptest.NewServer(CreateHTTPServer(app, 0).Handler)
	t.Cleanup(server.Close)
	accounts := []string{uuid.NewString(), uuid.NewString()}
	for i, id := range accounts {
		require.NoError(t, store.CreateCloudAccount(ctx, &config.CloudAccount{ID: id, Name: id, Provider: "aws", ExternalID: fmt.Sprintf("%012d", i+1), Enabled: true}))
	}
	initial, err := store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	initial.LadderingEnabled, initial.RIExchangeEnabled, initial.RIExchangeMode = false, false, "manual"
	ladderBody := fmt.Sprintf(`{"cloud_account_id":%q,"provider":"aws","enabled":true,"mode":"email_approval","cadence":"daily","ramp_schedule":{"steps":[{"after_days":0,"fraction":1}]}}`, accounts[0])
	for _, route := range []struct{ name, path, body string }{
		{"ladder", "/api/ladder/configs", ladderBody},
		{"global", "/api/config", `{"laddering_enabled":true}`},
		{"exchange", "/api/ri-exchange/config", `{"auto_exchange_enabled":true,"mode":"manual","utilization_threshold":50,"lookback_days":30}`},
	} {
		for _, tc := range []struct {
			name        string
			constraints *auth.PermissionConstraints
			allowed     []string
			key         bool
			want        int
		}{
			{"unrestricted", nil, []string{"*"}, false, 200},
			{"account-mismatch", &auth.PermissionConstraints{AccountIDs: accounts[1:]}, []string{"*"}, false, 403},
			{"provider-mismatch", &auth.PermissionConstraints{Providers: []string{"azure"}}, []string{"*"}, false, 403},
			{"service-bounded", &auth.PermissionConstraints{Services: []string{"ec2"}}, []string{"*"}, false, 403},
			{"region-bounded", &auth.PermissionConstraints{Regions: []string{"us-east-1"}}, []string{"*"}, false, 403},
			{"all-current-accounts", &auth.PermissionConstraints{AccountIDs: accounts}, []string{"*"}, false, 403},
			{"matching-account", &auth.PermissionConstraints{AccountIDs: accounts[:1], Providers: []string{"aws"}}, accounts[:1], false, 403},
			{"independent-account-scope", nil, accounts[1:], false, 403},
			{"key-restricted", &auth.PermissionConstraints{Providers: []string{"azure"}}, []string{"*"}, true, 403},
			{"key-owner-restricted", &auth.PermissionConstraints{Providers: []string{"azure"}}, []string{"*"}, true, 403},
			{"key-unrestricted", nil, []string{"*"}, true, 200},
			{"amount-not-purchase", &auth.PermissionConstraints{MaxPurchaseAmount: 1}, []string{"*"}, false, 200},
			{"non-admin", nil, []string{"*"}, false, 200},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				want := tc.want
				if route.name == "ladder" && (tc.name == "all-current-accounts" || tc.name == "matching-account") {
					want = http.StatusOK
				}
				if route.name == "global" && tc.name == "non-admin" {
					want = http.StatusForbidden
				}
				if route.name == "ladder" && tc.name == "independent-account-scope" {
					want = http.StatusNotFound
				}
				require.NoError(t, store.SaveGlobalConfig(ctx, initial))
				before, loadErr := store.GetLadderConfigs(ctx)
				require.NoError(t, loadErr)
				if route.name == "ladder" && len(before) > 0 {
					before[0].Enabled = false
					_, loadErr = store.UpsertLadderConfig(ctx, &before[0])
					require.NoError(t, loadErr)
					before, loadErr = store.GetLadderConfigs(ctx)
					require.NoError(t, loadErr)
				}
				permission := auth.Permission{Action: auth.ActionUpdate, Resource: auth.ResourceConfig, Constraints: tc.constraints}
				if route.name == "global" && tc.name != "non-admin" {
					permission.Action, permission.Resource = auth.ActionAdmin, auth.ResourceAll
				}
				group := &auth.Group{Name: uuid.NewString(), Permissions: []auth.Permission{permission}, AllowedAccounts: tc.allowed}
				if tc.key && tc.name != "key-owner-restricted" {
					group.Permissions[0].Constraints = nil
				}
				require.NoError(t, authStore.CreateGroup(ctx, group))
				user := &auth.User{Email: uuid.NewString() + "@example.test", Active: true, GroupIDs: []string{group.ID}}
				require.NoError(t, authStore.CreateUser(ctx, user))
				token := uuid.NewString()
				hash := sha256.Sum256([]byte(token))
				require.NoError(t, authStore.CreateSession(ctx, &auth.Session{Token: hex.EncodeToString(hash[:]), UserID: user.ID, Email: user.Email, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}))
				mac := hmac.New(sha256.New, csrfKey)
				_, err = mac.Write([]byte(token))
				require.NoError(t, err)
				req, reqErr := http.NewRequestWithContext(ctx, http.MethodPut, server.URL+route.path, strings.NewReader(route.body))
				require.NoError(t, reqErr)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-CSRF-Token", hex.EncodeToString(mac.Sum(nil)))
				req.Header.Set("Content-Type", "application/json")
				if tc.key {
					if tc.name == "key-owner-restricted" {
						permission.Constraints = nil
					}
					key := "cudly_" + uuid.NewString()
					keyHash := sha256.Sum256([]byte(key))
					require.NoError(t, authStore.CreateAPIKey(ctx, &auth.UserAPIKey{UserID: user.ID, Name: "test", KeyPrefix: key[:12], KeyHash: base64.RawURLEncoding.EncodeToString(keyHash[:]), IsActive: true, Permissions: []auth.Permission{permission}}))
					req.Header.Set("X-API-Key", key)
				}
				resp, reqErr := server.Client().Do(req)
				require.NoError(t, reqErr)
				body, readErr := io.ReadAll(resp.Body)
				require.NoError(t, resp.Body.Close())
				require.NoError(t, readErr)
				assert.Equal(t, want, resp.StatusCode, "%s", body)
				after, loadErr := store.GetGlobalConfig(ctx)
				require.NoError(t, loadErr)
				ladders, loadErr := store.GetLadderConfigs(ctx)
				require.NoError(t, loadErr)
				if want != http.StatusOK {
					assert.Equal(t, initial, after)
					assert.Equal(t, before, ladders)
				} else {
					switch route.name {
					case "ladder":
						require.Len(t, ladders, 1)
						assert.True(t, ladders[0].Enabled)
					case "global":
						assert.True(t, after.LadderingEnabled)
					case "exchange":
						assert.True(t, after.RIExchangeEnabled)
					}
				}
			})
		}
	}
}
