package accountpredicate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBuild_PinnedSQL pins the exact clause and bind args for both provider
// gates. The three callers (internal/analytics, internal/api, internal/config)
// produced these strings before the builder moved here (issue #120 item 5);
// each caller also keeps its own pinning test.
func TestBuild_PinnedSQL(t *testing.T) {
	mixed := map[string][]string{"azure": {"sub-1"}, "": {"raw"}, "aws": {"123", "456"}, "gcp": nil}
	tests := []struct {
		name       string
		base       []any
		uuids      []string
		ext        map[string][]string
		match      ProviderMatch
		wantClause string
		wantArgs   []any
	}{
		{name: "no filter keeps args", base: []any{"s"}, match: ProviderExact, wantClause: "", wantArgs: []any{"s"}},
		{name: "providers with no ids keep args", base: []any{"s"}, ext: map[string][]string{"aws": {}, "azure": nil}, match: ProviderExact, wantClause: "", wantArgs: []any{"s"}},
		{name: "no filter, nil base", match: ProviderCaseInsensitiveOrUnknown, wantClause: "", wantArgs: nil},
		{
			name:       "uuids only",
			base:       []any{"s", "e"},
			uuids:      []string{"u1", "u2"},
			match:      ProviderExact,
			wantClause: "(cloud_account_id = ANY($3))",
			wantArgs:   []any{"s", "e", []string{"u1", "u2"}},
		},
		{
			name:  "exact gate, mixed providers sorted",
			base:  []any{"s", "e"},
			uuids: []string{"u1"},
			ext:   mixed,
			match: ProviderExact,
			wantClause: "(cloud_account_id = ANY($3) OR account_id = ANY($4) OR " +
				"(provider = $5 AND account_id = ANY($6)) OR (provider = $7 AND account_id = ANY($8)))",
			wantArgs: []any{"s", "e", []string{"u1"}, []string{"raw"}, "aws", []string{"123", "456"}, "azure", []string{"sub-1"}},
		},
		{
			name:  "case-insensitive gate, mixed providers sorted",
			uuids: []string{"u1"},
			ext:   mixed,
			match: ProviderCaseInsensitiveOrUnknown,
			wantClause: "(cloud_account_id = ANY($1) OR account_id = ANY($2) OR " +
				"((provider = '' OR LOWER(provider) = LOWER($3)) AND account_id = ANY($4)) OR " +
				"((provider = '' OR LOWER(provider) = LOWER($5)) AND account_id = ANY($6)))",
			wantArgs: []any{[]string{"u1"}, []string{"raw"}, "aws", []string{"123", "456"}, "azure", []string{"sub-1"}},
		},
		{
			name:       "provider key is bound as given",
			ext:        map[string][]string{"AWS": {"123"}},
			match:      ProviderCaseInsensitiveOrUnknown,
			wantClause: "(((provider = '' OR LOWER(provider) = LOWER($1)) AND account_id = ANY($2)))",
			wantArgs:   []any{"AWS", []string{"123"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clause, args := Build(tc.base, tc.uuids, tc.ext, tc.match)
			assert.Equal(t, tc.wantClause, clause)
			assert.Equal(t, tc.wantArgs, args)
		})
	}
}

// TestBuild_Deterministic guards the sorted provider order: Go map iteration
// is randomized, so an unsorted builder would fail this within a few runs.
func TestBuild_Deterministic(t *testing.T) {
	ext := map[string][]string{"gcp": {"p"}, "azure": {"s"}, "aws": {"a"}, "oci": {"o"}}
	first, firstArgs := Build(nil, nil, ext, ProviderExact)
	for range 50 {
		clause, args := Build(nil, nil, ext, ProviderExact)
		assert.Equal(t, first, clause)
		assert.Equal(t, firstArgs, args)
	}
}
