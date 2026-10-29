package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountControlChars are the control characters the account write paths
// must reject in single-line identifiers (#970). A tab is included: unlike
// the render guard, which lets one through for the session email, a name or
// external ID has no use for it.
var accountControlChars = map[string]string{
	"LF":   "\n",
	"CRLF": "\r\n",
	"CR":   "\r",
	"TAB":  "\t",
	"NUL":  "\x00",
	"ESC":  "\x1b",
	"DEL":  "\x7f",
	"NEL":  "\u0085",
}

// accountBody returns a create/update body for an Azure account (no AWS
// external-ID charset rule applies), with name and external_id as given.
func accountBody(t *testing.T, name, externalID string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"name": name, "provider": "azure", "external_id": externalID})
	require.NoError(t, err)
	return string(b)
}

func requireClientError400(t *testing.T, err error, field string) {
	t.Helper()
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok, "expected a client error, got %v", err)
	assert.Equal(t, 400, ce.code)
	assert.Contains(t, ce.message, field+" must not contain control characters")
}

// TestCreateAccount_RejectsControlChars drives POST /api/accounts with a
// control character in name or external_id: the request is refused with a
// 400 naming the field and nothing is stored.
func TestCreateAccount_RejectsControlChars(t *testing.T) {
	ctx := context.Background()
	for label, c := range accountControlChars {
		for _, field := range []string{"name", "external_id"} {
			t.Run(field+"/"+label, func(t *testing.T) {
				mockAuth := new(MockAuthService)
				t.Cleanup(func() { mockAuth.AssertExpectations(t) })
				setupAdminAuth(ctx, mockAuth)
				store := new(MockConfigStore)
				t.Cleanup(func() { store.AssertExpectations(t) })
				handler := &Handler{auth: mockAuth, config: store}

				name, ext := "Acme", "acme-prod"
				if field == "name" {
					name = "Acme" + c + "touch PWNED"
				} else {
					ext = "acme" + c + "touch PWNED"
				}
				result, err := handler.createAccount(ctx, adminRequest(accountBody(t, name, ext)))
				assert.Nil(t, result)
				requireClientError400(t, err, field)
				store.AssertNotCalled(t, "CreateCloudAccount")
			})
		}
	}
}

// TestUpdateAccount_RejectsControlChars is the PUT /api/accounts/:id
// counterpart: the update is refused before the account is loaded or saved.
func TestUpdateAccount_RejectsControlChars(t *testing.T) {
	ctx := context.Background()
	const id = "11111111-1111-1111-1111-111111111111"
	for label, c := range accountControlChars {
		for _, field := range []string{"name", "external_id"} {
			t.Run(field+"/"+label, func(t *testing.T) {
				mockAuth := new(MockAuthService)
				t.Cleanup(func() { mockAuth.AssertExpectations(t) })
				setupAdminAuth(ctx, mockAuth)
				store := new(MockConfigStore)
				t.Cleanup(func() { store.AssertExpectations(t) })
				handler := &Handler{auth: mockAuth, config: store}

				name, ext := "Acme", "acme-prod"
				if field == "name" {
					name = "Acme" + c + "touch PWNED"
				} else {
					ext = "acme" + c + "touch PWNED"
				}
				req := adminRequest(accountBody(t, name, ext))
				req.RequestContext.HTTP.Method = "PUT"
				req.RequestContext.HTTP.Path = "/api/accounts/" + id
				result, err := handler.updateAccount(ctx, req, id)
				assert.Nil(t, result)
				requireClientError400(t, err, field)
				store.AssertNotCalled(t, "UpdateCloudAccount")
			})
		}
	}
}

// TestCreateAccount_AcceptsPrintableNames guards against over-rejection:
// spaces, punctuation and non-ASCII letters stay valid and are stored as sent.
func TestCreateAccount_AcceptsPrintableNames(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"Acme Production", "Acme (EU) – Zürich #2", "日本 本番"} {
		mockAuth := new(MockAuthService)
		t.Cleanup(func() { mockAuth.AssertExpectations(t) })
		setupAdminAuth(ctx, mockAuth)
		handler := &Handler{auth: mockAuth, config: new(MockConfigStore)}
		result, err := handler.createAccount(ctx, adminRequest(accountBody(t, name, "acme-prod")))
		require.NoError(t, err, name)
		assert.Equal(t, name, result.(*config.CloudAccount).Name)
	}
}

// TestSubmitRegistration_RejectsControlChars drives the public POST
// /api/register handler. CR and LF in account_name are still stripped (#544),
// but any other control character in account_name, and any control character
// in external_id, is refused, since approval would reject it anyway.
func TestSubmitRegistration_RejectsControlChars(t *testing.T) {
	ctx := context.Background()
	submit := func(t *testing.T, name, ext string) error {
		t.Helper()
		store := new(MockConfigStore)
		t.Cleanup(func() { store.AssertExpectations(t) })
		h := &Handler{config: store}
		b, err := json.Marshal(map[string]string{
			"provider": "azure", "external_id": ext, "account_name": name, "contact_email": "ops@example.com",
		})
		require.NoError(t, err)
		_, err = h.submitRegistration(ctx, adminRequest(string(b)))
		if err != nil {
			store.AssertNotCalled(t, "CreateAccountRegistration")
		}
		return err
	}
	for label, c := range accountControlChars {
		t.Run("external_id/"+label, func(t *testing.T) {
			requireClientError400(t, submit(t, "Acme", "acme"+c+"touch PWNED"), "external_id")
		})
		if label == "LF" || label == "CRLF" || label == "CR" {
			continue // stripped from account_name, covered below
		}
		t.Run("account_name/"+label, func(t *testing.T) {
			requireClientError400(t, submit(t, "Acme"+c+"touch PWNED", "acme-prod"), "account_name")
		})
	}
	t.Run("account_name CR/LF still stripped", func(t *testing.T) {
		req := RegistrationRequest{
			Provider: "azure", ExternalID: "acme-prod", AccountName: "Acme\r\ntouch PWNED", ContactEmail: "ops@example.com",
		}
		require.NoError(t, validateRegistrationRequest(&req))
		assert.Equal(t, "Acmetouch PWNED", req.AccountName)
	})
}
