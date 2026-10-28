package credentials

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// Hosts a GCP external_account (workload identity federation) config may name
// for its token exchange and service-account impersonation endpoints. These
// are the values gcloud writes into a generated credential config, and the
// same endpoints BuildGCPFederatedCredential uses.
const (
	gcpSTSHost            = "sts.googleapis.com"
	gcpIAMCredentialsHost = "iamcredentials.googleapis.com"
)

// gcpExternalAccountType is the "type" of a GCP external_account credential
// config.
const gcpExternalAccountType = "external_account"

// ValidateGCPWIFEndpoints checks the endpoint URLs of a GCP external_account
// credential config. token_url is required and must be an https URL whose
// host is exactly sts.googleapis.com. service_account_impersonation_url is
// optional; when set it must be an https URL whose host is exactly
// iamcredentials.googleapis.com. Neither URL may carry user info or an
// explicit port. The returned error names the field and the expected form
// only, never the submitted value.
func ValidateGCPWIFEndpoints(tokenURL, impersonationURL string) error {
	if tokenURL == "" {
		return fmt.Errorf("token_url is required")
	}
	if err := checkGCPEndpoint("token_url", tokenURL, gcpSTSHost); err != nil {
		return err
	}
	if impersonationURL == "" {
		return nil
	}
	return checkGCPEndpoint("service_account_impersonation_url", impersonationURL, gcpIAMCredentialsHost)
}

// checkGCPEndpoint requires raw to parse as an absolute https URL whose
// authority is exactly host: no user info, no port, no other host.
func checkGCPEndpoint(field, raw, host string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host != host {
		return fmt.Errorf("%s must be an https URL on %s with no user info or port", field, host)
	}
	return nil
}

// storedGCPExternalAccount holds the fields of a stored GCP credential JSON
// that ValidateGCPWIFEndpoints checks. The JSON tags match the ones
// golang.org/x/oauth2/google decodes the same file with, and decoding into a
// struct (not a map) matches keys the same way, so both read the same values.
type storedGCPExternalAccount struct {
	Type                           string `json:"type"`
	TokenURL                       string `json:"token_url"`
	ServiceAccountImpersonationURL string `json:"service_account_impersonation_url"`
}

// checkStoredGCPEndpoints checks a stored GCP credential JSON before a token
// source is built from it: when its type is external_account, its endpoints
// must pass ValidateGCPWIFEndpoints. Other types are left to the parser.
// Empty input passes so the caller can report the missing credential itself.
func checkStoredGCPEndpoints(accountID string, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var cfg storedGCPExternalAccount
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("credentials: parse gcp credentials for account %s: %w", accountID, err)
	}
	if cfg.Type != gcpExternalAccountType {
		return nil
	}
	if err := ValidateGCPWIFEndpoints(cfg.TokenURL, cfg.ServiceAccountImpersonationURL); err != nil {
		return fmt.Errorf("credentials: stored gcp workload identity config for account %s is not usable: %w; save the config again", accountID, err)
	}
	return nil
}
