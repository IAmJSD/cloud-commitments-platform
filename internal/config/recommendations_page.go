package config

// Types of the SQL-side paginated recommendations read (issue #981). A
// paginated GET /api/recommendations used to read every filtered row into Go
// to total the summary; PageStoredRecommendations instead evaluates every
// list filter in SQL, totals the summary in one aggregate query and returns
// only the requested page.

// RecommendationListScope is the row filter of one recommendations list
// request: the query-string filter plus the handler-side filters, all applied
// to the decoded payload the way the Go filters see it.
type RecommendationListScope struct {
	Filter RecommendationFilter
	// EnabledProviders keeps only rows whose provider is listed. Empty keeps
	// every provider (FilterRecommendationsByEnabledProviders).
	EnabledProviders []string
	// Restricted keeps only rows whose cloud_account_id is one of
	// AllowedAccountIDs; rows without an account are dropped. The caller
	// resolves the account scope to the exact set of IDs it allows.
	Restricted        bool
	AllowedAccountIDs []string
}

// RecommendationTriple is an (account, provider, service) combination of the
// stored rows, the key account overrides are resolved by. AccountID is ""
// when the payload has no cloud_account_id; HasAccount tells that apart from
// a present but empty one, which override resolution treats differently.
type RecommendationTriple struct {
	AccountID  string
	Provider   string
	Service    string
	HasAccount bool
}

// RecommendationOverrideRule is the resolved account-override configuration
// of one AccountConfigKey. Config is nil when neither a global service
// configuration nor an account override exists, so the rows pass unfiltered.
// MinCountExempt is set for Savings Plans services, whose count is not an
// instance count.
type RecommendationOverrideRule struct {
	Key            string
	Config         *ServiceConfig
	MinCountExempt bool
}

// StoredRecommendation is a decoded cache row with its table primary key.
type StoredRecommendation struct {
	RowID string
	Rec   RecommendationRecord
}

// RecommendationPageQuery parameterizes PageStoredRecommendations.
//
// Rules must hold an entry for every AccountConfigKey the scope's rows can
// produce (ListStoredRecommendationTriples); a row whose key has no entry
// makes the page fall back, because its overrides were never resolved.
//
// SuppressionKeys are the suppression 6-tuples (see SuppressionTupleKey)
// with every stored spelling of the service. Rows on one of them are read
// first and handed to DecideSuppressed, which returns the primary keys of
// the rows that survive suppressions and overrides; the other rows are
// filtered by Rules in SQL.
type RecommendationPageQuery struct {
	Scope            RecommendationListScope
	Rules            []RecommendationOverrideRule
	SuppressionKeys  []string
	DecideSuppressed func(rows []StoredRecommendation) (keepRowIDs []string, err error)
	// AfterID returns only rows whose ID sorts byte-wise after it; "" starts
	// at the first row.
	AfterID string
	// Limit caps the returned rows.
	Limit int
}

// RecommendationPage is one page of the filtered recommendations with the
// summary of the whole filtered set. TotalSavings and TotalUpfront are summed
// in ID order, the order the Go summary added them in. Regions holds each
// non-empty region once.
//
// Fallback is non-empty when SQL cannot reproduce the Go filters for this
// read (for example a value whose float arithmetic SQL would reject, or a
// row whose overrides were not resolved); the caller must then serve the
// request from the Go-side scan, and the other fields are unset.
type RecommendationPage struct {
	TotalCount   int
	TotalSavings float64
	TotalUpfront float64
	Regions      []string
	Rows         []RecommendationRecord
	Fallback     string
}

// SuppressionTupleKey joins a suppression 6-tuple into the string
// PageStoredRecommendations matches rows against. Two distinct tuples can
// only produce the same key when a value contains the separator; such a row
// is merely decided in Go instead of SQL, which is still exact.
func SuppressionTupleKey(accountID, provider, service, region, resourceType, engine string) string {
	const sep = "\x1f"
	return accountID + sep + provider + sep + service + sep + region + sep + resourceType + sep + engine
}

// SuppressionServiceSpellings returns every service spelling a stored rec
// can carry that CanonicalSuppressionService maps to canonical for provider:
// canonical itself (when it maps to itself) and its legacy and dash-free
// aliases.
func SuppressionServiceSpellings(provider, canonical string) []string {
	var out []string
	if CanonicalSuppressionService(provider, canonical) == canonical {
		out = append(out, canonical)
	}
	if provider != "aws" {
		return out
	}
	for _, aliases := range []map[string]string{awsLegacyServiceSlugs, awsSavingsPlansSpellings} {
		for alias := range aliases {
			if alias != canonical && CanonicalSuppressionService(provider, alias) == canonical {
				out = append(out, alias)
			}
		}
	}
	return out
}
