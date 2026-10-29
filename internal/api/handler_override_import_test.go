package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

func runImport(t *testing.T, h *Handler, in OverrideImportRequest) (*OverrideImportReport, error) {
	t.Helper()
	got, err := h.importAccountServiceOverrides(context.Background(), adminRequest(importBody(t, in)))
	if err != nil {
		return nil, err
	}
	return got.(*OverrideImportReport), nil
}

func actions(r *OverrideImportReport) []string {
	out := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		out = append(out, row.Action)
	}
	return out
}

func TestImportOverrides_ExportReimportIsUnchanged(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		t.Run(format, func(t *testing.T) {
			store := newOverrideTransferStore()
			h := transferHandler(t, store, nil)
			exported, err := h.exportAccountServiceOverrides(context.Background(), exportRequest(format))
			require.NoError(t, err)

			in := OverrideImportRequest{}
			if format == "csv" {
				in.CSV = exported.(*rawResponse).body
			} else {
				in.Overrides = exported.(OverrideExport).Overrides
			}
			report, err := runImport(t, h, in)
			require.NoError(t, err)
			assert.Equal(t, 4, report.Summary[overrideActionUnchanged], "%+v", report.Rows)
			assert.True(t, report.Applied)
			assert.Empty(t, store.saved, "an unchanged import writes nothing")
		})
	}
}

func TestImportOverrides_MatchesAnotherTenantsRowsByExternalID(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, nil)
	rows := []OverrideTransferRow{{
		AccountID:         "f0000000-0000-4000-8000-00000000f00d",
		AccountExternalID: "123456789012",
		Provider:          "aws", Service: "elasticache", Term: ptr(1), Payment: ptr("no-upfront"),
	}}

	report, err := runImport(t, h, OverrideImportRequest{Overrides: rows})
	require.NoError(t, err)
	assert.Equal(t, []string{overrideActionInsert}, actions(report))
	assert.Equal(t, xferAWS, report.Rows[0].AccountID)
	require.Len(t, store.saved, 1)
	assert.Equal(t, xferAWS, store.saved[0].AccountID)
	assert.Equal(t, 1, *store.saved[0].Term)
}

func TestImportOverrides_RowIsDesiredState(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, nil)
	// The stored ec2 override has enabled, term and payment; the row keeps
	// only enabled, so term and payment are cleared.
	rows := []OverrideTransferRow{{AccountID: xferAWS, Provider: "aws", Service: "ec2", Enabled: ptr(true)}}

	report, err := runImport(t, h, OverrideImportRequest{Overrides: rows})
	require.NoError(t, err)
	assert.Equal(t, []string{overrideActionUpdate}, actions(report))
	require.Len(t, store.saved, 1)
	assert.Equal(t, "o-1", store.saved[0].ID, "an update keeps the stored row's ID")
	assert.Nil(t, store.saved[0].Term)
	assert.Nil(t, store.saved[0].Payment)
}

func badImportRows() []OverrideTransferRow {
	return []OverrideTransferRow{
		{AccountID: xferAWS, Provider: "aws", Service: "ec2", Term: ptr(1)},                             // 1 update
		{AccountID: xferAWS, Provider: "aws", Service: "ec2", Term: ptr(3)},                             // 2 duplicate
		{AccountID: xferAzure, Provider: "azure", Service: "vm", Payment: ptr("all-upfront")},           // 3 AWS token on Azure
		{AccountID: xferAWS, Provider: "azure", Service: "vm"},                                          // 4 provider mismatch
		{AccountID: xferOther, Provider: "aws", Service: "ec2"},                                         // 5 out of scope
		{AccountID: xferAWS, Provider: "aws", Service: "redshift", Coverage: ptr(150.0)},                // 6 coverage range
		{AccountID: xferAWS, AccountExternalID: "000000000000", Provider: "aws", Service: "opensearch"}, // 7 external ID mismatch
		{AccountID: xferAWS, Provider: "aws", Service: "Bad Name"},                                      // 8 service name
		{AccountID: xferAzure, Provider: "azure", Service: "sql", Term: ptr(2)},                         // 9 term
		{AccountID: "", AccountExternalID: "sub-1", Provider: "azure", Service: "cosmos", Term: ptr(1)}, // 10 insert by external ID
	}
}

