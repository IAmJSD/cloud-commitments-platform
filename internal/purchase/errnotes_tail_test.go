package purchase

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

const fakeConnectError = "failed to connect to `host=db.internal.example user=cudly_app database=cudly`: " +
	"dial error (dial tcp 10.0.0.5:5432: connect: connection refused)"

var connectNeedles = []string{"db.internal.example", "cudly_app", "10.0.0.5", "dial", "connect"}

func assertNoConnectDetail(t *testing.T, where, s string) {
	t.Helper()
	for _, n := range connectNeedles {
		assert.NotContains(t, s, n, where)
	}
}

// A note whose tail is not a closed format shows only its fixed head.
func TestSanitizeStoredError_TailedNotesShowOnlyTheirHead(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ramp not advanced: " + fakeConnectError, "ramp not advanced"},
		{"ramp not advanced: " + fakeConnectError + "\nsecond line", "ramp not advanced"},
		{"refusing to execute: ladder tranche lookup failed: " + fakeConnectError, "refusing to execute: ladder tranche lookup failed"},
		{"refusing to execute ladder purchase: global config unreadable: " + fakeConnectError, "refusing to execute ladder purchase: global config unreadable"},
		{ErrStaleAzurePricing.Error() + ": cannot read the latest collection state to confirm Azure prices are current: " + fakeConnectError, ErrStaleAzurePricing.Error()},
		{ErrStaleAzurePricing.Error() + ": Azure recommendation \"r-1\" no longer has the approved price", ErrStaleAzurePricing.Error()},
		{ErrStepCommitmentCap.Error() + ": recommendation \"r-1\" is not priced in USD, so it cannot be compared against the USD cap", ErrStepCommitmentCap.Error()},
		{ErrClaimedRowRefused.Error() + ": " + fakeConnectError, ErrClaimedRowRefused.Error()},
		{"m5.large: ramp not advanced: " + fakeConnectError + "; recovered after reap; provider purchase succeeded", "m5.large: ramp not advanced; recovered after reap; provider purchase succeeded"},
		{partialRunPrefix + "ramp not advanced: " + fakeConnectError, partialRunPrefix + "ramp not advanced"},
	}
	for _, c := range cases {
		out := SanitizeStoredError(c.in)
		assertNoConnectDetail(t, c.in, out)
		assert.Equal(t, c.want, out, c.in)
	}
}

func TestSanitizeStoredError_NoPatternMatchesANewline(t *testing.T) {
	for _, in := range []string{
		"purchase refused after claim: the claimed execution no longer passes the authorization checks\narn:aws:iam::123456789012:role/x",
		"refusing to execute ladder purchase: its tranche is paused\narn:aws:iam::123456789012:role/x",
		"selected recommendations do not resolve to a single cloud account: 1 selected recommendation(s) carry no cloud_account_id while 2 target account a\nb",
		"no current recommendation matched plan p step 2 by 2026-01-02\n(3-day grace after the plan date)",
	} {
		assert.Equal(t, ErrCategoryMessage(ErrCategoryUnknown), SanitizeStoredError(in), in)
	}
}

// A sentinel yields only its own text, never the wrapped chain.
func TestServerNoteMessage_SentinelReturnsOnlyItsOwnText(t *testing.T) {
	for _, sentinel := range []error{ErrPlanStepNoRecommendations, ErrStaleAzurePricing, ErrStepCommitmentCap,
		ErrDueRowTooOld, ErrClaimedRowRefused, errAmbiguousAccountScope} {
		for _, err := range []error{
			fmt.Errorf("%w: %w", sentinel, errors.New(fakeConnectError)),
			fmt.Errorf("%w: cannot read the latest collection state to confirm Azure prices are current: %w", sentinel, errors.New(fakeConnectError)),
			fmt.Errorf("execution e-1: %w: %s", sentinel, fakeConnectError),
		} {
			msg, ok := ServerNoteMessage(err)
			assert.True(t, ok)
			assert.Equal(t, sentinel.Error(), msg)
			assertNoConnectDetail(t, sentinel.Error(), msg)
		}
	}
	msg, ok := ServerNoteMessage(errors.New("ramp not advanced: " + fakeConnectError))
	assert.True(t, ok)
	assert.Equal(t, "ramp not advanced", msg)
}
