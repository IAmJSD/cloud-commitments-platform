package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// overrideTransferStore is an in-memory account and override store for the
// export/import tests (issue #9). Like Postgres, it refuses a ramp_schedule
// longer than the VARCHAR(32) column, and WithTx undoes every write of a
// failed transaction, audit events included. failSave, when set, injects a
// store failure; failEvent fails SaveConfigAuditEventTx. events holds the
// import events and, for each audited row write, an event carrying the row's
// audit context (issue #1493).
type overrideTransferStore struct {
	*MockConfigStore
	accounts  []config.CloudAccount
	overrides map[string][]config.AccountServiceOverride
	saved     []config.AccountServiceOverride
	events    []overrideTransferEvent
	failSave  func(*config.AccountServiceOverride) error
	failEvent error
}

// overrideTransferEvent is one audit write the in-memory store received.
type overrideTransferEvent struct {
	event *config.AuditEvent             // set for SaveConfigAuditEventTx
	row   *config.AccountServiceOverride // set for an audited row write
	audit config.AuditContext
}

func (s *overrideTransferStore) WithTx(_ context.Context, fn func(pgx.Tx) error) error {
	overrides := make(map[string][]config.AccountServiceOverride, len(s.overrides))
	for k, v := range s.overrides {
		overrides[k] = append([]config.AccountServiceOverride(nil), v...)
	}
	saved := append([]config.AccountServiceOverride(nil), s.saved...)
	events := append([]overrideTransferEvent(nil), s.events...)
	if err := fn(nil); err != nil {
		s.overrides, s.saved, s.events = overrides, saved, events
		return err
	}
	return nil
}

func (s *overrideTransferStore) SaveAccountServiceOverrideAuditedTx(ctx context.Context, _ pgx.Tx, o *config.AccountServiceOverride, audit config.AuditContext) error {
	if err := s.SaveAccountServiceOverride(ctx, o); err != nil {
		return err
	}
	row := *o
	s.events = append(s.events, overrideTransferEvent{row: &row, audit: audit})
	return nil
}

func (s *overrideTransferStore) SaveConfigAuditEventTx(_ context.Context, _ pgx.Tx, ev *config.AuditEvent) error {
	if s.failEvent != nil {
		return s.failEvent
	}
	if ev.ID == "" {
		ev.ID = "import-event"
	}
	copied := *ev
	s.events = append(s.events, overrideTransferEvent{event: &copied})
	return nil
}

func (s *overrideTransferStore) ListCloudAccounts(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
	return append([]config.CloudAccount(nil), s.accounts...), nil
}

func (s *overrideTransferStore) ListAccountServiceOverrides(_ context.Context, accountID string) ([]config.AccountServiceOverride, error) {
	return append([]config.AccountServiceOverride(nil), s.overrides[accountID]...), nil
}

func (s *overrideTransferStore) SaveAccountServiceOverride(_ context.Context, o *config.AccountServiceOverride) error {
	if o.RampSchedule != nil && utf8.RuneCountInString(*o.RampSchedule) > 32 {
		return fmt.Errorf("value too long for type character varying(32)")
	}
	if s.failSave != nil {
		if err := s.failSave(o); err != nil {
			return err
		}
	}
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

// formulaOverride holds a spreadsheet formula in every user-supplied text
// field the service-config rules do not restrict, and a value that itself
// starts with the escape quote.
func formulaOverride() config.AccountServiceOverride {
	return config.AccountServiceOverride{
		ID: "o-5", AccountID: xferAWS, Provider: "aws", Service: "opensearch",
		RampSchedule:   ptr(`=HYPERLINK("http://x","y")`),
		IncludeEngines: []string{"-2+3", "postgres"}, ExcludeEngines: []string{"@SUM(A1)"},
		IncludeRegions: []string{"+1"}, ExcludeRegions: []string{"'quoted", "=x"},
		IncludeTypes: []string{"=1+2"}, ExcludeTypes: []string{"'=1+2"},
	}
}

// csvTextColumns are the CSV cells that carry stored or user-supplied text.
var csvTextColumns = map[string]bool{
	"account_id": true, "account_name": true, "account_external_id": true, "provider": true, "service": true,
	"payment": true, "ramp_schedule": true,
	"include_engines": true, "exclude_engines": true, "include_regions": true,
	"exclude_regions": true, "include_types": true, "exclude_types": true,
}

func TestOverridesCSV_NeutralizesEveryTextCellAndRoundTrips(t *testing.T) {
	o := formulaOverride()
	o.Payment = ptr("+cmd")
	acct := &config.CloudAccount{ID: xferAWS, Name: "@name", Provider: "aws", ExternalID: "-123"}
	row := overrideTransferRowFrom(acct, &o)
	row.Provider, row.Service = "=aws", "+svc"

	text, err := encodeOverridesCSV([]OverrideTransferRow{row})
	require.NoError(t, err)
	records, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 2)
	for i, cell := range records[1] {
		col := records[0][i]
		if csvTextColumns[col] && cell != "" {
			assert.NotContains(t, "=+-@\t\r", cell[:1], "column %s cell %q would run as a formula", col, cell)
		}
	}

	back, err := decodeOverridesCSV(text)
	require.NoError(t, err)
	require.Len(t, back, 1)
	assert.Equal(t, row.fields(), back[0].fields(), "import must strip the escape prefix again")
	assert.Equal(t, []string{"-123", "=aws", "+svc"}, []string{back[0].AccountExternalID, back[0].Provider, back[0].Service})
}

func TestOverridesCSV_KeepsAQuoteThatIsNotAnEscape(t *testing.T) {
	csvText := "account_id,provider,service,include_types,ramp_schedule\n" + xferAWS + ",aws,ec2,'m5;'=x,'immediate\n"
	rows, err := decodeOverridesCSV(csvText)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, []string{"'m5", "'=x"}, rows[0].IncludeTypes, "only a leading cell prefix is an escape")
	assert.Equal(t, "'immediate", *rows[0].RampSchedule, "a quote before a non-formula character is data")
}

func TestImportOverrides_CSVWithFormulaValuesReimportsUnchanged(t *testing.T) {
	store := newOverrideTransferStore()
	store.overrides[xferAWS] = append(store.overrides[xferAWS], formulaOverride())
	h := transferHandler(t, store, nil)

	exported, err := h.exportAccountServiceOverrides(context.Background(), exportRequest("csv"))
	require.NoError(t, err)
	body := exported.(*rawResponse).body
	assert.Contains(t, body, `"'=HYPERLINK(""http://x"",""y"")"`)
	assert.Contains(t, body, ",'=1+2,''=1+2,")

	got, err := h.importAccountServiceOverrides(context.Background(), adminRequest(importBody(t, OverrideImportRequest{CSV: body})))
	require.NoError(t, err)
	report := got.(*OverrideImportReport)
	assert.Equal(t, 5, report.Summary[overrideActionUnchanged], "%+v", report.Rows)
	assert.Empty(t, store.saved)
}
