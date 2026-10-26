//go:build integration

package api

// Real-Postgres regression tests for issue #1234: approving or rejecting a
// registration with the admin API key wrote the non-UUID sentinel
// "admin-api-key" into account_registrations.reviewed_by (a UUID foreign key
// to users), so Postgres raised SQLSTATE 22P02 and the request returned 500.
// The requests go through HandleRequest so the router and the admin-key
// principal middleware are part of what is tested.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const registrationTestAdminKey = "registration-review-admin-key"

type registrationReviewFixture struct {
	*createConcurrencyFixture
	ctx    context.Context
	userID string
}

func newRegistrationReviewFixture(t *testing.T) *registrationReviewFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	f := &registrationReviewFixture{createConcurrencyFixture: newCreateConcurrencyFixture(ctx, t), ctx: ctx}
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email='creator@example.com'`).Scan(&f.userID))

	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", mock.Anything, "admin-token").
		Return(&Session{UserID: f.userID, Email: "creator@example.com"}, nil).Maybe()
	mockAuth.On("ValidateCSRFToken", mock.Anything, "admin-token", "test-csrf").Return(nil).Maybe()
	mockAuth.grantAdmin()
	f.handler.auth = mockAuth
	f.handler.apiKey = registrationTestAdminKey
	return f
}

// seedPendingRegistration stores a pending Azure registration. Azure keeps the
// external IDs non-numeric, so no fixture looks like an AWS account ID.
func (f *registrationReviewFixture) seedPendingRegistration(t *testing.T) *config.AccountRegistration {
	t.Helper()
	reg := &config.AccountRegistration{
		ReferenceToken: uuid.NewString(), Status: "pending", Provider: "azure",
		ExternalID: uuid.NewString(), AccountName: "Registration " + t.Name(),
		ContactEmail: "owner@example.com",
	}
	require.NoError(t, f.store.CreateAccountRegistration(f.ctx, reg))
	return reg
}

// adminKeyHeaders authenticate with the admin API key only (no session).
func adminKeyHeaders() map[string]string {
	return map[string]string{"x-api-key": registrationTestAdminKey, "Content-Type": "application/json"}
}

// sessionHeaders authenticate as the real users row seeded by the fixture.
func sessionHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer admin-token", "X-CSRF-Token": "test-csrf", "Content-Type": "application/json",
	}
}

// send is safe to call from any goroutine: it never calls t.FailNow.
func (f *registrationReviewFixture) send(headers map[string]string, path string, body any) (*events.LambdaFunctionURLResponse, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return f.handler.HandleRequest(f.ctx, &events.LambdaFunctionURLRequest{
		Headers: headers, Body: string(encoded),
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: http.MethodPost, Path: path},
		},
	})
}

func (f *registrationReviewFixture) post(t *testing.T, headers map[string]string, path string, body any) *events.LambdaFunctionURLResponse {
	t.Helper()
	resp, err := f.send(headers, path, body)
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

func approveBody(reg *config.AccountRegistration) CloudAccountRequest {
	return CloudAccountRequest{Name: reg.AccountName, Provider: reg.Provider, ExternalID: reg.ExternalID}
}

// reviewColumns reads the raw review columns so the assertions do not depend
// on the store's scan code.
type reviewColumns struct {
	status         string
	reviewedBy     *string
	transitionedBy *string
	reviewedAt     *time.Time
	cloudAccountID *string
	rejection      *string
}

func (f *registrationReviewFixture) readReviewColumns(t *testing.T, id string) reviewColumns {
	t.Helper()
	var c reviewColumns
	require.NoError(t, f.pool.QueryRow(f.ctx, `
		SELECT status, reviewed_by::text, transitioned_by::text, reviewed_at,
		       cloud_account_id::text, rejection_reason
		  FROM account_registrations WHERE id = $1`, id,
	).Scan(&c.status, &c.reviewedBy, &c.transitionedBy, &c.reviewedAt, &c.cloudAccountID, &c.rejection))
	return c
}

func TestRegistrationReview_AdminAPIKey_Postgres(t *testing.T) {
	f := newRegistrationReviewFixture(t)

	t.Run("approve", func(t *testing.T) {
		reg := f.seedPendingRegistration(t)
		resp := f.post(t, adminKeyHeaders(), "/api/registrations/"+reg.ID+"/approve", approveBody(reg))
		require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)

		var account config.CloudAccount
		require.NoError(t, json.Unmarshal([]byte(resp.Body), &account))
		require.NotEmpty(t, account.ID)

		got := f.readReviewColumns(t, reg.ID)
		assert.Equal(t, "approved", got.status)
		assert.Nil(t, got.reviewedBy, "the admin API key has no users row, so reviewed_by must be NULL")
		assert.Nil(t, got.transitionedBy)
		assert.NotNil(t, got.reviewedAt, "reviewed_at is still recorded")
		require.NotNil(t, got.cloudAccountID, "the post-approval link update must also succeed")
		assert.Equal(t, account.ID, *got.cloudAccountID)
	})

	t.Run("reject", func(t *testing.T) {
		reg := f.seedPendingRegistration(t)
		resp := f.post(t, adminKeyHeaders(), "/api/registrations/"+reg.ID+"/reject", RejectRequest{Reason: "not ours"})
		require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)

		got := f.readReviewColumns(t, reg.ID)
		assert.Equal(t, "rejected", got.status)
		assert.Nil(t, got.reviewedBy, "the admin API key has no users row, so reviewed_by must be NULL")
		assert.Nil(t, got.transitionedBy)
		assert.NotNil(t, got.reviewedAt, "reviewed_at is still recorded")
		require.NotNil(t, got.rejection)
		assert.Equal(t, "not ours", *got.rejection)
	})
}

// TestRegistrationReview_SessionReviewerStamped_Postgres guards the other direction: a
// real user's review must still be attributed to that user.
func TestRegistrationReview_SessionReviewerStamped_Postgres(t *testing.T) {
	f := newRegistrationReviewFixture(t)

	approved := f.seedPendingRegistration(t)
	resp := f.post(t, sessionHeaders(), "/api/registrations/"+approved.ID+"/approve", approveBody(approved))
	require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)
	got := f.readReviewColumns(t, approved.ID)
	require.NotNil(t, got.reviewedBy)
	assert.Equal(t, f.userID, *got.reviewedBy)
	require.NotNil(t, got.transitionedBy)
	assert.Equal(t, f.userID, *got.transitionedBy)

	rejected := f.seedPendingRegistration(t)
	resp = f.post(t, sessionHeaders(), "/api/registrations/"+rejected.ID+"/reject", RejectRequest{Reason: "dup"})
	require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)
	got = f.readReviewColumns(t, rejected.ID)
	require.NotNil(t, got.reviewedBy)
	assert.Equal(t, f.userID, *got.reviewedBy)
}

// TestRegistrationReview_AdminAPIKey_ConcurrentApprove_Postgres fires several admin-key
// approvals of one registration at once. Before the fix every one of them
// failed with 500; now exactly one must win, the rest must get 409, and only
// one cloud account may be created.
func TestRegistrationReview_AdminAPIKey_ConcurrentApprove_Postgres(t *testing.T) {
	f := newRegistrationReviewFixture(t)
	reg := f.seedPendingRegistration(t)

	const callers = 8
	codes := make([]int, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := f.send(adminKeyHeaders(), "/api/registrations/"+reg.ID+"/approve", approveBody(reg))
			if err != nil || resp == nil {
				codes[i] = -1
				return
			}
			codes[i] = resp.StatusCode
		}()
	}
	close(start)
	wg.Wait()

	counts := map[int]int{}
	for _, c := range codes {
		counts[c]++
	}
	assert.Equal(t, map[int]int{http.StatusOK: 1, http.StatusConflict: callers - 1}, counts)

	var accounts int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM cloud_accounts WHERE provider = 'azure' AND external_id = $1`, reg.ExternalID,
	).Scan(&accounts))
	assert.Equal(t, 1, accounts)

	got := f.readReviewColumns(t, reg.ID)
	assert.Equal(t, "approved", got.status)
	assert.Nil(t, got.reviewedBy)
}
