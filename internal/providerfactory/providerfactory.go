// Package providerfactory builds credentialed cloud providers through the
// cloud-commitments-go provider registry, so the internal layer no longer
// calls the concrete Azure/GCP constructors directly (platform#41).
//
// Credentials go in the registry's typed slots (AzureTokenCredential,
// GCPTokenSource) and the account identity in the typed
// AzureSubscriptionID / GCPProjectID fields, never the deprecated Profile
// field.
package providerfactory

import (
	"errors"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"golang.org/x/oauth2"

	// The provider packages register their factories with the global
	// registry in init(); importing them here guarantees "azure" and "gcp"
	// are registered in every binary that uses this package.
	_ "github.com/LeanerCloud/cloud-commitments-go/providers/azure"
	_ "github.com/LeanerCloud/cloud-commitments-go/providers/gcp"
)

// Registry names of the providers built here.
const (
	azureName = "azure"
	gcpName   = "gcp"
)

// ErrMissingGCPProjectID is returned by NewGCP when the project ID is empty.
var ErrMissingGCPProjectID = errors.New("providerfactory: GCP project ID is required")

// ErrMissingGCPTokenSource is returned by NewGCP when the token source is nil.
var ErrMissingGCPTokenSource = errors.New("providerfactory: GCP token source is required")

// NewAzure returns an Azure provider pinned to subscriptionID that
// authenticates with cred, built through the global provider registry.
//
// A nil cred leaves the provider on its ambient-credential fallback, the
// same as installing a nil credential on a directly constructed provider.
func NewAzure(subscriptionID string, cred azcore.TokenCredential) (provider.Provider, error) {
	return newAzure(provider.GetRegistry(), subscriptionID, cred)
}

// NewGCP returns a GCP provider for projectID that authenticates with ts,
// built through the global provider registry.
//
// projectID must be non-empty: the registry's GCP factory resolves an empty
// project ID by asking the cloud for the credential's default project, which
// the direct constructor never did and which could pin the provider to a
// project other than the account's. ts must be non-nil; callers in ADC mode
// use the ambient factory path instead.
func NewGCP(projectID string, ts oauth2.TokenSource) (provider.Provider, error) {
	return newGCP(provider.GetRegistry(), projectID, ts)
}

func newAzure(reg *provider.Registry, subscriptionID string, cred azcore.TokenCredential) (provider.Provider, error) {
	// A nil cred converts to a nil `any`, which the Azure factory treats as
	// "no credential supplied".
	return build(reg, azureName, &provider.ProviderConfig{
		Name:                 azureName,
		AzureSubscriptionID:  subscriptionID,
		AzureTokenCredential: cred,
	})
}

func newGCP(reg *provider.Registry, projectID string, ts oauth2.TokenSource) (provider.Provider, error) {
	if projectID == "" {
		return nil, ErrMissingGCPProjectID
	}
	if ts == nil {
		return nil, ErrMissingGCPTokenSource
	}
	return build(reg, gcpName, &provider.ProviderConfig{
		Name:           gcpName,
		GCPProjectID:   projectID,
		GCPTokenSource: ts,
	})
}

func build(reg *provider.Registry, name string, cfg *provider.ProviderConfig) (provider.Provider, error) {
	prov, err := reg.GetProviderWithConfig(name, cfg)
	if err != nil {
		return nil, fmt.Errorf("providerfactory: build %s provider: %w", name, err)
	}
	if prov == nil {
		return nil, fmt.Errorf("providerfactory: %s factory returned no provider", name)
	}
	return prov, nil
}
