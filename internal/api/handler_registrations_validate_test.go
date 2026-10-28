package api

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateRegistrationRequest_AccountNameSanitized is the defense-in-depth
// regression test for #544 / #401: account_name from the unauthenticated
// POST /api/register endpoint must be CR/LF-stripped and length-capped at the
// data source before it is persisted or interpolated into any email header.
func TestValidateRegistrationRequest_AccountNameSanitized(t *testing.T) {
	t.Run("strips CRLF", func(t *testing.T) {
		req := RegistrationRequest{
			Provider:     "aws",
			ExternalID:   "ext-123",
			AccountName:  "Acme\r\nBcc: attacker@evil.example.com",
			ContactEmail: "user@example.com",
		}
		require.NoError(t, validateRegistrationRequest(&req))
		assert.NotContains(t, req.AccountName, "\r")
		assert.NotContains(t, req.AccountName, "\n")
	})

	t.Run("length-capped (rune-safe)", func(t *testing.T) {
		// Use a multibyte rune so the cap is exercised by rune count, not byte
		// length, and confirm truncation never splits a rune (valid UTF-8).
		req := RegistrationRequest{
			Provider:     "aws",
			ExternalID:   "ext-123",
			AccountName:  strings.Repeat("é", maxAccountNameLen+50),
			ContactEmail: "user@example.com",
		}
		require.NoError(t, validateRegistrationRequest(&req))
		assert.LessOrEqual(t, utf8.RuneCountInString(req.AccountName), maxAccountNameLen)
		assert.True(t, utf8.ValidString(req.AccountName), "cap must not split a multibyte rune")
	})

	t.Run("CRLF-only name is rejected as empty", func(t *testing.T) {
		req := RegistrationRequest{
			Provider:     "aws",
			ExternalID:   "ext-123",
			AccountName:  "\r\n",
			ContactEmail: "user@example.com",
		}
		err := validateRegistrationRequest(&req)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "account_name is required")
	})

	t.Run("whitespace-only name is rejected as empty", func(t *testing.T) {
		req := RegistrationRequest{
			Provider:     "aws",
			ExternalID:   "ext-123",
			AccountName:  "   \t  ",
			ContactEmail: "user@example.com",
		}
		err := validateRegistrationRequest(&req)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "account_name is required")
	})
}

// TestValidateRegistrationRequest_ContactEmail pins issue #311: the public
// registration endpoint used to accept any 5+ character string containing
// "@" as contact_email, so a value carrying a line break and a Bcc header,
// a second address, or a host with no TLD was stored as-is. It now applies
// validateEmailFormat and stores the bare address.
func TestValidateRegistrationRequest_ContactEmail(t *testing.T) {
	valid := map[string]string{
		"plain address":     "user@example.com",
		"display-name form": "Jane Doe <jane@example.com>",
		"surrounding space": "  user@example.com ",
		"trailing comment":  "user@example.com (ops)",
	}
	wantStored := map[string]string{
		"plain address":     "user@example.com",
		"display-name form": "jane@example.com",
		"surrounding space": "user@example.com",
		"trailing comment":  "user@example.com",
	}
	for name, raw := range valid {
		t.Run("accepts "+name, func(t *testing.T) {
			req := RegistrationRequest{Provider: "aws", ExternalID: "ext-123", AccountName: "Acme", ContactEmail: raw}
			require.NoError(t, validateRegistrationRequest(&req))
			assert.Equal(t, wantStored[name], req.ContactEmail, "the bare address must be stored")
		})
	}

	invalid := map[string]string{
		"CRLF header injection": "a@b\r\nBcc: x@y",
		"CRLF after valid addr": "user@example.com\r\nBcc: attacker@evil.example.com",
		"two addresses":         "user@example.com, other@example.com",
		"no TLD":                "user@hostonly",
		"no local part":         "@example.com",
		"no at sign":            "user.example.com",
		"quoted local part":     `"a b"@example.com`,
		"display name only":     "Jane Doe",
	}
	for name, raw := range invalid {
		t.Run("rejects "+name, func(t *testing.T) {
			req := RegistrationRequest{Provider: "aws", ExternalID: "ext-123", AccountName: "Acme", ContactEmail: raw}
			err := validateRegistrationRequest(&req)
			ce, ok := IsClientError(err)
			require.True(t, ok, "want a 400 client error, got %v", err)
			assert.Equal(t, 400, ce.code)
			assert.Equal(t, "contact_email must be a valid email address", ce.message)
		})
	}
}
