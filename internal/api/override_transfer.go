package api

import (
	"reflect"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// OverrideTransferRow is one account_service_overrides row in an export or
// import file (issue #9). The account is named twice: AccountID is the
// tenant-local UUID, and Provider plus AccountExternalID (AWS account number,
// Azure subscription, GCP project) identify it portably, so a file exported
// from one tenant can be imported into another. AccountName, CreatedAt and
// UpdatedAt are informational: import ignores them.
//
// An import row is the desired state of the override: an unset field clears
// that field, unlike the sparse PUT /api/accounts/:id/service-overrides/...
// merge. Overrides missing from the file are left alone.
type OverrideTransferRow struct {
	AccountID         string     `json:"account_id,omitempty"`
	AccountName       string     `json:"account_name,omitempty"`
	AccountExternalID string     `json:"account_external_id,omitempty"`
	Provider          string     `json:"provider"`
	Service           string     `json:"service"`
	Enabled           *bool      `json:"enabled,omitempty"`
	Term              *int       `json:"term,omitempty"`
	Payment           *string    `json:"payment,omitempty"`
	Coverage          *float64   `json:"coverage,omitempty"`
	RampSchedule      *string    `json:"ramp_schedule,omitempty"`
	IncludeEngines    []string   `json:"include_engines,omitempty"`
	ExcludeEngines    []string   `json:"exclude_engines,omitempty"`
	IncludeRegions    []string   `json:"include_regions,omitempty"`
	ExcludeRegions    []string   `json:"exclude_regions,omitempty"`
	IncludeTypes      []string   `json:"include_types,omitempty"`
	ExcludeTypes      []string   `json:"exclude_types,omitempty"`
	CreatedAt         *time.Time `json:"created_at,omitempty"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
}

// OverrideExport is the JSON export document.
type OverrideExport struct {
	Version    int                   `json:"version"`
	ExportedAt time.Time             `json:"exported_at"`
	Overrides  []OverrideTransferRow `json:"overrides"`
}

// overrideExportVersion is bumped when the row shape changes incompatibly.
const overrideExportVersion = 1

// overrideTransferRowFrom renders a stored override with its account.
func overrideTransferRowFrom(acct *config.CloudAccount, o *config.AccountServiceOverride) OverrideTransferRow {
	row := OverrideTransferRow{
		AccountID:         acct.ID,
		AccountName:       acct.Name,
		AccountExternalID: acct.ExternalID,
		Provider:          o.Provider,
		Service:           o.Service,
		CreatedAt:         timePtrOrNil(o.CreatedAt),
		UpdatedAt:         timePtrOrNil(o.UpdatedAt),
	}
	setOverrideFields(&row, overrideFieldsOf(o))
	return row
}

func timePtrOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// overrideFields are the settable fields of an override, shared by the stored
// row and the transfer row so import can compare and copy them as one value.
type overrideFields struct {
	Enabled        *bool
	Term           *int
	Payment        *string
	Coverage       *float64
	RampSchedule   *string
	IncludeEngines []string
	ExcludeEngines []string
	IncludeRegions []string
	ExcludeRegions []string
	IncludeTypes   []string
	ExcludeTypes   []string
}

func overrideFieldsOf(o *config.AccountServiceOverride) overrideFields {
	return overrideFields{
		Enabled: o.Enabled, Term: o.Term, Payment: o.Payment, Coverage: o.Coverage, RampSchedule: o.RampSchedule,
		IncludeEngines: o.IncludeEngines, ExcludeEngines: o.ExcludeEngines,
		IncludeRegions: o.IncludeRegions, ExcludeRegions: o.ExcludeRegions,
		IncludeTypes: o.IncludeTypes, ExcludeTypes: o.ExcludeTypes,
	}
}

func (r *OverrideTransferRow) fields() overrideFields {
	return overrideFields{
		Enabled: r.Enabled, Term: r.Term, Payment: r.Payment, Coverage: r.Coverage, RampSchedule: r.RampSchedule,
		IncludeEngines: r.IncludeEngines, ExcludeEngines: r.ExcludeEngines,
		IncludeRegions: r.IncludeRegions, ExcludeRegions: r.ExcludeRegions,
		IncludeTypes: r.IncludeTypes, ExcludeTypes: r.ExcludeTypes,
	}
}

func setOverrideFields(r *OverrideTransferRow, f overrideFields) {
	r.Enabled, r.Term, r.Payment, r.Coverage, r.RampSchedule = f.Enabled, f.Term, f.Payment, f.Coverage, f.RampSchedule
	r.IncludeEngines, r.ExcludeEngines = f.IncludeEngines, f.ExcludeEngines
	r.IncludeRegions, r.ExcludeRegions = f.IncludeRegions, f.ExcludeRegions
	r.IncludeTypes, r.ExcludeTypes = f.IncludeTypes, f.ExcludeTypes
}

func applyOverrideFieldsTo(o *config.AccountServiceOverride, f overrideFields) {
	o.Enabled, o.Term, o.Payment, o.Coverage, o.RampSchedule = f.Enabled, f.Term, f.Payment, f.Coverage, f.RampSchedule
	o.IncludeEngines, o.ExcludeEngines = f.IncludeEngines, f.ExcludeEngines
	o.IncludeRegions, o.ExcludeRegions = f.IncludeRegions, f.ExcludeRegions
	o.IncludeTypes, o.ExcludeTypes = f.IncludeTypes, f.ExcludeTypes
}

// normalized maps empty lists to nil, so a stored empty array and an absent
// list compare equal.
func (f overrideFields) normalized() overrideFields {
	for _, l := range []*[]string{&f.IncludeEngines, &f.ExcludeEngines, &f.IncludeRegions, &f.ExcludeRegions, &f.IncludeTypes, &f.ExcludeTypes} {
		if len(*l) == 0 {
			*l = nil
		}
	}
	return f
}

func (f overrideFields) equal(g overrideFields) bool {
	return reflect.DeepEqual(f.normalized(), g.normalized())
}
