//go:build integration

package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestApplicationRateLimiter(t *testing.T) {
	for _, key := range []string{
		"CREDENTIAL_ENCRYPTION_KEY_SECRET_ARN", "CREDENTIAL_ENCRYPTION_KEY_SECRET_NAME",
		"CREDENTIAL_ENCRYPTION_KEY_SECRET_ID", "CREDENTIAL_ENCRYPTION_ALLOW_DEV_KEY",
		"CUDLY_SIGNING_KEY_ID", "CUDLY_SIGNING_KEY_VAULT_URL", "CUDLY_SIGNING_KEY_NAME",
		"CUDLY_SIGNING_KEY_RESOURCE", "AWS_PROFILE", "AWS_LAMBDA_RUNTIME_API",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", strings.Repeat("12", 32))
	t.Setenv("CUDLY_ISSUER_URL", "https://rate-limit.example.test")
	t.Setenv("CUDLY_SOURCE_CLOUD", "aws")
	t.Setenv("SCHEDULED_TASK_AUTH_MODE", "disabled")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "local-rate-limit-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local-rate-limit-test")
	t.Setenv("AWS_REGION", "us-east-1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")

	for mode, isLambda := range []bool{false, true} {
		t.Run(fmt.Sprintf("lambda=%t", isLambda), func(t *testing.T) {
			cfg := ApplicationConfig{
				IsLambda: isLambda, DashboardURL: "https://rate-limit.example.test",
				DefaultTerm: 3, DefaultCoverage: 80,
				// Each 401 login runs Login's timing-equalizing bcrypt
				// compare, about 5s at cost 12 under -race. The limiter
				// does not depend on the cost.
				authBcryptCost: bcrypt.MinCost,
			}
			newApp := func() *Application {
				app, appErr := NewApplicationFromDeps(ctx, cfg, ExternalDeps{
					DBConfig: pg.Config, EmailSender: &noopEmailSender{},
				})
				require.NoError(t, appErr)
				t.Cleanup(func() { require.NoError(t, app.Close()) })
				return app
			}
			apps := []*Application{newApp(), newApp()}
			servers := make([]*httptest.Server, len(apps))
			for i, app := range apps {
				servers[i] = httptest.NewServer(CreateHTTPServer(app, 0).Handler)
				t.Cleanup(servers[i].Close)
			}
			client := &http.Client{Timeout: 10 * time.Second}
			ip := func(bucket int) string { return fmt.Sprintf("192.0.2.%d", mode*10+bucket) }
			request := func(replica int, path, sourceIP, body string) (int, error) {
				method := http.MethodGet
				if path == "/api/auth/login" || path == "/api/auth/forgot-password" ||
					strings.HasPrefix(path, "/api/purchases/") {
					method = http.MethodPost
				}
				req, reqErr := http.NewRequestWithContext(ctx, method, servers[replica].URL+path, strings.NewReader(body))
				if reqErr != nil {
					return 0, reqErr
				}
				req.Header.Set("X-Forwarded-For", sourceIP)
				req.Header.Set("Content-Type", "application/json")
				resp, reqErr := client.Do(req)
				if reqErr != nil {
					return 0, reqErr
				}
				defer resp.Body.Close()
				_, reqErr = io.Copy(io.Discard, resp.Body)
				return resp.StatusCode, reqErr
			}
			checkRequest := func(replica int, path, sourceIP string, want int) {
				t.Helper()
				body := ""
				if path == "/api/auth/login" {
					body = `{"email":"absent@example.test","password":"d3JvbmctcGFzc3dvcmQ="}`
				}
				if strings.HasPrefix(path, "/api/purchases/") {
					body = `{"token":"invalid"}`
				}
				status, reqErr := request(replica, path, sourceIP, body)
				require.NoError(t, reqErr)
				assert.Equal(t, want, status, "replica=%d path=%s source=%s", replica, path, sourceIP)
			}
			checkCount := func(sourceIP, endpoint string, want int) {
				t.Helper()
				var count int
				err := pg.DB.Pool().QueryRow(ctx, "SELECT count FROM rate_limits WHERE id = $1",
					"IP#"+sourceIP+"#ENDPOINT#"+endpoint).Scan(&count)
				assert.NoError(t, err)
				assert.Equal(t, want, count)
			}

			for i := range 7 {
				want := http.StatusUnauthorized
				if i >= 5 {
					want = http.StatusTooManyRequests
				}
				checkRequest(i%2, "/api/auth/login", ip(1), want)
			}
			checkCount(ip(1), "login", 7)
			checkRequest(1, "/api/auth/login", ip(2), http.StatusUnauthorized)
			checkCount(ip(2), "login", 1)

			for i := range 32 {
				action := "approve"
				if i%2 == 1 {
					action = "cancel"
				}
				want := http.StatusUnauthorized
				if i >= 30 {
					want = http.StatusTooManyRequests
				}
				checkRequest(i%2, "/api/purchases/"+action+"/00000000-0000-4000-8000-000000000109", ip(3), want)
			}
			checkCount(ip(3), "approve_cancel_public", 32)

			// Forgot-password per-IP cap (#13): distinct addresses from one
			// client share one bucket across replicas, and a client-supplied
			// X-Forwarded-For entry left of the proxy-appended one opens no
			// fresh bucket.
			forgot := func(replica, i int, xff string) (int, error) {
				return request(replica, "/api/auth/forgot-password", xff,
					fmt.Sprintf(`{"email":"probe%d@example.test"}`, i))
			}
			for i := range 12 {
				want := http.StatusOK
				if i >= 10 {
					want = http.StatusTooManyRequests
				}
				status, reqErr := forgot(i%2, i, fmt.Sprintf("198.51.100.%d, %s", i+1, ip(7)))
				require.NoError(t, reqErr)
				assert.Equal(t, want, status, "forgot-password attempt %d", i+1)
			}
			checkCount(ip(7), "forgot_password_ip", 12)

			type outcome struct {
				status int
				err    error
			}
			forgotResults := make(chan outcome, 15)
			for i := range 15 {
				go func() {
					status, reqErr := forgot(i%2, 100+i, ip(8))
					forgotResults <- outcome{status, reqErr}
				}()
			}
			forgotStatuses := make(map[int]int)
			for range 15 {
				result := <-forgotResults
				require.NoError(t, result.err)
				forgotStatuses[result.status]++
			}
			assert.Equal(t, map[int]int{http.StatusOK: 10, http.StatusTooManyRequests: 5}, forgotStatuses)
			checkCount(ip(8), "forgot_password_ip", 15)

			results := make(chan outcome, 12)
			for i := range 12 {
				go func() {
					status, reqErr := request(i%2, "/api/auth/login", ip(4), "{")
					results <- outcome{status, reqErr}
				}()
			}
			statuses := make(map[int]int)
			for range 12 {
				result := <-results
				require.NoError(t, result.err)
				statuses[result.status]++
			}
			assert.Equal(t, map[int]int{http.StatusBadRequest: 5, http.StatusTooManyRequests: 7}, statuses)
			checkCount(ip(4), "login", 12)

			apps[0].DB.Close()
			checkRequest(0, "/api/auth/login", ip(5), http.StatusServiceUnavailable)
			checkRequest(1, "/api/auth/login", ip(5), http.StatusUnauthorized)
			checkCount(ip(5), "login", 1)

			cold := newApp()
			canceled, stop := context.WithCancel(ctx)
			stop()
			req := httptest.NewRequestWithContext(canceled, http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
			req.Header.Set("X-Forwarded-For", ip(6))
			response := httptest.NewRecorder()
			CreateHTTPServer(cold, 0).Handler.ServeHTTP(response, req)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.False(t, cold.dbConnected)
			var count int
			require.NoError(t, pg.DB.Pool().QueryRow(ctx, "SELECT COUNT(*) FROM rate_limits WHERE id = $1",
				"IP#"+ip(6)+"#ENDPOINT#login").Scan(&count))
			assert.Zero(t, count)
		})
	}
}
