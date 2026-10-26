package providerfactory

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	azureprovider "github.com/LeanerCloud/cloud-commitments-go/providers/azure"
	gcpprovider "github.com/LeanerCloud/cloud-commitments-go/providers/gcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

const (
	testSubscriptionID = "00000000-0000-0000-0000-000000000001"
	testProjectID      = "example-project"
)

// fakeAzureCred is an azcore.TokenCredential that never talks to Azure.
type fakeAzureCred struct{ id string }

func (f *fakeAzureCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errors.New("fakeAzureCred: no network in tests")
}

// fakeTokenSource is an oauth2.TokenSource that never talks to GCP.
type fakeTokenSource struct{ id string }

func (f *fakeTokenSource) Token() (*oauth2.Token, error) {
	return nil, errors.New("fakeTokenSource: no network in tests")
}

// recordingRegistry returns a private registry whose name factory records
// the config it receives and returns want.
func recordingRegistry(t *testing.T, name string, want provider.Provider, wantErr error) (*provider.Registry, *[]*provider.ProviderConfig) {
	t.Helper()
	reg := provider.NewRegistry()
	var got []*provider.ProviderConfig
	require.NoError(t, reg.Register(name, func(cfg *provider.ProviderConfig) (provider.Provider, error) {
		got = append(got, cfg)
		return want, wantErr
	}))
	return reg, &got
}

// stubProvider is a minimal provider.Provider for the fake-registry tests.
type stubProvider struct{ provider.Provider }

func TestNewAzure_PassesTypedConfigToRegistry(t *testing.T) {
	cred := &fakeAzureCred{id: "a"}
	want := &stubProvider{}
	reg, got := recordingRegistry(t, "azure", want, nil)

	prov, err := newAzure(reg, testSubscriptionID, cred)

	require.NoError(t, err)
	assert.Same(t, want, prov, "must return the provider the registry built")
	require.Len(t, *got, 1, "the azure factory must be called exactly once")
	cfg := (*got)[0]
	assert.Equal(t, "azure", cfg.Name)
	assert.Equal(t, testSubscriptionID, cfg.AzureSubscriptionID)
	assert.Same(t, cred, cfg.AzureTokenCredential, "the resolved credential must be passed through unchanged")
	assert.Empty(t, cfg.Profile, "the deprecated Profile field must not be used") //nolint:staticcheck // SA1019: asserting the deprecated field stays unset
	assert.Nil(t, cfg.GCPTokenSource)
	assert.Nil(t, cfg.ProviderOverride)
}

func TestNewAzure_NilCredentialLeavesSlotNil(t *testing.T) {
	reg, got := recordingRegistry(t, "azure", &stubProvider{}, nil)

	_, err := newAzure(reg, testSubscriptionID, nil)

	require.NoError(t, err)
	require.Len(t, *got, 1)
	assert.Nil(t, (*got)[0].AzureTokenCredential, "a nil credential must reach the factory as a nil slot (ambient fallback)")
}

func TestNewGCP_PassesTypedConfigToRegistry(t *testing.T) {
	ts := &fakeTokenSource{id: "g"}
	want := &stubProvider{}
	reg, got := recordingRegistry(t, "gcp", want, nil)

	prov, err := newGCP(reg, testProjectID, ts)

	require.NoError(t, err)
	assert.Same(t, want, prov)
	require.Len(t, *got, 1, "the gcp factory must be called exactly once")
	cfg := (*got)[0]
	assert.Equal(t, "gcp", cfg.Name)
	assert.Equal(t, testProjectID, cfg.GCPProjectID)
	assert.Same(t, ts, cfg.GCPTokenSource, "the resolved token source must be passed through unchanged")
	assert.Empty(t, cfg.Profile, "the deprecated Profile field must not be used") //nolint:staticcheck // SA1019: asserting the deprecated field stays unset
	assert.Nil(t, cfg.AzureTokenCredential)
	assert.Nil(t, cfg.ProviderOverride)
}

