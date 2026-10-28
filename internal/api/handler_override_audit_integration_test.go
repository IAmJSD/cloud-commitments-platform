//go:build integration

package api

// Real-Postgres tests for the account service override audit trail (issue
// #1493): every override write through the single PUT, the single DELETE and
// an applied import leaves config_audit_events rows with the before and after
// values, in the same transaction as the write.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

type overrideAuditFixture struct {
	pg    *testhelpers.PostgresContainer
	store *config.PostgresStore
	h     *Handler
	acct  *config.CloudAccount
}

func newOverrideAuditFixture(ctx context.Context, t *testing.T) *overrideAuditFixture {
	t.Helper()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
	store := config.NewPostgresStore(pg.DB)
	acct := &config.CloudAccount{Name: "audit fixture", Provider: "aws", ExternalID: "123456789012", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, acct))
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(adminAccountSession(), nil)
	mockAuth.grantPermissionsScoped([]auth.Permission{{Action: auth.ActionAdmin, Resource: auth.ResourceAll}}, nil)
	return &overrideAuditFixture{pg: pg, store: store, h: &Handler{auth: mockAuth, config: store}, acct: acct}
}

// auditRow is one config_audit_events row, with before/after/details decoded.
type auditRow struct {
	ID, EventType, Actor   string
	ParentID, ActorUserID  *string
	AccountID              *string
	Provider, Service      *string
	Before, After, Details map[string]any
	BeforeNull, AfterNull  bool
}

