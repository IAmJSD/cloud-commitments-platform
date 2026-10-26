package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
)

// maxOverrideImportRows bounds one import request.
const maxOverrideImportRows = 5000

// Import modes: strict applies nothing when any row is rejected; best_effort
// applies the valid rows and reports the rest.
const (
	overrideImportStrict     = "strict"
	overrideImportBestEffort = "best_effort"
)

// Per-row import actions.
const (
	overrideActionInsert    = "insert"
	overrideActionUpdate    = "update"
	overrideActionUnchanged = "unchanged"
	overrideActionRejected  = "rejected"
)

// OverrideImportRequest is the POST /api/accounts/service-overrides/import
// body. Exactly one of Overrides (the JSON export's rows) or CSV (the CSV
// export's text) is set.
type OverrideImportRequest struct {
	Mode      string                `json:"mode,omitempty"`
	DryRun    bool                  `json:"dry_run,omitempty"`
	Overrides []OverrideTransferRow `json:"overrides,omitempty"`
	CSV       string                `json:"csv,omitempty"`
}

// OverrideImportRowResult reports what import did, or would do, with one row.
// Row is 1-based in file order.
type OverrideImportRowResult struct {
	Row       int    `json:"row"`
	AccountID string `json:"account_id,omitempty"`
	Provider  string `json:"provider"`
	Service   string `json:"service"`
	Action    string `json:"action"`
	Error     string `json:"error,omitempty"`
}

// OverrideImportReport is the import response. Applied is true when the
// insert and update rows were written (never on a dry run, or in strict mode
// with a rejected row).
type OverrideImportReport struct {
	Mode    string                    `json:"mode"`
	DryRun  bool                      `json:"dry_run"`
	Applied bool                      `json:"applied"`
	Summary map[string]int            `json:"summary"`
	Rows    []OverrideImportRowResult `json:"rows"`
}

// plannedOverride is a validated row with the stored override it replaces.
type plannedOverride struct {
	account  *config.CloudAccount
	row      *OverrideTransferRow
	existing *config.AccountServiceOverride
}

// importAccountServiceOverrides handles POST
// /api/accounts/service-overrides/import (issue #9). Each row is resolved to
// an in-scope account (by account_id, else by provider and
// account_external_id, so a file from another tenant imports), validated with
// the same rules as a service config, and compared with the stored override.
// A row sets every field of its override; overrides not in the file are left
// alone. A strict import with a rejected row answers 400 with the report and
// writes nothing. Re-running an import is safe: writes are upserts.
func (h *Handler) importAccountServiceOverrides(ctx context.Context, req *events.LambdaFunctionURLRequest) (any, error) {
	session, err := h.requirePermission(ctx, req, "update", "accounts")
	if err != nil {
		return nil, err
	}
	in, rows, err := parseOverrideImportRequest(req.Body)
	if err != nil {
		return nil, err
	}
	accounts, err := h.scopedAccounts(ctx, session)
	if err != nil {
		return nil, err
	}
	report, plan, err := h.planOverrideImport(ctx, newOverrideAccountIndex(accounts), rows)
	if err != nil {
		return nil, err
	}
	report.Mode, report.DryRun = in.Mode, in.DryRun
	if report.Summary[overrideActionRejected] > 0 && in.Mode == overrideImportStrict {
		return nil, NewClientErrorWithDetails(400, "import rejected: fix the rejected rows or use best_effort mode", map[string]any{"report": report})
	}
	if in.DryRun {
		return report, nil
	}
	if err := h.applyOverrideImport(ctx, plan); err != nil {
		return nil, err
	}
	report.Applied = true
	return report, nil
}

func parseOverrideImportRequest(body string) (OverrideImportRequest, []OverrideTransferRow, error) {
	var in OverrideImportRequest
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		return in, nil, NewClientError(400, "invalid request body")
	}
	if in.Mode == "" {
		in.Mode = overrideImportStrict
	}
	if in.Mode != overrideImportStrict && in.Mode != overrideImportBestEffort {
		return in, nil, NewClientError(400, "mode must be strict or best_effort")
	}
	if (in.CSV == "") == (len(in.Overrides) == 0) {
		return in, nil, NewClientError(400, "send exactly one of overrides or csv, with at least one row")
	}
	rows, err := overrideImportRows(&in)
	return in, rows, err
}

// overrideImportRows returns the request's rows (decoding the CSV form) and
// enforces the 1..maxOverrideImportRows bound.
func overrideImportRows(in *OverrideImportRequest) ([]OverrideTransferRow, error) {
	rows := in.Overrides
	if in.CSV != "" {
		parsed, err := decodeOverridesCSV(in.CSV)
		if err != nil {
			return nil, NewClientError(400, err.Error())
		}
		rows = parsed
	}
	if len(rows) == 0 || len(rows) > maxOverrideImportRows {
		return nil, NewClientError(400, fmt.Sprintf("an import needs 1 to %d rows, got %d", maxOverrideImportRows, len(rows)))
	}
	return rows, nil
}

