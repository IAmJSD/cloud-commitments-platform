//go:build integration

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

// TestImportOverrides_StrictIsAllOrNothingPostgres runs the override import
// (issue #9) against Postgres: a strict import whose last row breaks a column
// bound is a 400 with a report and writes nothing, and a write the database
// refuses part way rolls the whole import back.
func TestImportOverrides_StrictIsAllOrNothingPostgres(t *testing.T) {
	ctx := context.Background()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)
	acct := &config.CloudAccount{Name: "import fixture", Provider: "aws", ExternalID: "123456789012", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, acct))

	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminAccountSession(), nil)
	mockAuth.grantPermissionsScoped([]auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}, nil)
	h := &Handler{auth: mockAuth, config: store}

	rows := fortyImportRows()
	for i := range rows {
		rows[i].AccountID = acct.ID
	}
	count := func() int {
		var n int
		require.NoError(t, pg.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM account_service_overrides WHERE account_id = $1`, acct.ID).Scan(&n))
		return n
	}

	// Row 40's ramp_schedule does not fit VARCHAR(32).
	bad := append([]OverrideTransferRow(nil), rows...)
	bad[39].RampSchedule = ptr(strings.Repeat("r", 33))
	_, err := runImport(t, h, OverrideImportRequest{Overrides: bad})
	ce, ok := IsClientError(err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, 400, ce.code)
	report := ce.details["report"].(*OverrideImportReport)
	assert.Equal(t, overrideActionRejected, report.Rows[39].Action)
	assert.Equal(t, 0, count(), "a rejected strict import writes nothing")

	// The database refuses row 40: rows 1-39 roll back with it.
	_, err = pg.DB.Pool().Exec(ctx, `
		CREATE FUNCTION reject_svc_40() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'svc-40 rejected'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_svc_40 BEFORE INSERT ON account_service_overrides
		FOR EACH ROW WHEN (NEW.service = 'svc-40') EXECUTE FUNCTION reject_svc_40();`)
	require.NoError(t, err)
	_, err = runImport(t, h, OverrideImportRequest{Overrides: rows})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row 40")
	assert.Equal(t, 0, count(), "a failed write rolls the whole import back")

	_, err = pg.DB.Pool().Exec(ctx, `DROP TRIGGER reject_svc_40 ON account_service_overrides`)
	require.NoError(t, err)
	got, err := runImport(t, h, OverrideImportRequest{Overrides: rows})
	require.NoError(t, err)
	assert.True(t, got.Applied)
	assert.Equal(t, 40, count())
}
