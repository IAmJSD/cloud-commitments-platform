package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/api"
)

// readRequestBody reads the whole request body, capped at the API router's
// own limit (api.MaxRequestBodySize) so the transport never buffers a body
// the router would reject anyway. http.MaxBytesReader makes an oversize body
// an error rather than truncating it, and a read error (a client that
// disconnects mid-upload) is returned rather than read as an empty body
// (#326).
func readRequestBody(w http.ResponseWriter, r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	defer r.Body.Close()
	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize))
	if err != nil {
		return "", err
	}
	return string(bodyBytes), nil
}

// writeBodyReadError answers a failed body read: 413 for a body over the cap,
// with the same JSON error the API router returns for one, and 400 for any
// other read failure.
func writeBodyReadError(w http.ResponseWriter, err error) {
	status, msg := http.StatusBadRequest, "Failed to read request body"
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status, msg = http.StatusRequestEntityTooLarge, "Request body too large"
	} else {
		log.Printf("Failed to read request body: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encErr := json.NewEncoder(w).Encode(map[string]string{"error": msg}); encErr != nil {
		log.Printf("Failed to encode body-read error response: %v", encErr)
	}
}
