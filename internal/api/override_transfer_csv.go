package api

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// overrideCSVColumns is the CSV header, in order. List cells hold
// semicolon-separated values; an empty cell is an unset field.
var overrideCSVColumns = []string{
	"account_id", "account_name", "account_external_id", "provider", "service",
	"enabled", "term", "payment", "coverage", "ramp_schedule",
	"include_engines", "exclude_engines", "include_regions", "exclude_regions",
	"include_types", "exclude_types", "created_at", "updated_at",
}

const overrideCSVListSep = ";"

// utf8BOM is stripped from the first header cell (spreadsheet exports add it).
const utf8BOM = "\uFEFF"

// encodeOverridesCSV writes rows as CSV with a header line.
func encodeOverridesCSV(rows []OverrideTransferRow) (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(overrideCSVColumns); err != nil {
		return "", err
	}
	for i := range rows {
		if err := w.Write(overrideCSVRecord(&rows[i])); err != nil {
			return "", err
		}
	}
	w.Flush()
	return buf.String(), w.Error()
}

// overrideCSVRecord renders one row. Every text cell goes through csvSafeText;
// only the cells this package formats itself (booleans, numbers, timestamps)
// are written as is.
func overrideCSVRecord(r *OverrideTransferRow) []string {
	list := func(l []string) string { return csvSafeText(strings.Join(l, overrideCSVListSep)) }
	return []string{
		csvSafeText(r.AccountID), csvSafeText(r.AccountName), csvSafeText(r.AccountExternalID), csvSafeText(r.Provider), csvSafeText(r.Service),
		formatBoolPtr(r.Enabled), formatIntPtr(r.Term), csvSafeText(derefString(r.Payment)), formatFloatPtr(r.Coverage), csvSafeText(derefString(r.RampSchedule)),
		list(r.IncludeEngines), list(r.ExcludeEngines),
		list(r.IncludeRegions), list(r.ExcludeRegions),
		list(r.IncludeTypes), list(r.ExcludeTypes),
		formatTimePtr(r.CreatedAt), formatTimePtr(r.UpdatedAt),
	}
}

// csvFormulaLeads are the leading characters that make a spreadsheet read a
// cell as a formula (OWASP CSV injection). A leading quote is escaped too, so
// that csvUnescapeText can undo the prefix without guessing.
const csvFormulaLeads = "=+-@\t\r'"

// csvSafeText stops a spreadsheet from running a text cell as a formula (CSV
// injection) by prefixing a quote. Stored override values (ramp_schedule, the
// include/exclude lists, payment) are user-supplied, so every text cell is
// escaped, not only the account name.
func csvSafeText(s string) string {
	if s != "" && strings.ContainsRune(csvFormulaLeads, rune(s[0])) {
		return "'" + s
	}
	return s
}

// csvUnescapeText undoes csvSafeText on import: a quote followed by a
// formula-lead character (or another quote) is the escape prefix and is
// dropped, so an escaped value round-trips to the stored original. A quote
// before anything else is not something csvSafeText writes, so it is kept.
func csvUnescapeText(s string) string {
	if len(s) >= 2 && s[0] == '\'' && strings.ContainsRune(csvFormulaLeads, rune(s[1])) {
		return s[1:]
	}
	return s
}

func formatBoolPtr(p *bool) string {
	if p == nil {
		return ""
	}
	return strconv.FormatBool(*p)
}

func formatIntPtr(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func formatFloatPtr(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func formatTimePtr(p *time.Time) string {
	if p == nil {
		return ""
	}
	return p.UTC().Format(time.RFC3339)
}

// decodeOverridesCSV parses a CSV export back into rows. The header must name
// every column it uses; unknown columns are an error so a typo cannot silently
// drop a field. Column order is free.
func decodeOverridesCSV(text string) ([]OverrideTransferRow, error) {
	r := csv.NewReader(strings.NewReader(text))
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("csv header: %w", err)
	}
	index, err := overrideCSVHeaderIndex(header)
	if err != nil {
		return nil, err
	}
	var rows []OverrideTransferRow
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, fmt.Errorf("csv line %d: %w", line, err)
		}
		row, err := overrideRowFromCSV(rec, index)
		if err != nil {
			return nil, fmt.Errorf("csv line %d: %w", line, err)
		}
		rows = append(rows, row)
	}
}

func overrideCSVHeaderIndex(header []string) (map[string]int, error) {
	known := make(map[string]bool, len(overrideCSVColumns))
	for _, c := range overrideCSVColumns {
		known[c] = true
	}
	index := make(map[string]int, len(header))
	for i, h := range header {
		name := strings.TrimSpace(strings.TrimPrefix(h, utf8BOM))
		if !known[name] {
			return nil, fmt.Errorf("csv header: unknown column %q", name)
		}
		if _, dup := index[name]; dup {
			return nil, fmt.Errorf("csv header: duplicate column %q", name)
		}
		index[name] = i
	}
	return index, nil
}

func overrideRowFromCSV(rec []string, index map[string]int) (OverrideTransferRow, error) {
	cell := func(name string) string {
		if i, ok := index[name]; ok && i < len(rec) {
			return csvUnescapeText(strings.TrimSpace(rec[i]))
		}
		return ""
	}
	row := OverrideTransferRow{
		AccountID: cell("account_id"), AccountExternalID: cell("account_external_id"),
		Provider: cell("provider"), Service: cell("service"),
		Payment: stringPtrOrNil(cell("payment")), RampSchedule: stringPtrOrNil(cell("ramp_schedule")),
		IncludeEngines: splitCSVList(cell("include_engines")), ExcludeEngines: splitCSVList(cell("exclude_engines")),
		IncludeRegions: splitCSVList(cell("include_regions")), ExcludeRegions: splitCSVList(cell("exclude_regions")),
		IncludeTypes: splitCSVList(cell("include_types")), ExcludeTypes: splitCSVList(cell("exclude_types")),
	}
	var err error
	if row.Enabled, err = parseBoolCell(cell("enabled")); err != nil {
		return row, fmt.Errorf("enabled: %w", err)
	}
	if row.Term, err = parseIntCell(cell("term")); err != nil {
		return row, fmt.Errorf("term: %w", err)
	}
	if row.Coverage, err = parseFloatCell(cell("coverage")); err != nil {
		return row, fmt.Errorf("coverage: %w", err)
	}
	return row, nil
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func splitCSVList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, v := range strings.Split(s, overrideCSVListSep) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func parseBoolCell(s string) (*bool, error) {
	if s == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not true or false", s)
	}
	return &b, nil
}

func parseIntCell(s string) (*int, error) {
	if s == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not a whole number", s)
	}
	return &n, nil
}

func parseFloatCell(s string) (*float64, error) {
	if s == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number", s)
	}
	return &f, nil
}
