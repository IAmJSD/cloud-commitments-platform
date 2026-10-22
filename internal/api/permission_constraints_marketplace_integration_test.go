//go:build integration

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The nil embedded interface fails loudly if the HTTP path needs an unimplemented operation.
type marketplaceAuthFixture struct {
	AuthServiceInterface
	service *auth.Service
}

func (a *marketplaceAuthFixture) ValidateSession(ctx context.Context, token string) (*Session, error) {
	s, err := a.service.ValidateSession(ctx, token)
	if err != nil {
		return nil, err
	}
	return &Session{UserID: s.UserID, Email: s.Email}, nil
}
func (a *marketplaceAuthFixture) ValidateCSRFToken(ctx context.Context, token, csrf string) error {
	return a.service.ValidateCSRFToken(ctx, token, csrf)
}
func (a *marketplaceAuthFixture) ValidateUserAPIKeyAPI(ctx context.Context, key string) (any, any, error) {
	return a.service.ValidateUserAPIKeyAPI(ctx, key)
}
func (a *marketplaceAuthFixture) RecordAPIKeyUsageAsync(key string) { a.service.RecordUsageAsync(key) }
func (a *marketplaceAuthFixture) GetAllowedAccountsAPI(ctx context.Context, user string) ([]string, error) {
	return a.service.ResolveAllowedAccounts(ctx, user)
}
func (a *marketplaceAuthFixture) HasPermissionAPI(ctx context.Context, user, action, resource string) (bool, error) {
	return a.service.HasPermissionAPI(ctx, user, action, resource)
}
func (a *marketplaceAuthFixture) HasAPIKeyPermissionAPI(ctx context.Context, key, action, resource string) (string, string, bool, error) {
	return a.service.HasAPIKeyPermissionAPI(ctx, key, action, resource)
}
func (a *marketplaceAuthFixture) HasPermissionForConstraintsAPI(ctx context.Context, user, action, resource string, sets []auth.PermissionConstraints) (bool, error) {
	return a.service.HasPermissionForConstraintsAPI(ctx, user, action, resource, sets)
}
func (a *marketplaceAuthFixture) HasAPIKeyPermissionForConstraintsAPI(ctx context.Context, key, user, action, resource string, sets []auth.PermissionConstraints) (bool, error) {
	return a.service.HasAPIKeyPermissionForConstraintsAPI(ctx, key, user, action, resource, sets)
}

func marketplaceSession(t *testing.T, store *auth.PostgresStore, permissions []auth.Permission, allowed []string, csrfKey []byte) (string, string, string) {
	t.Helper()
	ctx := t.Context()
	group := &auth.Group{Name: uuid.NewString(), Permissions: permissions, AllowedAccounts: allowed}
	require.NoError(t, store.CreateGroup(ctx, group))
	user := &auth.User{Email: uuid.NewString() + "@example.test", Active: true, GroupIDs: []string{group.ID}}
	require.NoError(t, store.CreateUser(ctx, user))
	token := uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	require.NoError(t, store.CreateSession(ctx, &auth.Session{Token: hex.EncodeToString(hash[:]), UserID: user.ID, Email: user.Email, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}))
	mac := hmac.New(sha256.New, csrfKey)
	_, err := mac.Write([]byte(token))
	require.NoError(t, err)
	return user.ID, token, hex.EncodeToString(mac.Sum(nil))
}

