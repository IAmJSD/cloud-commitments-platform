package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// requestBodyLimit is the cap the HTTP transport applies to request bodies:
// the API router's own limit, so the two cannot disagree.
const requestBodyLimit = api.MaxRequestBodySize

// failingBody yields some bytes and then a read error, like a client that
// disconnects mid-upload.
type failingBody struct {
	sent bool
}

func (b *failingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `{"partial":`), nil
	}
	return 0, errors.New("connection reset by peer")
}

func (b *failingBody) Close() error { return nil }

// bodyHandlers lists every HTTP handler that converts the request through
// httpToLambdaRequest, so each one is checked for the same body handling.
func bodyHandlers(app *Application) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"handleHTTPRequest": app.handleHTTPRequest,
		"handleVersion":     app.handleVersion,
		"handleOIDCHTTP":    app.handleOIDCHTTP,
	}
}

// An oversize body must be rejected with 413, not truncated at the cap and
// handed to the API router cut mid-JSON (#326, A06-024).
func TestHTTPHandlers_RejectOversizeBodyWith413(t *testing.T) {
	app := &Application{API: api.NewHandler(api.HandlerConfig{})}
	for name, h := range bodyHandlers(app) {
		t.Run(name, func(t *testing.T) {
			body := bytes.Repeat([]byte("a"), requestBodyLimit+1)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/recommendations", bytes.NewReader(body))
			rec := httptest.NewRecorder()

			h(rec, req)

			testutil.AssertEqual(t, http.StatusRequestEntityTooLarge, rec.Code)
			testutil.AssertEqual(t, "application/json", rec.Header().Get("Content-Type"))
			testutil.AssertTrue(t, strings.Contains(rec.Body.String(), `"error":"Request body too large"`),
				"expected the router's JSON 413 body, got: "+rec.Body.String())
		})
	}
}

// A body read error must be answered with 400, not read as an empty body and
// routed (#326, A06-024).
func TestHTTPHandlers_RejectBodyReadErrorWith400(t *testing.T) {
	app := &Application{API: api.NewHandler(api.HandlerConfig{})}
	for name, h := range bodyHandlers(app) {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/recommendations", nil)
			req.Body = &failingBody{}
			rec := httptest.NewRecorder()

			h(rec, req)

			testutil.AssertEqual(t, http.StatusBadRequest, rec.Code)
			testutil.AssertTrue(t, strings.Contains(rec.Body.String(), "Failed to read request body"),
				"expected the body-read error message, got: "+rec.Body.String())
		})
	}
}

// A body of exactly the cap is still accepted whole.
func TestHttpToLambdaRequest_BodyAtLimitIsKeptWhole(t *testing.T) {
	body := bytes.Repeat([]byte("a"), requestBodyLimit)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/test", bytes.NewReader(body))

	lambdaReq, err := httpToLambdaRequest(httptest.NewRecorder(), req)

	testutil.AssertNoError(t, err)
	testutil.AssertEqual(t, requestBodyLimit, len(lambdaReq.Body))
}

// One byte over the cap is an error carrying *http.MaxBytesError, not a body
// silently truncated to the cap.
func TestHttpToLambdaRequest_OversizeBodyIsAnError(t *testing.T) {
	body := bytes.Repeat([]byte("a"), requestBodyLimit+1)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/test", bytes.NewReader(body))

	lambdaReq, err := httpToLambdaRequest(httptest.NewRecorder(), req)

	testutil.AssertError(t, err)
	var tooLarge *http.MaxBytesError
	testutil.AssertTrue(t, errors.As(err, &tooLarge), "expected *http.MaxBytesError, got: "+err.Error())
	testutil.AssertTrue(t, lambdaReq == nil, "expected no request on an oversize body")
}

// A read error is returned, not swallowed into an empty body.
func TestHttpToLambdaRequest_ReadErrorIsReturned(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/test", nil)
	req.Body = &failingBody{}

	lambdaReq, err := httpToLambdaRequest(httptest.NewRecorder(), req)

	testutil.AssertError(t, err)
	testutil.AssertTrue(t, !errors.Is(err, io.EOF), "a read error must not look like a clean EOF")
	testutil.AssertTrue(t, lambdaReq == nil, "expected no request on a read error")
}