func TestImportOverrides_StrictRejectsAllWithReport(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, []string{xferAWS, xferAzure})

	_, err := runImport(t, h, OverrideImportRequest{Overrides: badImportRows()})
	ce, ok := IsClientError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, 400, ce.code)
	report := ce.details["report"].(*OverrideImportReport)
	assert.Equal(t, 8, report.Summary[overrideActionRejected])
	assert.False(t, report.Applied)
	assert.Empty(t, store.saved, "strict mode writes nothing when a row is rejected")
}

func TestImportOverrides_BestEffortAppliesValidRows(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, []string{xferAWS, xferAzure})

	report, err := runImport(t, h, OverrideImportRequest{Mode: overrideImportBestEffort, Overrides: badImportRows()})
	require.NoError(t, err)
	assert.Equal(t, []string{
		overrideActionUpdate, overrideActionRejected, overrideActionRejected, overrideActionRejected, overrideActionRejected,
		overrideActionRejected, overrideActionRejected, overrideActionRejected, overrideActionRejected, overrideActionInsert,
	}, actions(report))
	errs := map[int]string{}
	for _, r := range report.Rows {
		errs[r.Row] = r.Error
	}
	assert.Contains(t, errs[2], "duplicate of row 1")
	assert.Contains(t, errs[3], "invalid payment option")
	assert.Contains(t, errs[4], "is a aws account, not azure")
	assert.Contains(t, errs[5], "unknown account", "an out-of-scope account reads as unknown")
	assert.Contains(t, errs[6], "coverage must be between")
	assert.Contains(t, errs[7], "has external ID")
	assert.Contains(t, errs[8], "invalid service name")
	assert.Contains(t, errs[9], "term must be 1 or 3")
	assert.True(t, report.Applied)
	require.Len(t, store.saved, 2)
	assert.Equal(t, xferAzure, store.saved[1].AccountID)
}

func TestImportOverrides_DryRunWritesNothing(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, nil)
	rows := []OverrideTransferRow{
		{AccountID: xferAWS, Provider: "aws", Service: "ec2", Term: ptr(1)},
		{AccountID: xferAWS, Provider: "aws", Service: "memorydb"},
	}

	report, err := runImport(t, h, OverrideImportRequest{DryRun: true, Overrides: rows})
	require.NoError(t, err)
	assert.Equal(t, []string{overrideActionUpdate, overrideActionInsert}, actions(report))
	assert.True(t, report.DryRun)
	assert.False(t, report.Applied)
	assert.Empty(t, store.saved)
}

