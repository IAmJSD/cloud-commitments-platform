package api

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #151 (audit finding A02-013): PUT /api/accounts/{id} built the stored
// row from the request alone, so a body without an `enabled` or
// `aws_is_org_root` key silently disabled the account and cleared its
// org-root flag. An omitted (or null) key now keeps the stored value, while an
// explicit false still clears it. Create keeps its defaults (both false).

// accountWriteCapture records the account handed to the store so the tests
// can assert what would be persisted, not just what the handler returns.
type accountWriteCapture struct {
	*mockConfigStoreAccounts
	updated *config.CloudAccount
	created *config.CloudAccount
}

func (m *accountWriteCapture) UpdateCloudAccount(_ context.Context, a *config.CloudAccount) error {
	cp := *a
	m.updated = &cp
	return nil
}

func (m *accountWriteCapture) CreateCloudAccount(_ context.Context, a *config.CloudAccount) error {
	cp := *a
	m.created = &cp
	return nil
}

func newAccountWriteCapture(ctx context.Context, existing *config.CloudAccount) *accountWriteCapture {
	return &accountWriteCapture{
		mockConfigStoreAccounts: &mockConfigStoreAccounts{
			MockConfigStore: setupAdminMock(ctx),
			getResult:       existing,
		},
	}
}

// putAccountFlags sends PUT /api/accounts/{id} with the given trailing JSON
// members (empty for none) against an existing row with the given flags and
// returns the row the handler wrote to the store.
func putAccountFlags(t *testing.T, existingEnabled, existingOrgRoot bool, extra string) *config.CloudAccount {
	t.Helper()
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	setupAdminAuth(ctx, mockAuth)

	existing := sampleAccount()
	existing.Enabled = existingEnabled
	existing.AWSIsOrgRoot = existingOrgRoot
	store := newAccountWriteCapture(ctx, &existing)
	handler := &Handler{auth: mockAuth, config: store}

	body := `{"name":"Renamed","provider":"aws","external_id":"123456789012"` + extra + `}`
	result, err := handler.updateAccount(ctx, adminRequest(body), existing.ID)
	require.NoError(t, err)
	require.NotNil(t, store.updated, "updateAccount must write the account")

	returned := result.(*config.CloudAccount)
	assert.Equal(t, store.updated.Enabled, returned.Enabled, "response must match the stored enabled")
	assert.Equal(t, store.updated.AWSIsOrgRoot, returned.AWSIsOrgRoot, "response must match the stored aws_is_org_root")
	assert.Equal(t, "Renamed", store.updated.Name, "the rest of the body is still applied")
	return store.updated
}

func TestUpdateAccount_OmittedFlagsKeepStoredValues(t *testing.T) {
	cases := []struct {
		name            string
		extra           string
		existingEnabled bool
		existingOrgRoot bool
		wantEnabled     bool
		wantOrgRoot     bool
	}{
		{name: "omitted keeps true", existingEnabled: true, existingOrgRoot: true, wantEnabled: true, wantOrgRoot: true},
		{name: "omitted keeps false", existingEnabled: false, existingOrgRoot: false, wantEnabled: false, wantOrgRoot: false},
		{name: "null keeps true", extra: `,"enabled":null,"aws_is_org_root":null`, existingEnabled: true, existingOrgRoot: true, wantEnabled: true, wantOrgRoot: true},
		{name: "explicit false clears", extra: `,"enabled":false,"aws_is_org_root":false`, existingEnabled: true, existingOrgRoot: true, wantEnabled: false, wantOrgRoot: false},
		{name: "explicit true sets", extra: `,"enabled":true,"aws_is_org_root":true`, existingEnabled: false, existingOrgRoot: false, wantEnabled: true, wantOrgRoot: true},
		{name: "enabled omitted, org root explicit false", extra: `,"aws_is_org_root":false`, existingEnabled: true, existingOrgRoot: true, wantEnabled: true, wantOrgRoot: false},
		{name: "enabled explicit false, org root omitted", extra: `,"enabled":false`, existingEnabled: true, existingOrgRoot: true, wantEnabled: false, wantOrgRoot: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := putAccountFlags(t, tc.existingEnabled, tc.existingOrgRoot, tc.extra)
			assert.Equal(t, tc.wantEnabled, stored.Enabled, "enabled")
			assert.Equal(t, tc.wantOrgRoot, stored.AWSIsOrgRoot, "aws_is_org_root")
		})
	}
}

// The issue's verification rule: a real false and a genuine absence go through
// the same path and must produce different outcomes.
func TestUpdateAccount_ExplicitFalseAndOmittedDiverge(t *testing.T) {
	omitted := putAccountFlags(t, true, true, "")
	explicit := putAccountFlags(t, true, true, `,"enabled":false,"aws_is_org_root":false`)
	assert.NotEqual(t, omitted.Enabled, explicit.Enabled, "enabled: omitted and explicit false must differ")
	assert.NotEqual(t, omitted.AWSIsOrgRoot, explicit.AWSIsOrgRoot, "aws_is_org_root: omitted and explicit false must differ")
}

func TestCreateAccount_FlagDefaultsUnchanged(t *testing.T) {
	cases := []struct {
		name        string
		extra       string
		wantEnabled bool
		wantOrgRoot bool
	}{
		{name: "omitted defaults to false"},
		{name: "null defaults to false", extra: `,"enabled":null,"aws_is_org_root":null`},
		{name: "explicit true", extra: `,"enabled":true,"aws_is_org_root":true`, wantEnabled: true, wantOrgRoot: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mockAuth := new(MockAuthService)
			setupAdminAuth(ctx, mockAuth)
			store := newAccountWriteCapture(ctx, nil)
			handler := &Handler{auth: mockAuth, config: store}

			body := `{"name":"Acme","provider":"aws","external_id":"123456789012"` + tc.extra + `}`
			_, err := handler.createAccount(ctx, adminRequest(body))
			require.NoError(t, err)
			require.NotNil(t, store.created)
			assert.Equal(t, tc.wantEnabled, store.created.Enabled, "enabled")
			assert.Equal(t, tc.wantOrgRoot, store.created.AWSIsOrgRoot, "aws_is_org_root")
		})
	}
}
