// Package accountpredicate builds the dual-column account WHERE fragment
// shared by every read path that scopes savings_snapshots or purchase_history
// rows to a set of cloud accounts (issue #120). Keeping the one builder here
// means a predicate fix is a single edit instead of three that can drift.
//
// Both tables carry two account identifiers, either of which may be the only
// one populated on a row: cloud_account_id (the cloud_accounts UUID FK, NULL
// on the AWS ambient and legacy rows) and account_id (the cloud-provider
// external number). Matching only cloud_account_id silently dropped every NULL
// row (issues #701/#498) and matching only account_id dropped UUID-only rows
// (#866), so the predicate ORs both:
//
//	(cloud_account_id = ANY($u)
//	   OR <provider gate for p1> AND account_id = ANY($e1)
//	   OR ...)
//
// The external-id half is grouped per provider so each external number only
// matches rows of its own provider, preserving the (provider, external_id)
// pairing: a filter for aws/123 never pulls azure/123 rows. The "" provider key
// (legacy raw external number, unknown provider) matches account_id with no
// provider gate. cloud_account_id is compared directly (no ::text cast) so the
// plain cloud_account_id UUID index (migration 000011) can back it. Providers
// are sorted so the SQL and its bind order are deterministic.
package accountpredicate

import (
	"fmt"
	"sort"
	"strings"
)

// ProviderMatch selects how the per-provider gate compares the stored
// provider column with the filter's provider key.
type ProviderMatch int

const (
	// ProviderExact gates on `provider = $p`. Used by the analytics reads of
	// savings_snapshots and purchase_history.
	ProviderExact ProviderMatch = iota

	// ProviderCaseInsensitiveOrUnknown gates on
	// `(provider = '' OR LOWER(provider) = LOWER($p))`, so a row stored as
	// "AWS" still matches an "aws" filter and a row with an empty (unknown)
	// stored provider matches on account_id alone, like the "" filter key
	// does (issue #24). A row stored under a different, non-empty provider is
	// still excluded. Used by the internal/config purchase_history reads.
	ProviderCaseInsensitiveOrUnknown
)

// Build appends the account binds to args and returns the parenthesized OR
// clause that references them by position, plus the extended args. When
// neither input yields a disjunct (both empty, or every provider key maps to
// no IDs) it returns an empty clause and args unchanged; each caller decides
// what "no account filter" means for its query.
func Build(args []any, accountUUIDs []string, externalIDsByProvider map[string][]string, match ProviderMatch) (clause string, outArgs []any) {
	var ors []string
	if len(accountUUIDs) > 0 {
		args = append(args, accountUUIDs)
		ors = append(ors, fmt.Sprintf("cloud_account_id = ANY($%d)", len(args)))
	}
	for _, provider := range sortedProviderKeys(externalIDsByProvider) {
		exts := externalIDsByProvider[provider]
		if len(exts) == 0 {
			continue
		}
		if provider == "" {
			args = append(args, exts)
			ors = append(ors, fmt.Sprintf("account_id = ANY($%d)", len(args)))
			continue
		}
		args = append(args, provider)
		providerArg := len(args)
		args = append(args, exts)
		ors = append(ors, fmt.Sprintf(match.gateFormat(), providerArg, len(args)))
	}
	if len(ors) == 0 {
		return "", args
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// gateFormat returns the per-provider disjunct with two %d verbs: the
// provider bind position, then the external-ID array bind position.
func (m ProviderMatch) gateFormat() string {
	if m == ProviderCaseInsensitiveOrUnknown {
		return "((provider = '' OR LOWER(provider) = LOWER($%d)) AND account_id = ANY($%d))"
	}
	return "(provider = $%d AND account_id = ANY($%d))"
}

// sortedProviderKeys returns the map keys in ascending order so the generated
// SQL (and its bind-arg ordering) is deterministic across calls and testable.
func sortedProviderKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