func TestMarketplacePermissionConstraintsHTTP(t *testing.T) {
	ctx := t.Context()
	pg, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Cleanup(context.Background())) })
	require.NoError(t, migrations.RunMigrations(ctx, pg.DB.Pool(), "../database/postgres/migrations", "", ""))
	store, authStore := config.NewPostgresStore(pg.DB), auth.NewPostgresStore(pg.DB)
	csrfKey := []byte(strings.Repeat("m", 32))
	handler := NewHandler(HandlerConfig{ConfigStore: store, AuthService: &marketplaceAuthFixture{service: auth.NewService(auth.ServiceConfig{Store: authStore, CSRFKey: csrfKey})}})
	handler.apiKey = uuid.NewString()
	handler.awsCfgOnce.Do(func() { handler.awsCfg = hostAWSConfig() })
	var providerCalls atomic.Int32
	handler.marketplaceEC2Factory = func(_ aws.Config) marketplaceEC2Client { providerCalls.Add(1); return &stubMarketplaceEC2{} }
	// HTTP transport, cached AWS config, and EC2 are fixtures; auth decisions and stores are real.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), 400)
			return
		}
		headers := make(map[string]string, len(r.Header))
		for name := range r.Header {
			headers[strings.ToLower(name)] = r.Header.Get(name)
		}
		resp, handleErr := handler.HandleRequest(r.Context(), &events.LambdaFunctionURLRequest{Body: string(body), Headers: headers, RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: r.Method, Path: r.URL.Path}}})
		if handleErr != nil {
			http.Error(w, handleErr.Error(), 500)
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.WriteString(w, resp.Body)
	}))
	t.Cleanup(server.Close)
	account, other := uuid.NewString(), uuid.NewString()
	for i, id := range []string{account, other} {
		require.NoError(t, store.CreateCloudAccount(ctx, &config.CloudAccount{ID: id, Name: id, Provider: "aws", ExternalID: []string{marketplaceHostAccount, "222222222222"}[i], AWSAuthMode: "role_arn", Enabled: true}))
	}
	matching := &auth.PermissionConstraints{AccountIDs: []string{account}, Providers: []string{"aws"}, Services: []string{"ec2"}, Regions: []string{" US-EAST-1 "}}
	own := auth.Permission{Action: auth.ActionSellOwn, Resource: auth.ResourcePurchases}
	anyGrant := auth.Permission{Action: auth.ActionSellAny, Resource: auth.ResourcePurchases}
	for _, route := range []string{"marketplace-list", "marketplace-cancel"} {
		for _, tc := range []struct {
			name                 string
			constraints          *auth.PermissionConstraints
			action               string
			allowed              []string
			key                  *auth.Permission
			credentials, missing string
			extraOwn             bool
			want                 int
		}{
			{name: "unrestricted", want: 200},
			{name: "matching-any", constraints: matching, allowed: []string{account}, want: 200},
			{name: "matching-own", constraints: matching, action: auth.ActionSellOwn, allowed: []string{account}, want: 200},
			{name: "account", constraints: &auth.PermissionConstraints{AccountIDs: []string{other}}, want: 403},
			{name: "provider", constraints: &auth.PermissionConstraints{Providers: []string{"azure"}}, want: 403},
			{name: "service", constraints: &auth.PermissionConstraints{Services: []string{"rds"}}, want: 403},
			{name: "region", constraints: &auth.PermissionConstraints{Regions: []string{"us-west-2"}}, want: 403},
			{name: "admin", action: auth.ActionAdmin, want: 200},
			{name: "admin-bounded", action: auth.ActionAdmin, constraints: &auth.PermissionConstraints{Providers: []string{"azure"}}, want: 403},
			{name: "account-scope", allowed: []string{other}, want: 403},
			{name: "admin-account-scope", action: auth.ActionAdmin, allowed: []string{other}, want: 403},
			{name: "any-denial-terminal", constraints: &auth.PermissionConstraints{Providers: []string{"azure"}}, extraOwn: true, want: 403},
			{name: "key-bounded", key: &auth.Permission{Action: auth.ActionSellAny, Resource: auth.ResourcePurchases, Constraints: &auth.PermissionConstraints{Regions: []string{"us-west-2"}}}, want: 403},
			{name: "owner-bounded", constraints: &auth.PermissionConstraints{Regions: []string{"us-west-2"}}, key: &anyGrant, want: 403},
			{name: "key-own", extraOwn: true, key: &own, allowed: []string{account}, want: 200},
			{name: "owner-any-does-not-imply-own", key: &own, want: 403},
			{name: "mixed-owner", key: &anyGrant, credentials: "different-owner", want: 403},
			{name: "key-only", key: &anyGrant, credentials: "key-only", want: 401},
			{name: "infrastructure-only", credentials: "infrastructure-only", want: 401},
			{name: "infrastructure-bearer", credentials: "infrastructure-bearer", want: 401},
			{name: "invalid-key-fallback", credentials: "invalid-key", want: 200},
			{name: "missing-credentials", credentials: "none", want: 401},
			{name: "malformed-bearer", credentials: "malformed", want: 401},
			{name: "missing-verb", action: auth.ActionView, want: 403},
			{name: "key-missing-verb", key: &auth.Permission{Action: auth.ActionView, Resource: auth.ResourcePurchases}, want: 403},
			{name: "missing-account", missing: "account", constraints: matching, want: 403},
			{name: "resolved-account", missing: "account-resolved", constraints: matching, allowed: []string{account}, want: 200},
			{name: "missing-provider", missing: "provider", constraints: matching, want: 403},
			{name: "missing-service", missing: "service", constraints: matching, want: 403},
			{name: "missing-region", missing: "region", constraints: matching, want: 403},
			{name: "legacy-account", missing: "account", want: 200},
			{name: "legacy-provider", missing: "provider", want: 200},
			{name: "legacy-service", missing: "service", want: 200},
			{name: "legacy-region", missing: "region", want: 200},
			{name: "own-missing-account", action: auth.ActionSellOwn, missing: "account", want: 403},
			{name: "amount-not-sale", constraints: &auth.PermissionConstraints{MaxPurchaseAmount: 1}, want: 200},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				permission := auth.Permission{Action: tc.action, Resource: auth.ResourcePurchases, Constraints: tc.constraints}
				if permission.Action == "" {
					permission.Action = auth.ActionSellAny
				}
				if permission.Action == auth.ActionAdmin {
					permission.Resource = auth.ResourceAll
				}
				permissions := []auth.Permission{permission}
				if tc.extraOwn {
					permissions = append(permissions, own)
				}
				allowed := tc.allowed
				if allowed == nil {
					allowed = []string{"*"}
				}
				user, token, csrf := marketplaceSession(t, authStore, permissions, allowed, csrfKey)
				key := ""
				if tc.key != nil {
					key = "cudly_" + uuid.NewString()
					hash := sha256.Sum256([]byte(key))
					require.NoError(t, authStore.CreateAPIKey(ctx, &auth.UserAPIKey{UserID: user, Name: "marketplace", KeyPrefix: key[:12], KeyHash: base64.RawURLEncoding.EncodeToString(hash[:]), IsActive: true, Permissions: []auth.Permission{*tc.key}}))
				}
				row := standardRow()
				row.PurchaseID, row.CloudAccountID, row.Provider, row.Service = uuid.NewString(), &account, "aws", "ec2"
				switch tc.missing {
				case "account", "account-resolved":
					// A row with no cloud account is signed with the host's
					// credentials only when it belongs to the host account (#261).
					// The save resolves the registered host account (#1235);
					// "account" clears it again to keep a legacy unlinked row.
					row.CloudAccountID, row.AccountID = nil, marketplaceHostAccount
				case "provider":
					row.Provider = " "
				case "service":
					row.Service = ""
				case "region":
					row.Region = ""
				}
				require.NoError(t, store.SavePurchaseHistory(ctx, row))
				if tc.missing == "account" {
					_, err := pg.DB.Pool().Exec(ctx, `UPDATE purchase_history SET cloud_account_id = NULL WHERE purchase_id = $1`, row.PurchaseID)
					require.NoError(t, err)
				}
				if route == "marketplace-cancel" {
					require.NoError(t, store.UpdatePurchaseHistoryListing(ctx, row.PurchaseID, "ril-existing", config.ListingStateActive))
				}
				before, loadErr := store.GetPurchaseHistoryByPurchaseID(ctx, row.PurchaseID)
				require.NoError(t, loadErr)
				if tc.credentials == "different-owner" {
					_, token, csrf = marketplaceSession(t, authStore, []auth.Permission{anyGrant}, []string{"*"}, csrfKey)
				}
				req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/purchases/"+row.PurchaseID+"/"+route, strings.NewReader(`{}`))
				require.NoError(t, reqErr)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-CSRF-Token", csrf)
				req.Header.Set("X-API-Key", key)
				req.Header.Set("Content-Type", "application/json")
				switch tc.credentials {
				case "none", "key-only", "infrastructure-only":
					req.Header.Del("Authorization")
					req.Header.Del("X-CSRF-Token")
				case "malformed":
					req.Header.Set("Authorization", "Bearer invalid")
				case "invalid-key":
					req.Header.Set("X-API-Key", "invalid")
				}
				if strings.HasPrefix(tc.credentials, "infrastructure-") {
					req.Header.Set("X-API-Key", handler.apiKey)
				}
				providerCalls.Store(0)
				resp, reqErr := server.Client().Do(req)
				require.NoError(t, reqErr)
				body, readErr := io.ReadAll(resp.Body)
				require.NoError(t, resp.Body.Close())
				require.NoError(t, readErr)
				assert.Equal(t, tc.want, resp.StatusCode, "%s", body)
				after, loadErr := store.GetPurchaseHistoryByPurchaseID(ctx, row.PurchaseID)
				require.NoError(t, loadErr)
				if tc.want != 200 {
					assert.Equal(t, before, after)
					assert.Zero(t, providerCalls.Load())
				} else {
					assert.EqualValues(t, 1, providerCalls.Load())
					wantState := config.ListingStateActive
					if route == "marketplace-cancel" {
						wantState = config.ListingStateCancelled
					}
					assert.Equal(t, wantState, after.ListingState)
					assert.NotEqual(t, before.ListingState, after.ListingState)
				}
			})
		}
	}
}
