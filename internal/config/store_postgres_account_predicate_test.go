package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAppendAccountPredicate_ProviderGate pins the external-id half of the
// account predicate (issue #24). A NULL-cloud_account_id purchase_history row
// is reachable only through this half, so its provider gate must:
//   - compare case-insensitively, so a row stored as "AWS" still matches an
//     "aws" filter (and an "AWS" filter key still matches "aws" rows);
//   - let a row with an empty (unknown) stored provider match on account_id,
//     like the "" filter key does;
//   - still bind the provider, so azure/123 never matches an aws/123 filter.
//
// The real-Postgres behavior is covered by
// TestAccountPredicate_ProviderCaseAndEmpty_Integration.
func TestAppendAccountPredicate_ProviderGate(t *testing.T) {
	tests := []struct {
		name      string
		uuids     []string
		ext       map[string][]string
		wantConds []string
		wantArgs  []any
	}{
		{
			name:      "single provider",
			ext:       map[string][]string{"aws": {"123"}},
			wantConds: []string{"(((provider = '' OR LOWER(provider) = LOWER($1)) AND account_id = ANY($2)))"},
			wantArgs:  []any{"aws", []string{"123"}},
		},
		{
			name:      "wrong-case filter key is bound as given and lowered in SQL",
			ext:       map[string][]string{"AWS": {"123"}},
			wantConds: []string{"(((provider = '' OR LOWER(provider) = LOWER($1)) AND account_id = ANY($2)))"},
			wantArgs:  []any{"AWS", []string{"123"}},
		},
		{
			name:  "uuid plus two providers keeps per-provider gates",
			uuids: []string{"uuid-1"},
			ext:   map[string][]string{"azure": {"123"}, "aws": {"123"}},
			wantConds: []string{"(cloud_account_id = ANY($1) OR " +
				"((provider = '' OR LOWER(provider) = LOWER($2)) AND account_id = ANY($3)) OR " +
				"((provider = '' OR LOWER(provider) = LOWER($4)) AND account_id = ANY($5)))"},
			wantArgs: []any{[]string{"uuid-1"}, "aws", []string{"123"}, "azure", []string{"123"}},
		},
		{
			name:      "unknown-provider key has no provider gate",
			ext:       map[string][]string{"": {"123"}},
			wantConds: []string{"(account_id = ANY($1))"},
			wantArgs:  []any{[]string{"123"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conds, args := appendAccountPredicate(nil, nil, tc.uuids, tc.ext)
			assert.Equal(t, tc.wantConds, conds)
			assert.Equal(t, tc.wantArgs, args)
		})
	}
}
