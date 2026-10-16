package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The defining property of AccountScope (issue #1748): THE ZERO VALUE DENIES.
//
// This is the reason the type exists. The untyped []string it replaces used
// the same value -- empty -- for "no restriction configured" and for "we could
// not establish your scope", so every resolution failure silently granted
// access to every account.
//
// If someone later inverts the flag to `Restricted bool` because it reads
// better, the zero value starts granting everything and the bug returns with
// the compiler's blessing. These tests are the barrier.

func TestAccountScope_ZeroValueDeniesEverything(t *testing.T) {
	var zero AccountScope

	assert.False(t, zero.AllowsAll(), "the zero value must NOT be unrestricted")
	assert.False(t, zero.Allows("any-account-id", ""), "the zero value must allow no account")
	assert.False(t, zero.Allows("any-account-id", "any-name"))
	assert.Empty(t, zero.FilterIDs([]string{"a", "b", "c"}),
		"the zero value must filter every account out")
	assert.Equal(t, "no accounts", zero.String())
}

// A struct returned on an error path, or one whose fields a later change fails
// to populate, must deny. Same property, stated the way it will actually be
// encountered.
func TestAccountScope_UninitialisedOnErrorPathDenies(t *testing.T) {
	failing := func() (AccountScope, error) {
		var s AccountScope // deliberately not populated
		return s, assert.AnError
	}
	s, err := failing()
	require.Error(t, err)
	assert.False(t, s.Allows("acct-A", ""), "a scope returned alongside an error must not grant access")
}

func TestAccountScope_UnrestrictedMustBeSetPositively(t *testing.T) {
	assert.True(t, UnrestrictedScope().AllowsAll())
	assert.True(t, UnrestrictedScope().Allows("literally-anything", ""))
	assert.Equal(t, []string{"a", "b"}, UnrestrictedScope().FilterIDs([]string{"a", "b"}))
	assert.Equal(t, "all accounts", UnrestrictedScope().String())
}

// ScopeForAccounts with an EMPTY list grants nothing -- the opposite of the
// legacy []string convention. Pinned because this inversion is the whole point
// and is exactly what a careless migration would get backwards.
func TestAccountScope_EmptyExplicitListGrantsNothing(t *testing.T) {
	s := ScopeForAccounts(nil)
	assert.False(t, s.AllowsAll(), "an empty explicit list is NOT unrestricted")
	assert.False(t, s.Allows("acct-A", ""))

	s2 := ScopeForAccounts([]string{})
	assert.False(t, s2.AllowsAll())
	assert.False(t, s2.Allows("acct-A", ""))
}

func TestAccountScope_RestrictedAllowsOnlyItsOwn(t *testing.T) {
	s := ScopeForAccounts([]string{"acct-A", "prod-name"}).WithRegisteredNames([]string{"prod-name", "other-name"})

	assert.True(t, s.Allows("acct-A", ""), "matches by ID")
	assert.True(t, s.Allows("some-uuid", "prod-name"), "matches by a unique display name")
	assert.False(t, s.Allows("acct-B", ""), "an account outside the list is denied")
	assert.False(t, s.Allows("acct-B", "other-name"))
	assert.False(t, s.AllowsAll())
	assert.Equal(t, []string{"acct-A"}, s.FilterIDs([]string{"acct-A", "acct-B"}))
}

// ScopeFromLegacyList preserves the historical convention, and is safe only
// for an already-successful resolution. Both spellings of unrestricted must
// convert to a positively-set flag rather than to an empty restricted scope.
func TestAccountScope_FromLegacyList(t *testing.T) {
	for _, tc := range []struct {
		name             string
		in               []string
		wantUnrestricted bool
	}{
		{"nil means unrestricted under the legacy convention", nil, true},
		{"empty means unrestricted under the legacy convention", []string{}, true},
		{"wildcard means unrestricted", []string{"*"}, true},
		{"wildcard among others still means unrestricted", []string{"acct-A", "*"}, true},
		{"a real list stays restricted", []string{"acct-A"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ScopeFromLegacyList(tc.in)
			assert.Equal(t, tc.wantUnrestricted, s.AllowsAll())
			if tc.wantUnrestricted {
				assert.True(t, s.Allows("anything", ""))
			} else {
				assert.True(t, s.Allows("acct-A", ""))
				assert.False(t, s.Allows("acct-B", ""))
			}
		})
	}
}

// The migration must not accidentally promote a scoped principal to
// unrestricted -- the control the team lead asked for explicitly.
func TestAccountScope_ScopedPrincipalStaysLimited(t *testing.T) {
	s := ScopeFromLegacyList([]string{"acct-A", "acct-B"})

	require.False(t, s.AllowsAll(), "a genuinely scoped principal must not become unrestricted")
	assert.True(t, s.Allows("acct-A", ""))
	assert.True(t, s.Allows("acct-B", ""))
	assert.False(t, s.Allows("acct-C", ""))
	assert.Equal(t, []string{"acct-A"}, s.FilterIDs([]string{"acct-A", "acct-C"}))
}

// Behavioral parity with the helpers it replaces, so the migration cannot
// silently change who can reach what.
func TestAccountScope_ParityWithLegacyHelpers(t *testing.T) {
	for _, legacy := range [][]string{
		nil, {}, {"*"}, {"acct-A"}, {"acct-A", "acct-B"}, {"prod-name"},
	} {
		// Every probe name is unique among the registered accounts, which is
		// the only case where the name match is unchanged (issue #433).
		s := ScopeFromLegacyList(legacy).WithRegisteredNames([]string{"prod-name", "nope"})
		assert.Equal(t, IsUnrestrictedAccess(legacy), s.AllowsAll(),
			"AllowsAll must agree with IsUnrestrictedAccess for %v", legacy)
		for _, probe := range []struct{ id, name string }{
			{"acct-A", ""}, {"acct-B", ""}, {"some-uuid", "prod-name"}, {"acct-C", "nope"},
		} {
			assert.Equal(t, MatchesAccount(legacy, probe.id, probe.name), s.Allows(probe.id, probe.name),
				"Allows must agree with MatchesAccount for legacy=%v probe=%v", legacy, probe)
		}
	}
}

// ScopeFromLegacyList's safety is entirely borrowed from the resolver, so pin
// the boundary: it maps BOTH spellings of empty to unrestricted, which is
// correct only for inputs the resolver has already vetted.
//
// This is the test that would have caught the type change carrying #1748
// forward: if the resolver stopped refusing a widened empty union, this
// conversion would silently produce UnrestrictedScope() from it.
func TestAccountScope_LegacyConversionIsBorrowedSafety(t *testing.T) {
	assert.True(t, ScopeFromLegacyList(nil).AllowsAll(),
		"empty means unrestricted under the legacy convention -- safe ONLY post-resolver")
	assert.True(t, ScopeFromLegacyList([]string{}).AllowsAll())

	// The corollary: a caller that has NOT been through the resolver must not
	// use this. ScopeForAccounts is the safe constructor for raw input, and it
	// treats empty as "nothing" rather than "everything".
	assert.False(t, ScopeForAccounts([]string{}).AllowsAll(),
		"the raw-input constructor must NOT inherit the legacy empty-means-all rule")
	assert.False(t, ScopeForAccounts(nil).Allows("acct-A", ""))
}

// Issue #433: display names are not unique. A name entry matches only when
// exactly one registered account carries that name.
func TestAccountScope_SharedNameMatchesNoAccountByName(t *testing.T) {
	registered := []string{"prod", "prod", "solo"} // aws "prod", azure "prod", gcp "solo"
	s := ScopeForAccounts([]string{"prod", "solo"}).WithRegisteredNames(registered)

	assert.False(t, s.Allows("uuid-aws-prod", "prod"), "a shared name must not grant the first account")
	assert.False(t, s.Allows("uuid-az-prod", "prod"), "nor the other account with the same name")
	assert.True(t, s.Allows("uuid-solo", "solo"), "a unique name still matches")
	assert.False(t, s.Allows("uuid-other", "other"))
}

func TestAccountScope_IDEntriesIgnoreNameAmbiguity(t *testing.T) {
	s := ScopeForAccounts([]string{"uuid-aws-prod"}).WithRegisteredNames([]string{"prod", "prod"})

	assert.True(t, s.Allows("uuid-aws-prod", "prod"), "an ID entry grants its account whatever the name")
	assert.True(t, s.Allows("uuid-aws-prod", ""))
	assert.False(t, s.Allows("uuid-az-prod", "prod"))
	assert.Equal(t, []string{"uuid-aws-prod"}, s.FilterIDs([]string{"uuid-aws-prod", "uuid-az-prod"}))
}

// Name matching is opt-in: a scope that never learned the registered names
// (or learned an empty list) is ID-only, so forgetting the step fails closed.
func TestAccountScope_NameMatchRequiresRegisteredNames(t *testing.T) {
	s := ScopeForAccounts([]string{"acct-A", "prod"})
	assert.False(t, s.Allows("uuid-prod", "prod"), "no registered names: ID only")
	assert.True(t, s.Allows("acct-A", ""))

	assert.False(t, s.WithRegisteredNames(nil).Allows("uuid-prod", "prod"))
	assert.False(t, s.WithRegisteredNames([]string{"other"}).Allows("uuid-prod", "prod"),
		"a name no registered account carries does not match")

	var zero AccountScope
	assert.False(t, zero.WithRegisteredNames([]string{"prod"}).Allows("uuid-prod", "prod"),
		"the zero value still denies")
}

func TestAccountScope_WithRegisteredNamesEdgeCases(t *testing.T) {
	assert.True(t, UnrestrictedScope().WithRegisteredNames([]string{"a", "a"}).Allows("x", "a"),
		"unrestricted stays unrestricted")

	s := ScopeForAccounts([]string{""}).WithRegisteredNames([]string{""})
	assert.False(t, s.Allows("uuid", ""), "an empty name never matches")

	base := ScopeForAccounts([]string{"prod"})
	_ = base.WithRegisteredNames([]string{"prod"})
	assert.False(t, base.Allows("uuid", "prod"), "WithRegisteredNames returns a copy")

	assert.True(t, ScopeForAccounts([]string{"Prod"}).WithRegisteredNames([]string{"Prod", "prod"}).Allows("uuid", "Prod"),
		"names are compared exactly, as entries are")
}