// planOverrideImport resolves and validates every row and decides its action.
// Only store read failures are returned as errors; a bad row is a rejection.
func (h *Handler) planOverrideImport(ctx context.Context, idx *overrideAccountIndex, rows []OverrideTransferRow) (*OverrideImportReport, []plannedOverride, error) {
	report := &OverrideImportReport{Summary: map[string]int{
		overrideActionInsert: 0, overrideActionUpdate: 0, overrideActionUnchanged: 0, overrideActionRejected: 0,
	}}
	existingByAccount := map[string][]config.AccountServiceOverride{}
	seen := map[string]int{}
	var plan []plannedOverride
	for i := range rows {
		res := OverrideImportRowResult{Row: i + 1, Provider: rows[i].Provider, Service: rows[i].Service}
		p, rowErr := h.planOverrideRow(ctx, idx, &rows[i], seen, res.Row)
		if rowErr == nil {
			p.existing, rowErr = h.findStoredOverride(ctx, existingByAccount, p.account.ID, &rows[i])
		}
		if rowErr != nil && !isRowRejection(rowErr) {
			return nil, nil, rowErr
		}
		res.Action, res.Error = overrideRowAction(p, rowErr)
		if p.account != nil {
			res.AccountID = p.account.ID
		}
		report.Summary[res.Action]++
		report.Rows = append(report.Rows, res)
		if res.Action == overrideActionInsert || res.Action == overrideActionUpdate {
			plan = append(plan, p)
		}
	}
	return report, plan, nil
}

func overrideRowAction(p plannedOverride, err error) (action, msg string) {
	switch {
	case err != nil:
		return overrideActionRejected, err.Error()
	case p.existing == nil:
		return overrideActionInsert, ""
	case overrideFieldsOf(p.existing).equal(p.row.fields()):
		return overrideActionUnchanged, ""
	default:
		return overrideActionUpdate, ""
	}
}

// planOverrideRow resolves the row's account and validates it. seen maps each
// account/provider/service already in the file to its row number.
func (h *Handler) planOverrideRow(ctx context.Context, idx *overrideAccountIndex, row *OverrideTransferRow, seen map[string]int, rowNum int) (plannedOverride, error) {
	acct, err := idx.resolve(row)
	if err != nil {
		return plannedOverride{}, err
	}
	p := plannedOverride{account: acct, row: row}
	key := acct.ID + "/" + row.Provider + "/" + row.Service
	if first, dup := seen[key]; dup {
		return p, rejectRow("duplicate of row %d", first)
	}
	seen[key] = rowNum
	return p, h.validateOverrideRow(ctx, row)
}

// validateOverrideRow applies the service-config rules (provider, service
// name, term, provider-specific payment token, coverage) and the
// commitment-option combination check the PUT endpoint runs.
func (h *Handler) validateOverrideRow(ctx context.Context, row *OverrideTransferRow) error {
	if !serviceNameRegex.MatchString(row.Service) {
		return rejectRow("invalid service name %q", row.Service)
	}
	cfg := config.ServiceConfig{Provider: row.Provider, Service: row.Service, Term: derefInt(row.Term), Payment: derefString(row.Payment)}
	if row.Coverage != nil {
		if math.IsNaN(*row.Coverage) || math.IsInf(*row.Coverage, 0) {
			return rejectRow("coverage must be a number between %d and %d", config.MinCoverage, config.MaxCoverage)
		}
		cfg.Coverage = *row.Coverage
	}
	if err := cfg.Validate(); err != nil {
		return rejectRow("%s", err.Error())
	}
	if err := h.checkCommitmentOptionCombo(ctx, cfg); err != nil {
		if ce, ok := IsClientError(err); ok && ce.code == 400 {
			return rejectRow("%s", ce.Error())
		}
		return err
	}
	return nil
}

// findStoredOverride returns the stored override the row replaces, or nil.
// Each account's overrides are read once per import.
func (h *Handler) findStoredOverride(ctx context.Context, cache map[string][]config.AccountServiceOverride, accountID string, row *OverrideTransferRow) (*config.AccountServiceOverride, error) {
	stored, ok := cache[accountID]
	if !ok {
		var err error
		if stored, err = h.config.ListAccountServiceOverrides(ctx, accountID); err != nil {
			return nil, fmt.Errorf("accounts: %w", err)
		}
		cache[accountID] = stored
	}
	for i := range stored {
		if stored[i].Provider == row.Provider && stored[i].Service == row.Service {
			return &stored[i], nil
		}
	}
	return nil, nil
}

// applyOverrideImport writes the insert and update rows. A store failure
// stops the import part way; re-running it is safe because writes upsert.
func (h *Handler) applyOverrideImport(ctx context.Context, plan []plannedOverride) error {
	now := time.Now()
	for i, p := range plan {
		o := &config.AccountServiceOverride{ID: uuid.New().String(), AccountID: p.account.ID, Provider: p.row.Provider, Service: p.row.Service, CreatedAt: now, UpdatedAt: now}
		if p.existing != nil {
			o.ID, o.CreatedAt = p.existing.ID, p.existing.CreatedAt
		}
		applyOverrideFieldsTo(o, p.row.fields())
		if err := h.config.SaveAccountServiceOverride(ctx, o); err != nil {
			return fmt.Errorf("accounts: import stopped after %d of %d writes: %w", i, len(plan), err)
		}
	}
	return nil
}
