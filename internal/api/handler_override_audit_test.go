package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Unit tests of the override audit trail wiring (issue #1493). The store
// behavior (locks, before/after, no-op skip) is covered against Postgres in
// handler_override_audit_integration_test.go.

func adminAuditActor() config.AuditActor {
	uid := adminAccountSession().UserID
	return config.AuditActor{UserID: &uid, Identity: "admin@example.com"}
}

// TestSaveAccountServiceOverride_WritesThroughAuditedTx: the PUT writes via
// the audited store method inside a transaction, naming the session's actor,
// and never through the unaudited SaveAccountServiceOverride.
func TestSaveAccountServiceOverride_WritesThroughAuditedTx(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	setupAdminAuth(ctx, mockAuth)
	store := new(MockConfigStore)
	var gotAudit config.AuditContext
	store.On("SaveAccountServiceOverrideAuditedTx", ctx, mock.Anything, mock.AnythingOfType("*config.AccountServiceOverride"), mock.Anything).
		Run(func(args mock.Arguments) { gotAudit = args.Get(3).(config.AuditContext) }).Return(nil)
	h := &Handler{auth: mockAuth, config: store}

	_, err := h.saveAccountServiceOverride(ctx, adminRequest(`{"term":1}`), "11111111-1111-1111-1111-111111111111/service-overrides/aws/ec2")
	require.NoError(t, err)
	assert.Equal(t, config.AuditContext{Actor: adminAuditActor()}, gotAudit)
	store.AssertCalled(t, "WithTx", ctx, mock.Anything)
	store.AssertNotCalled(t, "SaveAccountServiceOverride", mock.Anything, mock.Anything)
}

// TestSaveAccountServiceOverride_AuditFailureFailsTheSave: a failed audit
// write is a failed save, not a silent unaudited one.
func TestSaveAccountServiceOverride_AuditFailureFailsTheSave(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	setupAdminAuth(ctx, mockAuth)
	store := new(MockConfigStore)
	store.On("SaveAccountServiceOverrideAuditedTx", ctx, mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("failed to record config audit event"))
	h := &Handler{auth: mockAuth, config: store}

	_, err := h.saveAccountServiceOverride(ctx, adminRequest(`{"term":1}`), "11111111-1111-1111-1111-111111111111/service-overrides/aws/ec2")
	require.ErrorContains(t, err, "config audit event")
}

func TestDeleteAccountServiceOverride_WritesThroughAuditedTx(t *testing.T) {
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	setupAdminAuth(ctx, mockAuth)
	store := new(MockConfigStore)
	store.On("DeleteAccountServiceOverrideAuditedTx", ctx, mock.Anything,
		"11111111-1111-1111-1111-111111111111", "aws", "ec2", config.AuditContext{Actor: adminAuditActor()}).Return(nil)
	h := &Handler{auth: mockAuth, config: store}

	_, err := h.deleteAccountServiceOverride(ctx, adminRequest(""), "11111111-1111-1111-1111-111111111111/service-overrides/aws/ec2")
	require.NoError(t, err)
	store.AssertExpectations(t)
	store.AssertCalled(t, "WithTx", ctx, mock.Anything)
	store.AssertNotCalled(t, "DeleteAccountServiceOverride", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestImportOverrides_RecordsLinkedAuditEvents: an applied import records the
// import event (mode, counts) first, then one audited write per inserted or
// updated row, each linked to it and carrying its file row number, in key
// order rather than file order.
func TestImportOverrides_RecordsLinkedAuditEvents(t *testing.T) {
	store := newOverrideTransferStore()
	h := transferHandler(t, store, nil)
	rows := []OverrideTransferRow{
		{AccountID: xferAWS, Provider: "aws", Service: "rds", Coverage: ptr(60.0)},                     // update
		{AccountID: xferAWS, Provider: "aws", Service: "elasticache", Term: ptr(1)},                    // insert
		{AccountID: xferAzure, Provider: "azure", Service: "vm", Enabled: ptr(false)},                  // unchanged
		{AccountID: xferAWS, Provider: "aws", Service: "opensearch", Term: ptr(7)},                     // rejected
		{AccountID: xferAWS, Provider: "aws", Service: "ec2", Enabled: ptr(true), Payment: ptr("bad")}, // rejected
	}

	report, err := runImport(t, h, OverrideImportRequest{Mode: overrideImportBestEffort, Overrides: rows})
	require.NoError(t, err)
	require.True(t, report.Applied)

	require.Len(t, store.events, 3)
	imp := store.events[0].event
	require.NotNil(t, imp, "the import event is written first")
	assert.Equal(t, config.AuditOverrideImport, imp.EventType)
	assert.Equal(t, adminAuditActor(), imp.Actor)
	var details map[string]any
	require.NoError(t, json.Unmarshal(imp.Details, &details))
	assert.Equal(t, "best_effort", details["mode"])
	assert.Equal(t, 5.0, details["rows"])
	assert.Equal(t, map[string]any{"insert": 1.0, "update": 1.0, "unchanged": 1.0, "rejected": 2.0}, details["summary"])

	services := make([]string, 0, len(store.events)-1)
	for _, e := range store.events[1:] {
		require.NotNil(t, e.row)
		require.NotNil(t, e.audit.ParentEventID)
		assert.Equal(t, imp.ID, *e.audit.ParentEventID)
		assert.Equal(t, adminAuditActor(), e.audit.Actor)
		services = append(services, e.row.Service)
	}
	assert.Equal(t, []string{"elasticache", "rds"}, services, "rows are written in key order")
	assert.Equal(t, map[string]any{"import_row": 2}, store.events[1].audit.Details)
	assert.Equal(t, map[string]any{"import_row": 1}, store.events[2].audit.Details)
}

// TestImportOverrides_NoAuditEventsWithoutWrites: a dry run, a rejected
// strict import and an import rolled back by a store failure leave no events.
func TestImportOverrides_NoAuditEventsWithoutWrites(t *testing.T) {
	t.Run("dry run", func(t *testing.T) {
		store := newOverrideTransferStore()
		_, err := runImport(t, transferHandler(t, store, nil), OverrideImportRequest{DryRun: true, Overrides: fortyImportRows()})
		require.NoError(t, err)
		assert.Empty(t, store.events)
	})
	t.Run("strict with a rejected row", func(t *testing.T) {
		store := newOverrideTransferStore()
		rows := fortyImportRows()
		rows[39].Term = ptr(7)
		_, err := runImport(t, transferHandler(t, store, nil), OverrideImportRequest{Overrides: rows})
		require.Error(t, err)
		assert.Empty(t, store.events)
	})
	t.Run("store failure", func(t *testing.T) {
		store := newOverrideTransferStore()
		store.failSave = func(o *config.AccountServiceOverride) error {
			if o.Service == "svc-40" {
				return errors.New("connection reset")
			}
			return nil
		}
		_, err := runImport(t, transferHandler(t, store, nil), OverrideImportRequest{Overrides: fortyImportRows()})
		require.Error(t, err)
		assert.Empty(t, store.events, "the rollback takes the import event and rows 1-39's events with it")
	})
}

// TestImportOverrides_ImportEventFailureWritesNothing: no row is written when
// the import event cannot be recorded.
func TestImportOverrides_ImportEventFailureWritesNothing(t *testing.T) {
	store := newOverrideTransferStore()
	store.failEvent = errors.New("audit insert failed")

	_, err := runImport(t, transferHandler(t, store, nil), OverrideImportRequest{Overrides: fortyImportRows()})
	require.ErrorContains(t, err, "audit insert failed")
	assert.Empty(t, store.saved)
}

func TestConfigAuditActor(t *testing.T) {
	user := configAuditActor(&Session{UserID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Email: "a@example.com", UserAPIKeyID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"})
	require.NotNil(t, user.UserID)
	require.NotNil(t, user.APIKeyID)
	assert.Equal(t, "a@example.com", user.Identity)
	assert.Equal(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", *user.APIKeyID)

	admin := configAuditActor(&Session{UserID: apiKeyAdminUserID})
	assert.Nil(t, admin.UserID, "the admin API key's sentinel is not a user ID")
	assert.Nil(t, admin.APIKeyID)
	assert.Equal(t, apiKeyAdminUserID, admin.Identity)
}
