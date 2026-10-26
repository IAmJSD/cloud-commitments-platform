package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// credentialTypeProviders maps each credential_type to the provider whose
// accounts use it.
var credentialTypeProviders = map[string]string{
	"aws_access_keys":              "aws",
	"azure_client_secret":          "azure",
	"gcp_service_account":          "gcp",
	"gcp_workload_identity_config": "gcp",
}

// parseRegistrationCredential checks the optional credential_type and
// credential_payload of a registration for provider. With both empty there is
// no credential and it returns (nil, nil). Otherwise both are required, the
// type must be one POST /api/accounts/:id/credentials accepts and must belong
// to provider, and the payload must be a JSON object that passes
// validateCredentialPayload, the same shape check that endpoint runs. It
// returns the payload re-encoded from the checked object, so the bytes that
// are stored are exactly the ones that were checked.
func parseRegistrationCredential(provider, credType, payload string) ([]byte, error) {
	if credType == "" && payload == "" {
		return nil, nil
	}
	if err := checkRegistrationCredentialType(provider, credType, payload); err != nil {
		return nil, err
	}
	var fields map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &fields); err != nil || fields == nil {
		return nil, NewClientError(400, "credential_payload must be a JSON object")
	}
	if err := validateCredentialPayload(credType, fields); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(fields)
	if err != nil {
		return nil, NewClientError(400, "credential_payload must be a JSON object")
	}
	return canonical, nil
}

// checkRegistrationCredentialType requires credential_type and
// credential_payload together, and a credential_type that is known and
// belongs to provider.
func checkRegistrationCredentialType(provider, credType, payload string) error {
	if credType == "" || payload == "" {
		return NewClientError(400, "credential_type and credential_payload must be provided together")
	}
	if !validCredentialTypes[credType] {
		return NewClientError(400, "credential_type must be one of: aws_access_keys, azure_client_secret, gcp_service_account, gcp_workload_identity_config")
	}
	if credentialTypeProviders[credType] != provider {
		return NewClientError(400, fmt.Sprintf("credential_type %q cannot be used with provider %q", credType, provider))
	}
	return nil
}

// registrationCredential decrypts the credential stored with a pending
// registration and checks it again with parseRegistrationCredential against
// the provider of the account being approved. It runs before the
// registration leaves "pending", so a credential that fails the check stops
// the approval with a 400 the admin sees, and nothing is created or saved.
// Returns (nil, nil) when the registration carries no credential.
func (h *Handler) registrationCredential(reg *config.AccountRegistration, provider string) ([]byte, error) {
	if reg.RegCredentialPayload == "" {
		return nil, nil
	}
	if h.credStore == nil {
		return nil, fmt.Errorf("registrations: credential store not configured")
	}
	plaintext, err := h.credStore.DecryptPayload(reg.RegCredentialPayload)
	if err != nil {
		return nil, fmt.Errorf("registrations: decrypt credential: %w", err)
	}
	canonical, err := parseRegistrationCredential(provider, reg.RegCredentialType, string(plaintext))
	if err != nil {
		return nil, NewClientError(400, "the credentials submitted with this registration are not valid ("+err.Error()+
			"); reject the registration and ask the registrant to submit it again")
	}
	return canonical, nil
}

// storeRegistrationCredentials saves a registration credential that
// registrationCredential has checked into the account_credentials table.
// The account already exists at this point, so a store error is logged, not
// returned.
func (h *Handler) storeRegistrationCredentials(ctx context.Context, reg *config.AccountRegistration, accountID string, payload []byte) {
	if payload == nil || h.credStore == nil {
		return
	}
	if err := h.credStore.SaveCredential(ctx, accountID, reg.RegCredentialType, payload); err != nil {
		logging.Warnf("registration %s: failed to store credential: %v", reg.ID, err)
	}
}