func TestImportOverrides_RejectsMalformedRequests(t *testing.T) {
	cases := map[string]string{
		"bad json":       `{`,
		"bad mode":       `{"mode":"yolo","csv":"provider,service\naws,ec2"}`,
		"no rows":        `{}`,
		"both forms":     `{"csv":"provider,service\naws,ec2","overrides":[{"provider":"aws","service":"ec2"}]}`,
		"unknown column": `{"csv":"provider,service,shade\naws,ec2,red"}`,
		"bad bool":       `{"csv":"provider,service,enabled\naws,ec2,maybe"}`,
		"header only":    `{"csv":"provider,service\n"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := transferHandler(t, newOverrideTransferStore(), nil)
			_, err := h.importAccountServiceOverrides(context.Background(), adminRequest(body))
			ce, ok := IsClientError(err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, 400, ce.code)
		})
	}
}

func TestImportOverrides_CSVRejectsNaNCoverage(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, nil)
	csv := "account_id,provider,service,coverage\n" + xferAWS + ",aws,ec2,NaN\n"

	report, err := runImport(t, h, OverrideImportRequest{Mode: overrideImportBestEffort, CSV: csv})
	require.NoError(t, err)
	assert.Equal(t, []string{overrideActionRejected}, actions(report))
	assert.Contains(t, report.Rows[0].Error, "coverage must be a number")
	assert.Empty(t, store.saved)
}

func TestImportOverrides_RequiresUpdateAccounts(t *testing.T) {
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", context.Background(), "admin-token").Return(adminAccountSession(), nil)
	mockAuth.grantPermissions(nil)
	h := &Handler{auth: mockAuth, config: newOverrideTransferStore()}

	_, err := h.importAccountServiceOverrides(context.Background(), adminRequest(`{"csv":"provider,service\naws,ec2"}`))
	ce, ok := IsClientError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, 403, ce.code)
}

// fortyImportRows is a valid 40-row file of new overrides on one account.
func fortyImportRows() []OverrideTransferRow {
	rows := make([]OverrideTransferRow, 40)
	for i := range rows {
		rows[i] = OverrideTransferRow{AccountID: xferAWS, Provider: "aws", Service: fmt.Sprintf("svc-%02d", i+1), Term: ptr(1), RampSchedule: ptr("immediate")}
	}
	return rows
}

func TestImportOverrides_StrictLateOverlongRampWritesNothing(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, nil)
	rows := fortyImportRows()
	rows[39].RampSchedule = ptr(strings.Repeat("r", 33)) // ramp_schedule is VARCHAR(32)

	_, err := runImport(t, h, OverrideImportRequest{Overrides: rows})
	ce, ok := IsClientError(err)
	require.True(t, ok, "a bad row must be a 4xx with a report, got %v", err)
	assert.Equal(t, 400, ce.code)
	report := ce.details["report"].(*OverrideImportReport)
	require.Len(t, report.Rows, 40)
	assert.Equal(t, 39, report.Summary[overrideActionInsert])
	assert.Equal(t, overrideActionRejected, report.Rows[39].Action)
	assert.Contains(t, report.Rows[39].Error, "ramp_schedule is 33 characters")
	assert.Empty(t, store.saved, "strict mode must not write rows 1-39")
	assert.Len(t, store.overrides[xferAWS], 2)
}

func TestImportOverrides_StoreFailureRollsBackEveryWrite(t *testing.T) {
	for _, mode := range []string{overrideImportStrict, overrideImportBestEffort} {
		t.Run(mode, func(t *testing.T) {
			store := newOverrideTransferStore()
			store.failSave = func(o *config.AccountServiceOverride) error {
				if o.Service == "svc-40" {
					return errors.New("connection reset")
				}
				return nil
			}
			h := transferHandler(t, store, nil)

			_, err := runImport(t, h, OverrideImportRequest{Mode: mode, Overrides: fortyImportRows()})
			require.Error(t, err)
			_, isClient := IsClientError(err)
			assert.False(t, isClient, "a store failure is a server error")
			assert.Contains(t, err.Error(), "row 40")
			assert.Empty(t, store.saved, "a failed write must roll back rows 1-39")
			assert.Len(t, store.overrides[xferAWS], 2)
		})
	}
}

func TestImportOverrides_RejectsValuesOutsideColumnBounds(t *testing.T) {
	cases := map[string]struct {
		row  OverrideTransferRow
		want string
	}{
		"ramp too long":      {OverrideTransferRow{RampSchedule: ptr(strings.Repeat("é", 33))}, "ramp_schedule is 33 characters"},
		"ramp control char":  {OverrideTransferRow{RampSchedule: ptr("weekly\n")}, "ramp_schedule must not contain control characters"},
		"coverage precision": {OverrideTransferRow{Coverage: ptr(33.333)}, "coverage must have at most 2 decimal places"},
		"list NUL":           {OverrideTransferRow{IncludeTypes: []string{"m5\x00"}}, "include_types must not contain control characters"},
		"list tab":           {OverrideTransferRow{ExcludeRegions: []string{"\tus-east-1"}}, "exclude_regions must not contain control characters"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newOverrideTransferStore()
			h := transferHandler(t, store, nil)
			row := tc.row
			row.AccountID, row.Provider, row.Service = xferAWS, "aws", "memorydb"

			report, err := runImport(t, h, OverrideImportRequest{Mode: overrideImportBestEffort, Overrides: []OverrideTransferRow{row}})
			require.NoError(t, err)
			require.Equal(t, []string{overrideActionRejected}, actions(report))
			assert.Contains(t, report.Rows[0].Error, tc.want)
			assert.Empty(t, store.saved)
		})
	}

	// The bounds themselves are accepted.
	store := newOverrideTransferStore()
	h := transferHandler(t, store, nil)
	row := OverrideTransferRow{AccountID: xferAWS, Provider: "aws", Service: "memorydb", RampSchedule: ptr(strings.Repeat("é", 32)), Coverage: ptr(33.33)}
	report, err := runImport(t, h, OverrideImportRequest{Overrides: []OverrideTransferRow{row}})
	require.NoError(t, err)
	assert.Equal(t, []string{overrideActionInsert}, actions(report))
}
