package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// API key usage must be booked once per HTTP REQUEST, never once per
// credential validation.
//
// A single request validates the same API key several times over: once to
// resolve the principal, then again inside every permission check, and once
// per verb on the multi-verb gates. While the async write was an idempotent
// last_used_at timestamp that multiplicity was harmless, but PR #1523 turned
// it into an additive counter, so booking on the validation path multiplied
// every reported number by a factor that varies per endpoint (2x on a plain
// gate, 5x on the four-verb purchase gate) and therefore cannot be divided
// back out of the stored totals.
//
// These tests live in package server rather than package api because they
// need the REAL auth.Service behind the handler: with api's mock auth service
// both the validation and the permission check are stubbed, so no booking
// path is exercised at all and the regression is structurally invisible. The
// same reason rules out a pure auth-package test -- the multiplicity comes
// from the handler calling down repeatedly, which only shows up when both
// layers are wired together.

// usageBookingRawKey is the raw API key value the harness authenticates with.
const usageBookingRawKey = "usage-booking-harness-api-key" // #nosec G101 -- test fixture, not a real credential

// usageBookingKeyHash mirrors how auth.Service.ValidateUserAPIKey derives the
// stored hash from the presented key, so the store mock can be keyed on it.
func usageBookingKeyHash() string {
	sum := sha256.Sum256([]byte(usageBookingRawKey))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// usageBookingHarness wires a real auth.Service (over a mock store) into a
// real api.Handler and observes both how many times a request re-validates
// the credential and how much usage reaches the store.
type usageBookingHarness struct {
	handler *api.Handler
	// flushes receives the delta of every RecordAPIKeyUsage call.
	flushes chan int64
	// totalDelta is the sum of all deltas written to the store.
	totalDelta atomic.Int64
	// validations counts credential validations. Every
	// ValidateUserAPIKey begins with exactly one GetAPIKeyByHash, so this
	// is the per-request multiplier the fix has to decouple usage from.
	validations atomic.Int64
}

// newUsageBookingHarness builds the harness. groupPermissions are granted to
// both the key owner's group and the key itself: an unscoped key now
// authorizes nothing (issue #61's fail-closed fix), so the harness scopes the
// key explicitly to the same permissions to exercise the booking path rather
// than the now-denied inheritance path.
func newUsageBookingHarness(t *testing.T, groupPermissions []auth.Permission) *usageBookingHarness {
	t.Helper()

	h := &usageBookingHarness{
		// Buffered well past the worst observed booking count so a flush
		// goroutine can never block on the test.
		flushes: make(chan int64, 64),
	}

	keyHash := usageBookingKeyHash()
	key := &auth.UserAPIKey{
		ID:          "key-1",
		UserID:      "user-1",
		Name:        "usage booking harness",
		KeyHash:     keyHash,
		IsActive:    true,
		Permissions: groupPermissions,
	}
	user := &auth.User{
		ID:       "user-1",
		Email:    "harness@example.com",
		Active:   true,
		GroupIDs: []string{"group-1"},
	}
	group := &auth.Group{ID: "group-1", Name: "harness", Permissions: groupPermissions}

	mockStore := new(auth.MockStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })

	mockStore.On("GetAPIKeyByHash", mock.Anything, keyHash).
		Run(func(mock.Arguments) { h.validations.Add(1) }).
		Return(key, nil)
	mockStore.On("GetUserByID", mock.Anything, "user-1").Return(user, nil)
	mockStore.On("GetGroup", mock.Anything, "group-1").Return(group, nil).Maybe()
	mockStore.On("ListAPIKeysByUser", mock.Anything, "user-1").
		Return([]*auth.UserAPIKey{key}, nil).Maybe()
	mockStore.On("RecordAPIKeyUsage", mock.Anything, "key-1", mock.Anything).
		Run(func(args mock.Arguments) {
			// Runs on the flush goroutine: only atomics and a non-blocking
			// send here, never a require.* call.
			delta, ok := args.Get(2).(int64)
			if !ok {
				// Surfaces as a negative total, failing the assertion loudly
				// rather than panicking into RecordUsageAsync's recover().
				delta = -1
			}
			h.totalDelta.Add(delta)
			select {
			case h.flushes <- delta:
			default:
			}
		}).
		Return(nil).Maybe()

	service := auth.NewService(auth.ServiceConfig{
		Store:           mockStore,
		SessionDuration: time.Hour,
		CSRFKey:         auth.TestCSRFKey(),
	})
	h.handler = api.NewHandler(api.HandlerConfig{
		AuthService:       newAuthServiceAdapter(service),
		CORSAllowedOrigin: "https://dashboard.example.com",
	})
	return h
}

