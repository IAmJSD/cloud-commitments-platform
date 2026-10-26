//go:build integration

package api

// Real-Postgres check for issue #601. PostgresStore reports a missing RI
// exchange as an error, not a nil record, so before the fix the approve and
// reject email links answered a missing id with 500 and an existing one with
// a wrong token with 403: an oracle for which exchange ids exist, open to
// callers with no session. These tests drive HandleRequest against the real
// store and compare the two answers.

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

const oracleExchangeRawToken = "oracle-exchange-raw-token"

// oracleMissingID is a well-formed UUID that no seeded exchange uses.
const oracleMissingID = "550e8400-e29b-41d4-a716-446655440601"

func setupOracleExchangeStore(ctx context.Context, t *testing.T) *config.PostgresStore {
	t.Helper()
	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Cleanup(context.Background()) })
	require.NoError(t, migrations.RunMigrations(ctx, container.DB.Pool(), getMigrationsPath(), "", ""))
	return config.NewPostgresStore(container.DB)
}

// seedOracleExchange stores a pending exchange; rawToken "" stores none.
func seedOracleExchange(ctx context.Context, t *testing.T, store *config.PostgresStore, rawToken string, expiresAt time.Time) string {
	t.Helper()
	rec := &config.RIExchangeRecord{
		AccountID:          "123456789012",
		Region:             "us-east-1",
		SourceRIIDs:        []string{"ri-601"},
		SourceInstanceType: "m5.large",
		SourceCount:        1,
		TargetOfferingID:   "offering-601",
		TargetInstanceType: "c5.large",
		TargetCount:        1,
		PaymentDue:         "10.00",
		Status:             "pending",
		ApprovalToken:      rawToken,
		Mode:               "manual",
		ExpiresAt:          &expiresAt,
	}
	require.NoError(t, store.SaveRIExchangeRecord(ctx, rec))
	return rec.ID
}

func oracleRequest(action, id, token string, headers map[string]string) *events.LambdaFunctionURLRequest {
	if headers == nil {
		headers = map[string]string{}
	}
	return &events.LambdaFunctionURLRequest{
		Headers:               headers,
		QueryStringParameters: map[string]string{"token": token},
		RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
			Method: "POST", Path: "/api/ri-exchange/" + action + "/" + id}},
	}
}

func TestRIExchangeTokenRoutes_Integration_MissingAnswersLikeWrongToken(t *testing.T) {
	ctx := context.Background()
	store := setupOracleExchangeStore(ctx, t)
	h := &Handler{config: store}

	_, lookupErr := store.GetRIExchangeRecord(ctx, oracleMissingID)
	require.ErrorIs(t, lookupErr, config.ErrNotFound, "fixture: the id must be missing, reported as PostgresStore does")

	pending := seedOracleExchange(ctx, t, store, oracleExchangeRawToken, time.Now().Add(24*time.Hour))
	noToken := seedOracleExchange(ctx, t, store, "", time.Now().Add(24*time.Hour))
	expired := seedOracleExchange(ctx, t, store, oracleExchangeRawToken+"-expired", time.Now().Add(-time.Hour)) // tokens are unique

	for _, action := range []string{"approve", "reject"} {
		missing, missErr := h.HandleRequest(ctx, oracleRequest(action, oracleMissingID, "wrong", nil))
		require.NoError(t, missErr)
		t.Run(action+"/missing id is a 403, not a 500", func(t *testing.T) {
			assert.Equal(t, 403, missing.StatusCode, missing.Body)
		})
		for name, id := range map[string]string{"pending": pending, "no token": noToken, "expired": expired} {
			t.Run(action+"/wrong token on "+name+" answers like a missing id", func(t *testing.T) {
				got, err := h.HandleRequest(ctx, oracleRequest(action, id, "wrong", nil))
				require.NoError(t, err)
				assert.Equal(t, missing.StatusCode, got.StatusCode, got.Body)
				assert.Equal(t, missing.Body, got.Body)
				assert.Equal(t, missing.Headers, got.Headers)
			})
		}
	}

	// The refused requests must not have touched the rows.
	for _, id := range []string{pending, noToken, expired} {
		rec, getErr := store.GetRIExchangeRecord(ctx, id)
		require.NoError(t, getErr)
		assert.Equal(t, "pending", rec.Status)
	}

	// Control: the token holder still rejects, so the 403s above come from
	// the token check and not from a fixture the routes cannot act on.
	resp, err := h.HandleRequest(ctx, oracleRequest("reject", pending, oracleExchangeRawToken, nil))
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode, resp.Body)
}

// TestApproveRIExchange_Integration_SessionMissingIs404 covers the
// session-authed approve: a missing exchange is the documented 404, not the
// 500 the bare store error used to produce.
func TestApproveRIExchange_Integration_SessionMissingIs404(t *testing.T) {
	ctx := context.Background()
	store := setupOracleExchangeStore(ctx, t)

	a := new(MockAuthService)
	a.On("ValidateSession", mock.Anything, "admin-bearer").Return(&Session{UserID: "admin-uuid", Email: "admin@example.com"}, nil).Maybe()
	a.grantAdminPurchaser()
	a.On("ValidateCSRFToken", mock.Anything, "admin-bearer", "csrf-ok").Return(nil).Maybe()
	h := &Handler{config: store, auth: a}

	resp, err := h.HandleRequest(ctx, oracleRequest("approve", oracleMissingID, "",
		map[string]string{"authorization": "Bearer admin-bearer", "x-csrf-token": "csrf-ok"}))
	require.NoError(t, err)
	assert.Equal(t, 404, resp.StatusCode, resp.Body)
	assert.Contains(t, resp.Body, "exchange record not found")
}
