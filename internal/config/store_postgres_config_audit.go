package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Configuration audit trail (issue #1493, migration 000147). Every write to
// account_service_overrides records a config_audit_events row in the same
// transaction, so a change and its audit row commit or roll back together.

// Config audit event types.
const (
	AuditOverrideUpsert = "account_service_override.upsert"
	AuditOverrideDelete = "account_service_override.delete"
	AuditOverrideImport = "account_service_override.import"
)

// accountServiceOverrideLockPrefix namespaces the per-override advisory lock
// SaveAccountServiceOverrideAuditedTx takes. It is a distinct text input to
// the same hashtextextended(text, 0) bigint key space as the other
// hashtextextended locks; a 64-bit hash collision could only cause a spurious
// wait, never a correctness break.
const accountServiceOverrideLockPrefix = "cudly:account-service-override:"

// AuditActor is who made a change. Identity is never empty: the email,
// or the admin API key actor. UserID and APIKeyID are set only when they are
// real users/API key IDs.
type AuditActor struct {
	UserID   *string
	APIKeyID *string
	Identity string
}

// AuditContext is passed with each audited override write: the actor,
// the import event the write belongs to (nil for a single PUT or DELETE) and
// optional event details (an import row's number).
type AuditContext struct {
	Actor         AuditActor
	ParentEventID *string
	Details       map[string]any
}

// AuditEvent is one config_audit_events row. Before and After are the
// override settings as JSON (nil for none).
type AuditEvent struct {
	ID            string
	EventType     string
	ParentEventID *string
	Actor         AuditActor
	AccountID     *string
	Provider      *string
	Service       *string
	Before        json.RawMessage
	After         json.RawMessage
	Details       json.RawMessage
	CreatedAt     time.Time
}

// overrideAuditSnapshot is the before/after JSON of an override: its
// settings, without the identifiers and timestamps the event carries itself.
type overrideAuditSnapshot struct {
	Enabled        *bool    `json:"enabled"`
	Term           *int     `json:"term"`
	Payment        *string  `json:"payment"`
	Coverage       *float64 `json:"coverage"`
	RampSchedule   *string  `json:"ramp_schedule"`
	IncludeEngines []string `json:"include_engines"`
	ExcludeEngines []string `json:"exclude_engines"`
	IncludeRegions []string `json:"include_regions"`
	ExcludeRegions []string `json:"exclude_regions"`
	IncludeTypes   []string `json:"include_types"`
	ExcludeTypes   []string `json:"exclude_types"`
}

// overrideSnapshotJSON returns o's settings as JSON, or nil for a nil o.
func overrideSnapshotJSON(o *AccountServiceOverride) (json.RawMessage, error) {
	if o == nil {
		return nil, nil
	}
	b, err := json.Marshal(overrideAuditSnapshot{
		Enabled: o.Enabled, Term: o.Term, Payment: o.Payment, Coverage: o.Coverage, RampSchedule: o.RampSchedule,
		IncludeEngines: o.IncludeEngines, ExcludeEngines: o.ExcludeEngines,
		IncludeRegions: o.IncludeRegions, ExcludeRegions: o.ExcludeRegions,
		IncludeTypes: o.IncludeTypes, ExcludeTypes: o.ExcludeTypes,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode service override audit snapshot: %w", err)
	}
	return b, nil
}

const accountServiceOverrideColumns = `id, account_id, provider, service,
			enabled, term, payment, coverage, ramp_schedule,
			include_engines, exclude_engines, include_regions, exclude_regions,
			include_types, exclude_types,
			created_at, updated_at`

// scanAccountServiceOverride scans one row of accountServiceOverrideColumns.
// A missing row is (nil, nil).
func scanAccountServiceOverride(row pgx.Row) (*AccountServiceOverride, error) {
	var o AccountServiceOverride
	err := row.Scan(
		&o.ID, &o.AccountID, &o.Provider, &o.Service,
		&o.Enabled, &o.Term, &o.Payment, &o.Coverage, &o.RampSchedule,
		&o.IncludeEngines, &o.ExcludeEngines, &o.IncludeRegions, &o.ExcludeRegions,
		&o.IncludeTypes, &o.ExcludeTypes,
		&o.CreatedAt, &o.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// SaveAccountServiceOverrideAuditedTx upserts o inside tx and records the
// change in config_audit_events. It takes a transaction-scoped advisory lock
// on the override's (account, provider, service), then reads the stored row
// FOR UPDATE, so the event's before is exactly the row this write replaces
// even when another writer creates or changes the same override at the same
// moment (a row lock alone cannot serialize two creates of a row that does
// not exist yet). after is read back from the upsert, so it is what the
// columns hold (coverage rounded to DECIMAL(5,2)). A write that changes no
// setting records no event. o is updated to the stored row (its ID is the
// existing override's when one was replaced).
//
// Callers writing several overrides in one tx must call this in a consistent
// key order, or two such transactions could deadlock on each other's locks.
func (s *PostgresStore) SaveAccountServiceOverrideAuditedTx(ctx context.Context, tx pgx.Tx, o *AccountServiceOverride, audit AuditContext) error {
	key := accountServiceOverrideLockPrefix + o.AccountID + "/" + o.Provider + "/" + o.Service
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key); err != nil {
		return fmt.Errorf("failed to lock service override: %w", err)
	}
	before, err := scanAccountServiceOverride(tx.QueryRow(ctx,
		`SELECT `+accountServiceOverrideColumns+` FROM account_service_overrides
		WHERE account_id = $1 AND provider = $2 AND service = $3 FOR UPDATE`,
		o.AccountID, o.Provider, o.Service))
	if err != nil {
		return fmt.Errorf("failed to read service override: %w", err)
	}
	after, err := upsertAccountServiceOverrideReturning(ctx, tx, o)
	if err != nil {
		return err
	}
	*o = *after
	return recordOverrideChange(ctx, tx, AuditOverrideUpsert, o.AccountID, o.Provider, o.Service, before, after, audit)
}

// upsertAccountServiceOverrideReturning is saveAccountServiceOverride with the
// stored row read back.
func upsertAccountServiceOverrideReturning(ctx context.Context, tx pgx.Tx, o *AccountServiceOverride) (*AccountServiceOverride, error) {
	if o.ID == "" {
		o.ID = uuid.New().String()
	}
	now := time.Now()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	stored, err := scanAccountServiceOverride(tx.QueryRow(ctx, `
		INSERT INTO account_service_overrides (`+accountServiceOverrideColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (account_id, provider, service) DO UPDATE SET
			enabled = $5, term = $6, payment = $7, coverage = $8, ramp_schedule = $9,
			include_engines = $10, exclude_engines = $11,
			include_regions = $12, exclude_regions = $13,
			include_types = $14, exclude_types = $15,
			updated_at = NOW()
		RETURNING `+accountServiceOverrideColumns,
		o.ID, o.AccountID, o.Provider, o.Service,
		o.Enabled, o.Term, o.Payment, o.Coverage, o.RampSchedule,
		o.IncludeEngines, o.ExcludeEngines, o.IncludeRegions, o.ExcludeRegions,
		o.IncludeTypes, o.ExcludeTypes,
		o.CreatedAt, now,
	))
	if err != nil {
		return nil, fmt.Errorf("failed to save service override: %w", err)
	}
	if stored == nil {
		return nil, fmt.Errorf("failed to save service override: no row returned")
	}
	return stored, nil
}

// DeleteAccountServiceOverrideAuditedTx deletes the override inside tx and
// records the deleted row as the event's before. Deleting an override that
// does not exist records nothing.
func (s *PostgresStore) DeleteAccountServiceOverrideAuditedTx(ctx context.Context, tx pgx.Tx, accountID, provider, service string, audit AuditContext) error {
	before, err := scanAccountServiceOverride(tx.QueryRow(ctx,
		`DELETE FROM account_service_overrides WHERE account_id = $1 AND provider = $2 AND service = $3
		RETURNING `+accountServiceOverrideColumns,
		accountID, provider, service))
	if err != nil {
		return fmt.Errorf("failed to delete service override: %w", err)
	}
	return recordOverrideChange(ctx, tx, AuditOverrideDelete, accountID, provider, service, before, nil, audit)
}

// recordOverrideChange writes the event for one override write, unless the
// write changed no setting.
func recordOverrideChange(ctx context.Context, tx pgx.Tx, eventType, accountID, provider, service string, before, after *AccountServiceOverride, audit AuditContext) error {
	beforeJSON, err := overrideSnapshotJSON(before)
	if err != nil {
		return err
	}
	afterJSON, err := overrideSnapshotJSON(after)
	if err != nil {
		return err
	}
	if bytes.Equal(beforeJSON, afterJSON) {
		return nil // nothing changed (or nothing to delete)
	}
	details, err := auditDetailsJSON(audit.Details)
	if err != nil {
		return err
	}
	return insertConfigAuditEvent(ctx, tx, &AuditEvent{
		EventType: eventType, ParentEventID: audit.ParentEventID, Actor: audit.Actor,
		AccountID: &accountID, Provider: &provider, Service: &service,
		Before: beforeJSON, After: afterJSON, Details: details,
	})
}

func auditDetailsJSON(details map[string]any) (json.RawMessage, error) {
	if len(details) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(details)
	if err != nil {
		return nil, fmt.Errorf("failed to encode audit event details: %w", err)
	}
	return b, nil
}

// SaveConfigAuditEventTx records ev inside tx. An empty ID is filled in, so a
// caller can link later events to it via ParentEventID.
func (s *PostgresStore) SaveConfigAuditEventTx(ctx context.Context, tx pgx.Tx, ev *AuditEvent) error {
	return insertConfigAuditEvent(ctx, tx, ev)
}

func insertConfigAuditEvent(ctx context.Context, tx pgx.Tx, ev *AuditEvent) error {
	if ev.Actor.Identity == "" {
		return fmt.Errorf("config audit event %s has no actor", ev.EventType)
	}
	if ev.ID == "" {
		ev.ID = uuid.New().String()
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO config_audit_events (
			id, event_type, parent_event_id, actor, actor_user_id, actor_api_key_id,
			account_id, provider, service, before, after, details
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING created_at`,
		ev.ID, ev.EventType, ev.ParentEventID, ev.Actor.Identity, ev.Actor.UserID, ev.Actor.APIKeyID,
		ev.AccountID, ev.Provider, ev.Service, nullJSON(ev.Before), nullJSON(ev.After), nullJSON(ev.Details),
	).Scan(&ev.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to record config audit event: %w", err)
	}
	return nil
}

// nullJSON passes an empty JSON value as SQL NULL rather than an empty
// string, which JSONB would refuse.
func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