// request builds an API-key-authenticated Lambda Function URL request.
func (h *usageBookingHarness) request(method, path string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"x-api-key": usageBookingRawKey},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: method, Path: path},
		},
	}
}

// requireExactlyOneBooking asserts the request booked exactly one request
// against the key. It must run inside the synctest bubble that ran the
// request.
//
// The bookings themselves are synchronous -- RecordUsageAsync increments an
// in-memory counter before returning -- so by the time HandleRequest returns,
// every booking the request will ever make has already been made; only the
// flush to the store runs on a goroutine. synctest.Wait returns once that
// goroutine has finished (or is durably blocked), so every flush has reached
// the store and the drain below sees all of them. With the bug present the
// surplus bookings surface as a flush carrying a delta above 1 or as a second
// flush, and both are caught. A fixed quiet window could instead pass a
// second flush that a loaded runner had not scheduled yet.
func (h *usageBookingHarness) requireExactlyOneBooking(t *testing.T) {
	t.Helper()

	synctest.Wait()
	var deltas []int64
	for drained := false; !drained; {
		select {
		case delta := <-h.flushes:
			deltas = append(deltas, delta)
		default:
			drained = true
		}
	}
	require.Equal(t, []int64{1}, deltas,
		"one HTTP request must book exactly 1 usage in one flush; flushes wrote %v (total %d): "+
			"usage is being counted per credential validation, not per request", deltas, h.totalDelta.Load())
}

// TestHandleRequest_UserAPIKey_BooksOneUsagePerRequest pins the single-verb
// path: GET /api/api-keys/usage-stats authenticates the key once and then
// re-validates it inside the view:api-keys permission check, so booking on
// the validation path reported one request as two.
func TestHandleRequest_UserAPIKey_BooksOneUsagePerRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newUsageBookingHarness(t, []auth.Permission{
			{Action: auth.ActionView, Resource: auth.ResourceAPIKeys},
		})

		resp, err := h.handler.HandleRequest(context.Background(),
			h.request("GET", "/api/api-keys/usage-stats"))

		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
		require.Greater(t, h.validations.Load(), int64(1),
			"harness precondition: this endpoint must re-validate the credential, otherwise it cannot detect per-validation booking")
		h.requireExactlyOneBooking(t)
	})
}

// TestHandleRequest_UserAPIKeyMultiVerbGate_BooksOneUsagePerRequest pins the
// worst case: requireDeleteOrCancelPurchasePermission walks four verbs and
// authorizeAPIKeyAny re-validates the credential once per verb, so booking on
// the validation path reported one request as five.
//
// The key holds none of the four verbs, so the gate walks the entire list.
// The resulting 403 is incidental -- what is pinned is that an authenticated
// request books one usage no matter how many permission checks it triggers.
func TestHandleRequest_UserAPIKeyMultiVerbGate_BooksOneUsagePerRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newUsageBookingHarness(t, []auth.Permission{
			{Action: auth.ActionView, Resource: auth.ResourceRecommendations},
		})

		resp, err := h.handler.HandleRequest(context.Background(),
			h.request("DELETE", "/api/purchases/planned/11111111-1111-1111-1111-111111111111"))

		require.NoError(t, err)
		require.Equal(t, 403, resp.StatusCode, "body: %s", resp.Body)
		require.GreaterOrEqual(t, h.validations.Load(), int64(5),
			"harness precondition: the four-verb gate must re-validate the credential once per verb")
		h.requireExactlyOneBooking(t)
	})
}

// TestHandleRequest_UserAPIKeyDeploymentInfo_BooksOneUsage pins issue #156:
// /api/info/deployment is AuthUser, but a prefix match on the public
// "/api/info" skipped the middleware's booking block for it.
func TestHandleRequest_UserAPIKeyDeploymentInfo_BooksOneUsage(t *testing.T) {
	t.Setenv("CUDLY_SOURCE_CLOUD", "gcp") // skip the best-effort STS lookup
	synctest.Test(t, func(t *testing.T) {
		h := newUsageBookingHarness(t, []auth.Permission{
			{Action: auth.ActionView, Resource: auth.ResourceAPIKeys},
		})

		resp, err := h.handler.HandleRequest(context.Background(),
			h.request("GET", "/api/info/deployment"))

		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
		h.requireExactlyOneBooking(t)
	})
}