func (f *overrideAuditFixture) events(ctx context.Context, t *testing.T) []auditRow {
	t.Helper()
	rows, err := f.pg.DB.Pool().Query(ctx, `
		SELECT id::text, event_type, actor, parent_event_id::text, actor_user_id::text,
		       account_id::text, provider, service, before, after, details
		FROM config_audit_events ORDER BY seq`)
	require.NoError(t, err)
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var before, after, details []byte
		require.NoError(t, rows.Scan(&r.ID, &r.EventType, &r.Actor, &r.ParentID, &r.ActorUserID,
			&r.AccountID, &r.Provider, &r.Service, &before, &after, &details))
		r.BeforeNull, r.AfterNull = before == nil, after == nil
		decodeAuditJSON(t, before, &r.Before)
		decodeAuditJSON(t, after, &r.After)
		decodeAuditJSON(t, details, &r.Details)
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func decodeAuditJSON(t *testing.T, raw []byte, dst *map[string]any) {
	t.Helper()
	if raw != nil {
		require.NoError(t, json.Unmarshal(raw, dst))
	}
}

func (f *overrideAuditFixture) put(ctx context.Context, t *testing.T, service, body string) {
	t.Helper()
	_, err := f.h.saveAccountServiceOverride(ctx, adminRequest(body), f.acct.ID+"/service-overrides/aws/"+service)
	require.NoError(t, err)
}

func (f *overrideAuditFixture) del(ctx context.Context, t *testing.T, service string) {
	t.Helper()
	_, err := f.h.deleteAccountServiceOverride(ctx, adminRequest(""), f.acct.ID+"/service-overrides/aws/"+service)
	require.NoError(t, err)
}

// TestOverrideAudit_SinglePutAndDelete: an insert, an update, a no-op save, a
// delete and a delete of nothing. Each change is one event naming the actor,
// the account, provider and service, with the stored row before and after.
func TestOverrideAudit_SinglePutAndDelete(t *testing.T) {
	ctx := context.Background()
	f := newOverrideAuditFixture(ctx, t)

	f.put(ctx, t, "rds", `{"term":1,"coverage":50}`)
	f.put(ctx, t, "rds", `{"term":3,"coverage":75.5}`)
	f.put(ctx, t, "rds", `{"term":3,"coverage":75.5}`) // no change: no event
	f.del(ctx, t, "rds")
	f.del(ctx, t, "rds") // nothing to delete: no event

	ev := f.events(ctx, t)
	require.Len(t, ev, 3)
	for _, e := range ev {
		assert.Equal(t, "admin@example.com", e.Actor)
		assert.Equal(t, adminAccountSession().UserID, *e.ActorUserID)
		assert.Equal(t, f.acct.ID, *e.AccountID)
		assert.Equal(t, []string{"aws", "rds"}, []string{*e.Provider, *e.Service})
		assert.Nil(t, e.ParentID)
	}

	insert, update, del := ev[0], ev[1], ev[2]
	assert.Equal(t, "account_service_override.upsert", insert.EventType)
	assert.True(t, insert.BeforeNull, "an insert has no before")
	assert.Equal(t, 1.0, insert.After["term"])
	assert.Equal(t, 50.0, insert.After["coverage"])

	assert.Equal(t, "account_service_override.upsert", update.EventType)
	assert.Equal(t, insert.After, update.Before, "the update's before is the row the insert wrote")
	assert.Equal(t, 75.5, update.After["coverage"])
	assert.Equal(t, 3.0, update.After["term"])

	assert.Equal(t, "account_service_override.delete", del.EventType)
	assert.Equal(t, update.After, del.Before)
	assert.True(t, del.AfterNull, "a delete has no after")
}

// TestOverrideAudit_Import: an applied import records one import event with
// its mode and counts, and one linked event per written row. Unchanged rows,
// dry runs and rejected strict imports write no events.
func TestOverrideAudit_Import(t *testing.T) {
	ctx := context.Background()
	f := newOverrideAuditFixture(ctx, t)
	f.put(ctx, t, "ec2", `{"term":1}`)
	f.put(ctx, t, "rds", `{"term":3}`)
	base := len(f.events(ctx, t))

	rows := []OverrideTransferRow{
		{AccountID: f.acct.ID, Provider: "aws", Service: "ec2", Term: ptr(3)},         // update
		{AccountID: f.acct.ID, Provider: "aws", Service: "rds", Term: ptr(3)},         // unchanged
		{AccountID: f.acct.ID, Provider: "aws", Service: "elasticache", Term: ptr(1)}, // insert
		{AccountID: f.acct.ID, Provider: "aws", Service: "opensearch", Term: ptr(7)},  // rejected
	}

	_, err := runImport(t, f.h, OverrideImportRequest{Overrides: rows, Mode: overrideImportStrict})
	require.Error(t, err)
	_, err = runImport(t, f.h, OverrideImportRequest{Overrides: rows, Mode: overrideImportBestEffort, DryRun: true})
	require.NoError(t, err)
	require.Len(t, f.events(ctx, t), base, "a rejected strict import and a dry run write no events")

	report, err := runImport(t, f.h, OverrideImportRequest{Overrides: rows, Mode: overrideImportBestEffort})
	require.NoError(t, err)
	require.True(t, report.Applied)

	ev := f.events(ctx, t)[base:]
	require.Len(t, ev, 3, "one import event and one event per written row")
	imp := ev[0]
	assert.Equal(t, "account_service_override.import", imp.EventType)
	assert.Equal(t, "admin@example.com", imp.Actor)
	assert.Nil(t, imp.AccountID)
	assert.Equal(t, "best_effort", imp.Details["mode"])
	assert.Equal(t, map[string]any{"insert": 1.0, "update": 1.0, "unchanged": 1.0, "rejected": 1.0}, imp.Details["summary"])

	byService := map[string]auditRow{}
	for _, e := range ev[1:] {
		assert.Equal(t, "account_service_override.upsert", e.EventType)
		require.NotNil(t, e.ParentID)
		assert.Equal(t, imp.ID, *e.ParentID)
		byService[*e.Service] = e
	}
	assert.Equal(t, 1.0, byService["ec2"].Before["term"])
	assert.Equal(t, 3.0, byService["ec2"].After["term"])
	assert.Equal(t, 1.0, byService["ec2"].Details["import_row"])
	assert.True(t, byService["elasticache"].BeforeNull)
	assert.Equal(t, 1.0, byService["elasticache"].After["term"])
	assert.Equal(t, 3.0, byService["elasticache"].Details["import_row"])

	// A write the database refuses part way rolls back the rows and every
	// event, the import event included.
	_, err = f.pg.DB.Pool().Exec(ctx, `
		CREATE FUNCTION reject_audit_svc() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'svc rejected'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_audit_svc BEFORE INSERT ON account_service_overrides
		FOR EACH ROW WHEN (NEW.service = 'zz-last') EXECUTE FUNCTION reject_audit_svc();`)
	require.NoError(t, err)
	before := len(f.events(ctx, t))
	_, err = runImport(t, f.h, OverrideImportRequest{Overrides: []OverrideTransferRow{
		{AccountID: f.acct.ID, Provider: "aws", Service: "ec2", Term: ptr(1)},
		{AccountID: f.acct.ID, Provider: "aws", Service: "zz-last", Term: ptr(1)},
	}})
	require.Error(t, err)
	require.Len(t, f.events(ctx, t), before, "a failed import leaves no events")
	got, err := f.store.GetAccountServiceOverride(ctx, f.acct.ID, "aws", "ec2")
	require.NoError(t, err)
	assert.Equal(t, 3, *got.Term, "and no override writes")
}

// TestOverrideAudit_ConcurrentFirstSavesChain: two PUTs creating the same
// override at once. The second writer replaces the first's row, so its
// event's before must be the first event's after; an audit that read the
// row before the other insert committed would record before = null for
// both and lose the first change from the trail.
func TestOverrideAudit_ConcurrentFirstSavesChain(t *testing.T) {
	ctx := context.Background()
	f := newOverrideAuditFixture(ctx, t)

	const rounds = 20
	broken := 0
	for round := 1; round <= rounds; round++ {
		service := fmt.Sprintf("svc-%02d", round)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, body := range []string{`{"term":1}`, `{"coverage":40}`} {
			wg.Add(1)
			go func(body string) {
				defer wg.Done()
				<-start
				_, err := f.h.saveAccountServiceOverride(ctx, adminRequest(body), f.acct.ID+"/service-overrides/aws/"+service)
				assert.NoError(t, err)
			}(body)
		}
		close(start)
		wg.Wait()

		var mine []auditRow
		for _, e := range f.events(ctx, t) {
			if e.Service != nil && *e.Service == service {
				mine = append(mine, e)
			}
		}
		require.Len(t, mine, 2, "round %d: one event per PUT", round)
		if !mine[0].BeforeNull || mine[1].BeforeNull || fmt.Sprint(mine[1].Before) != fmt.Sprint(mine[0].After) {
			broken++
			t.Logf("round %d: first before=%v after=%v, second before=%v", round, mine[0].Before, mine[0].After, mine[1].Before)
		}
	}
	require.Zero(t, broken, "%d of %d rounds recorded an audit chain that skips a write", broken, rounds)
}
