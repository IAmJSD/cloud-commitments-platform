package api //nolint:revive // var-naming: package name "api" is intentional for handler package

// Issue #980: POST /api/api-keys must reject an explicit max_amount of 0 with
// a 400, as group writes do since #133. The handler used to decode into a
// local type whose max_amount was a plain float64 with omitempty, so the
// explicit 0 was erased before the auth layer could tell it from an absent
// field, and the key was minted with no cap. These tests drive the real
// handler into a real auth.Service (MockStore, no database).

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiKeyBodyWithConstraints builds a create body whose constraints object is
// the raw JSON given, so literals such as 0.0 and -0 reach the decoder as
// written (json.Marshal would normalize them).
func apiKeyBodyWithConstraints(constraints string) string {
	return `{"name":"capped key","password":"` +
		base64.StdEncoding.EncodeToString([]byte(roundtripOwnerPw)) +
		`","expires_at":"` + time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339) +
		`","permissions":[{"action":"view","resource":"recommendations","constraints":` +
		constraints + `}]}`
}

func TestHandleRequest_CreateAPIKey_RejectsExplicitZeroMaxAmount(t *testing.T) {
	for _, literal := range []string{`0`, `0.0`, `-0`, `0e5`} {
		t.Run(literal, func(t *testing.T) {
			h, _, stored := newAPIKeyRoundtripHandler(t)

			resp := apiKeyRoundtripRequest(t, h, "POST",
				apiKeyBodyWithConstraints(`{"regions":["us-east-1"],"max_amount":`+literal+`}`))

			assert.Equal(t, 400, resp.StatusCode, "body: %s", resp.Body)
			assert.Contains(t, resp.Body, "max_amount must be greater than 0")
			assert.Nil(t, *stored, "a key with an explicit zero cap must not be stored")
		})
	}
}

// Absent and null keep meaning "no cap" (stored as 0), and a positive cap is
// stored as sent, so the zero check does not over-reject.
func TestHandleRequest_CreateAPIKey_AcceptsAbsentNullAndPositiveMaxAmount(t *testing.T) {
	cases := map[string]float64{
		`{"regions":["us-east-1"]}`:                   0,
		`{"regions":["us-east-1"],"max_amount":null}`: 0,
		`{"regions":["us-east-1"],"max_amount":5000}`: 5000,
	}
	for constraints, want := range cases {
		t.Run(constraints, func(t *testing.T) {
			h, _, stored := newAPIKeyRoundtripHandler(t)

			resp := apiKeyRoundtripRequest(t, h, "POST", apiKeyBodyWithConstraints(constraints))

			require.Equal(t, 200, resp.StatusCode, "body: %s", resp.Body)
			require.NotNil(t, *stored)
			require.Len(t, (*stored).Permissions, 1)
			require.NotNil(t, (*stored).Permissions[0].Constraints)
			assert.Equal(t, want, (*stored).Permissions[0].Constraints.MaxPurchaseAmount)
			assert.Equal(t, []string{"us-east-1"}, (*stored).Permissions[0].Constraints.Regions)
		})
	}
}