// The registry's GCP factory resolves an empty project ID by asking the
// cloud for the credential's default project. The direct constructor never
// did that, so NewGCP must refuse before the factory runs.
func TestNewGCP_RejectsMissingInputsBeforeRegistry(t *testing.T) {
	cases := []struct {
		name      string
		projectID string
		ts        oauth2.TokenSource
		wantErr   error
	}{
		{"empty project", "", &fakeTokenSource{}, ErrMissingGCPProjectID},
		{"nil token source", testProjectID, nil, ErrMissingGCPTokenSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, got := recordingRegistry(t, "gcp", &stubProvider{}, nil)

			prov, err := newGCP(reg, tc.projectID, tc.ts)

			require.ErrorIs(t, err, tc.wantErr)
			assert.Nil(t, prov)
			assert.Empty(t, *got, "the factory must not be called")
		})
	}
}

func TestBuild_PropagatesFactoryErrorsAndNilProviders(t *testing.T) {
	boom := errors.New("boom")
	reg, _ := recordingRegistry(t, "azure", nil, boom)
	_, err := newAzure(reg, testSubscriptionID, &fakeAzureCred{})
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "build azure provider")

	reg, _ = recordingRegistry(t, "gcp", nil, nil)
	_, err = newGCP(reg, testProjectID, &fakeTokenSource{})
	require.Error(t, err, "a nil provider with a nil error must not be returned as success")
	assert.Contains(t, err.Error(), "gcp factory returned no provider")

	_, err = newAzure(provider.NewRegistry(), testSubscriptionID, nil)
	require.Error(t, err, "an unregistered provider must fail")
}

// The real registered Azure factory must produce a provider identical to the
// legacy construction it replaces in purchase/execution.go and
// scheduler/scheduler.go: NewAzureProvider(&ProviderConfig{Profile: sub})
// followed by SetCredential(cred).
func TestNewAzure_MatchesLegacyConstruction(t *testing.T) {
	cred := &fakeAzureCred{id: "legacy"}

	legacy, err := azureprovider.NewAzureProvider(&provider.ProviderConfig{Profile: testSubscriptionID}) //nolint:staticcheck // SA1019: this is the legacy baseline being compared against
	require.NoError(t, err)
	legacy.SetCredential(cred)

	built, err := NewAzure(testSubscriptionID, cred)
	require.NoError(t, err)
	got, ok := built.(*azureprovider.Provider)
	require.True(t, ok, "registry must build the real Azure provider, got %T", built)

	// SetCredential also invalidates the (still empty) accounts cache, which
	// bumps an internal generation counter. Do the same on the registry-built
	// provider so the comparison covers every other field.
	got.InvalidateAccountsCache()
	assert.Equal(t, legacy, got,
		"registry-built Azure provider must match the legacy construction (same subscription, credential and region)")

	// Guard against a vacuous comparison: a different credential or
	// subscription must make the providers differ.
	other, err := NewAzure(testSubscriptionID, &fakeAzureCred{id: "other"})
	require.NoError(t, err)
	other.(*azureprovider.Provider).InvalidateAccountsCache()
	assert.NotEqual(t, legacy, other, "a different credential must be detected")

	otherSub, err := NewAzure("00000000-0000-0000-0000-000000000002", cred)
	require.NoError(t, err)
	otherSub.(*azureprovider.Provider).InvalidateAccountsCache()
	assert.NotEqual(t, legacy, otherSub, "a different subscription must be detected")
}

// The real registered GCP factory must produce a provider identical to the
// legacy gcpprovider.NewProviderWithCredentials(ctx, project, ts) it
// replaces. The legacy constructor stored the caller's ctx, which the
// provider never reads; the baseline uses context.Background(), the value
// the registry factory stores, so the comparison covers every field.
func TestNewGCP_MatchesLegacyConstruction(t *testing.T) {
	ts := &fakeTokenSource{id: "legacy"}

	legacy := gcpprovider.NewProviderWithCredentials(context.Background(), testProjectID, ts)

	built, err := NewGCP(testProjectID, ts)
	require.NoError(t, err)
	got, ok := built.(*gcpprovider.Provider)
	require.True(t, ok, "registry must build the real GCP provider, got %T", built)
	assert.Equal(t, legacy, got,
		"registry-built GCP provider must match the legacy construction (same project and token source)")

	other, err := NewGCP(testProjectID, &fakeTokenSource{id: "other"})
	require.NoError(t, err)
	assert.NotEqual(t, legacy, other, "a different token source must be detected")

	otherProject, err := NewGCP("other-project", ts)
	require.NoError(t, err)
	assert.NotEqual(t, legacy, otherProject, "a different project must be detected")
}
