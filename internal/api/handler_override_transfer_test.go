package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// overrideTransferStore is an in-memory account and override store for the
// export/import tests (issue #9).
type overrideTransferStore struct {
	*MockConfigStore
	accounts  []config.CloudAccount
	overrides map[string][]config.AccountServiceOverride
	saved     []config.AccountServiceOverride
}

func (s *overrideTransferStore) ListCloudAccounts(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
	return append([]config.CloudAccount(nil), s.accounts...), nil
}

func (s *overrideTransferStore) ListAccountServiceOverrides(_ context.Context, accountID string) ([]config.AccountServiceOverride, error) {
	return append([]config.AccountServiceOverride(nil), s.overrides[accountID]...), nil
}

func (s *overrideTransferStore) SaveAccountServiceOverride(_ context.Context, o *config.AccountServiceOverride) error {
	s.saved = append(s.saved, *o)
	list := s.overrides[o.AccountID]
	for i := range list {
		if list[i].Provider == o.Provider && list[i].Service == o.Service {
			list[i] = *o
			return nil
		}
	}
	s.overrides[o.AccountID] = append(list, *o)
	return nil
}

const (
	xferAWS   = "11111111-1111-1111-1111-111111111111"
	xferAzure = "22222222-2222-2222-2222-222222222222"
	xferOther = "33333333-3333-3333-3333-333333333333"
)

func ptr[T any](v T) *T { return &v }

func newOverrideTransferStore() *overrideTransferStore {
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return &overrideTransferStore{
		MockConfigStore: new(MockConfigStore),
		accounts: []config.CloudAccount{
			{ID: xferAzure, Name: "Azure prod", Provider: "azure", ExternalID: "sub-1"},
			{ID: xferAWS, Name: "=HYPERLINK(\"x\")", Provider: "aws", ExternalID: "123456789012"},
			{ID: xferOther, Name: "Other team", Provider: "aws", ExternalID: "210987654321"},
		},
		overrides: map[string][]config.AccountServiceOverride{
			xferAWS: {
				{ID: "o-2", AccountID: xferAWS, Provider: "aws", Service: "rds", Coverage: ptr(50.0), IncludeRegions: []string{"us-east-1", "eu-west-1"}, CreatedAt: created, UpdatedAt: created},
				{ID: "o-1", AccountID: xferAWS, Provider: "aws", Service: "ec2", Enabled: ptr(true), Term: ptr(3), Payment: ptr("all-upfront"), CreatedAt: created, UpdatedAt: created},
			},
			xferAzure: {{ID: "o-3", AccountID: xferAzure, Provider: "azure", Service: "vm", Enabled: ptr(false), CreatedAt: created, UpdatedAt: created}},
			xferOther: {{ID: "o-4", AccountID: xferOther, Provider: "aws", Service: "ec2", Term: ptr(1), CreatedAt: created, UpdatedAt: created}},
		},
	}
}

// transferHandler returns a handler whose session may see only scope (all
// accounts when scope is nil).
func transferHandler(t *testing.T, store *overrideTransferStore, scope []string) *Handler {
	t.Helper()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", context.Background(), "admin-token").Return(adminAccountSession(), nil)
	mockAuth.grantPermissionsScoped([]auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}, scope)
	return &Handler{auth: mockAuth, config: store}
}

func exportRequest(format string) *events.LambdaFunctionURLRequest {
	req := adminRequest("")
	if format != "" {
		req.QueryStringParameters = map[string]string{"format": format}
	}
	return req
}

func TestExportOverrides_JSONListsScopedAccountsInOrder(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, []string{xferAWS, xferAzure})

	got, err := h.exportAccountServiceOverrides(context.Background(), exportRequest(""))
	require.NoError(t, err)
	doc := got.(OverrideExport)
	assert.Equal(t, overrideExportVersion, doc.Version)

	keys := make([]string, 0, len(doc.Overrides))
	for _, r := range doc.Overrides {
		keys = append(keys, r.Provider+":"+r.AccountExternalID+"/"+r.Service)
	}
	// Sorted by provider, external ID, then service; the out-of-scope
	// account's override is absent.
	assert.Equal(t, []string{"aws:123456789012/ec2", "aws:123456789012/rds", "azure:sub-1/vm"}, keys)
	assert.Equal(t, xferAWS, doc.Overrides[0].AccountID)
	assert.Equal(t, 3, *doc.Overrides[0].Term)
	assert.Equal(t, []string{"us-east-1", "eu-west-1"}, doc.Overrides[1].IncludeRegions)
}

func TestExportOverrides_CSVRoundTripsAndNeutralizesFormulas(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, nil)

	got, err := h.exportAccountServiceOverrides(context.Background(), exportRequest("csv"))
	require.NoError(t, err)
	raw := got.(*rawResponse)
	assert.Equal(t, "text/csv; charset=utf-8", raw.contentType)
	assert.True(t, strings.HasPrefix(raw.body, strings.Join(overrideCSVColumns, ",")+"\n"))
	assert.Contains(t, raw.body, `"'=HYPERLINK(""x"")"`, "a formula-like account name must be prefixed")

	rows, err := decodeOverridesCSV(raw.body)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	assert.Equal(t, "us-east-1;eu-west-1", strings.Join(rows[1].IncludeRegions, ";"))
	assert.Equal(t, 50.0, *rows[1].Coverage)
	assert.Nil(t, rows[1].Term, "an empty cell is an unset field")
}

func TestExportOverrides_RejectsUnknownFormat(t *testing.T) {
	h := transferHandler(t, newOverrideTransferStore(), nil)
	_, err := h.exportAccountServiceOverrides(context.Background(), exportRequest("xml"))
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
}

func TestOverrideTransferRoutes_PrecedeAccountPrefixRoutes(t *testing.T) {
	store := newOverrideTransferStore()
	r := NewRouter(transferHandler(t, store, nil))

	got, err := r.Route(context.Background(), "GET", "/api/accounts/service-overrides/export", exportRequest(""))
	require.NoError(t, err)
	assert.IsType(t, OverrideExport{}, got)

	body := `{"dry_run":true,"overrides":[{"account_id":"` + xferAWS + `","provider":"aws","service":"ec2"}]}`
	got, err = r.Route(context.Background(), "POST", "/api/accounts/service-overrides/import", adminRequest(body))
	require.NoError(t, err)
	assert.IsType(t, &OverrideImportReport{}, got)
}

// importBody marshals an import request.
func importBody(t *testing.T, in OverrideImportRequest) string {
	t.Helper()
	b, err := json.Marshal(in)
	require.NoError(t, err)
	return string(b)
}
