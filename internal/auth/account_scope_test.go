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
	s := ScopeForAccounts([]string{"acct-A", "uuid-prod"}).WithRegisteredAccounts([]RegisteredAccount{
		{ID: "uuid-prod", Name: "prod-name", ExternalID: "ext-123"}, {ID: "uuid-other", Name: "other-name", ExternalID: "ext-456"},
	})

	assert.True(t, s.Allows("acct-A", ""), "matches by ID")
	assert.True(t, s.Allows("ext-123", "prod-name"), "a unique name identifies an account whose ID is in scope")
	assert.False(t, s.Allows("ext-456", "prod-name"), "but only for that account's own external ID")
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

// AllowsAll agrees with the legacy IsUnrestrictedAccess convention for every
// resolved list, so the type cannot silently change who is unrestricted.
func TestAccountScope_AllowsAllParityWithLegacyConvention(t *testing.T) {
	for _, legacy := range [][]string{
		nil, {}, {"*"}, {"acct-A"}, {"acct-A", "acct-B"}, {"prod-name"}, {"acct-A", "*"},
	} {
		s := ScopeFromLegacyList(legacy)
		assert.Equal(t, IsUnrestrictedAccess(legacy), s.AllowsAll(),
			"AllowsAll must agree with IsUnrestrictedAccess for %v", legacy)
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

// Issue #1034: a scope entry is bound to an account ID. A name entry is not
// bound to anything, so once the account it was meant for is deleted or
// renamed it would grant whichever account takes the name next. It must
// therefore match no account, even when the name is unique.
func TestAccountScope_NameEntryNeverMatches(t *testing.T) {
	// The account the grant was meant for is gone; a new account took its name.
	later := []RegisteredAccount{{ID: "uuid-new", Name: "prod"}}
	s := ScopeForAccounts([]string{"prod"}).WithRegisteredAccounts(later)

	assert.False(t, s.Allows("uuid-new", "prod"), "a later account reusing the name must not be granted")
	assert.False(t, s.Allows("uuid-new", ""))
	assert.False(t, s.Allows("ext-new", "prod"), "nor when the caller knows it by external ID and name")
	assert.Empty(t, s.FilterIDs([]string{"uuid-new"}))
}

// The same grant made by ID survives the rename: the renamed account keeps
// access, the account that takes the old name does not.
func TestAccountScope_IDEntrySurvivesRenameAndReuse(t *testing.T) {
	afterRename := []RegisteredAccount{
		{ID: "uuid-orig", Name: "prod-old", ExternalID: "ext-orig"}, // renamed away from "prod"
		{ID: "uuid-new", Name: "prod", ExternalID: "ext-new"},       // took the old name
	}
	s := ScopeForAccounts([]string{"uuid-orig"}).WithRegisteredAccounts(afterRename)

	assert.True(t, s.Allows("uuid-orig", "prod-old"))
	assert.True(t, s.Allows("ext-orig", "prod-old"), "the renamed account is still identified by its new name")
	assert.False(t, s.Allows("uuid-new", "prod"), "the account that took the old name is not granted")
	assert.False(t, s.Allows("ext-new", "prod"))
}

// Issue #433: display names are not unique. A shared name identifies no
// account, so it cannot be used to reach either of them.
func TestAccountScope_SharedNameIdentifiesNoAccount(t *testing.T) {
	registered := []RegisteredAccount{
		{ID: "uuid-aws-prod", Name: "prod", ExternalID: "ext-aws"},
		{ID: "uuid-az-prod", Name: "prod", ExternalID: "ext-az"},
		{ID: "uuid-solo", Name: "solo", ExternalID: "ext-solo"},
	}
	s := ScopeForAccounts([]string{"uuid-aws-prod", "uuid-solo"}).WithRegisteredAccounts(registered)

	assert.True(t, s.Allows("uuid-aws-prod", "prod"), "an ID entry grants its account whatever the name")
	assert.False(t, s.Allows("ext-az", "prod"), "a shared name must not resolve to the in-scope account")
	assert.False(t, s.Allows("uuid-az-prod", "prod"), "nor grant the other account with that name")
	assert.True(t, s.Allows("ext-solo", "solo"), "a unique name still identifies its account")
	assert.False(t, s.Allows("uuid-other", "other"))
	assert.Equal(t, []string{"uuid-aws-prod"}, s.FilterIDs([]string{"uuid-aws-prod", "uuid-az-prod"}))

	// Whichever copy is in scope, the shared name identifies neither.
	for _, id := range []string{"uuid-aws-prod", "uuid-az-prod"} {
		assert.False(t, ScopeForAccounts([]string{id}).WithRegisteredAccounts(registered).Allows("ext-aws", "prod"),
			"shared name must not resolve to %s", id)
	}
}

// A name only identifies the account carrying it. Naming an account after an
// in-scope account's ID gains that account nothing.
func TestAccountScope_NameResolvesToTheAccountCarryingIt(t *testing.T) {
	registered := []RegisteredAccount{
		{ID: "uuid-in", Name: "in-scope"},
		{ID: "uuid-attacker", Name: "uuid-in"}, // display name set to the in-scope ID
	}
	s := ScopeForAccounts([]string{"uuid-in"}).WithRegisteredAccounts(registered)

	assert.False(t, s.Allows("uuid-attacker", "uuid-in"),
		"a display name equal to an in-scope ID must not grant the account carrying it")
	assert.True(t, s.Allows("uuid-in", "in-scope"))
}

// Name lookup is opt-in: a scope that never learned the registered accounts
// (or learned an empty list) is ID-only, so forgetting the step fails closed.
func TestAccountScope_NameLookupRequiresRegisteredAccounts(t *testing.T) {
	s := ScopeForAccounts([]string{"acct-A", "uuid-prod"})
	assert.False(t, s.Allows("ext-prod", "prod"), "no registered accounts: ID only")
	assert.True(t, s.Allows("acct-A", ""))

	assert.False(t, s.WithRegisteredAccounts(nil).Allows("ext-prod", "prod"))
	assert.False(t, s.WithRegisteredAccounts([]RegisteredAccount{{ID: "uuid-x", Name: "other"}}).Allows("ext-prod", "prod"),
		"a name no registered account carries identifies nothing")

	var zero AccountScope
	assert.False(t, zero.WithRegisteredAccounts([]RegisteredAccount{{ID: "uuid-prod", Name: "prod"}}).Allows("ext-prod", "prod"),
		"the zero value still denies")
}

func TestAccountScope_WithRegisteredAccountsEdgeCases(t *testing.T) {
	dup := []RegisteredAccount{{ID: "u1", Name: "a"}, {ID: "u2", Name: "a"}}
	assert.True(t, UnrestrictedScope().WithRegisteredAccounts(dup).Allows("x", "a"),
		"unrestricted stays unrestricted")

	s := ScopeForAccounts([]string{""}).WithRegisteredAccounts([]RegisteredAccount{{ID: "", Name: ""}})
	assert.False(t, s.Allows("", ""), "an empty entry never matches")
	assert.False(t, s.Allows("uuid", ""))

	base := ScopeForAccounts([]string{"uuid-prod"})
	_ = base.WithRegisteredAccounts([]RegisteredAccount{{ID: "uuid-prod", Name: "prod"}})
	assert.False(t, base.Allows("ext-prod", "prod"), "WithRegisteredAccounts returns a copy")

	exact := ScopeForAccounts([]string{"uuid-P"}).WithRegisteredAccounts([]RegisteredAccount{
		{ID: "uuid-P", Name: "Prod", ExternalID: "ext"}, {ID: "uuid-p", Name: "prod", ExternalID: "ext-lower"},
	})
	assert.True(t, exact.Allows("ext", "Prod"), "names are compared exactly")
	assert.False(t, exact.Allows("ext", "prod"))
}

// rE7 on #1116: the name and the identifier a caller passes can come from
// different accounts. External IDs are free text, so an editor of in-scope
// account Z can set Z's external ID to victim Y's internal ID; a lookup keyed
// by identifier then hands back Z's name for Y. The name must only ever
// identify the account the identifier denotes, so this is refused.
func TestAccountScope_NameMustIdentifyTheAccountOfTheIdentifier(t *testing.T) {
	registered := []RegisteredAccount{
		{ID: "uuid-Y", Name: "prod-payments", ExternalID: "123456789012"},
		{ID: "uuid-Z", Name: "zz-team", ExternalID: "uuid-Y"}, // Z's external ID set to Y's ID
	}
	s := ScopeForAccounts([]string{"uuid-Z"}).WithRegisteredAccounts(registered)

	assert.False(t, s.Allows("uuid-Y", "zz-team"), "Z's name must not grant Y through Z's external ID")
	assert.False(t, s.Allows("uuid-Y", "prod-payments"))
	assert.False(t, s.Allows("123456789012", "zz-team"), "a name of another account never binds")
	assert.True(t, s.Allows("uuid-Z", "zz-team"), "control: Z itself is still granted")
}

// External IDs are unique per provider only, so two accounts can share one.
// The caller resolves which account a record means (within the record's
// provider) and passes that account's name; the name then binds only to its
// own account, never to the other account carrying the same external ID.
func TestAccountScope_SharedExternalIDBindsOnlyTheNamedAccount(t *testing.T) {
	registered := []RegisteredAccount{
		{ID: "uuid-Y", Name: "prod-payments", ExternalID: "123456789012"},
		{ID: "uuid-Z", Name: "zz-team", ExternalID: "123456789012"}, // another provider
	}
	inY := ScopeForAccounts([]string{"uuid-Y"}).WithRegisteredAccounts(registered)
	assert.True(t, inY.Allows("123456789012", "prod-payments"), "Y's own record")
	assert.False(t, inY.Allows("123456789012", "zz-team"), "Z's record is not Y's")

	inZ := ScopeForAccounts([]string{"uuid-Z"}).WithRegisteredAccounts(registered)
	assert.False(t, inZ.Allows("123456789012", "prod-payments"), "Y's record is not Z's")
	assert.False(t, inZ.Allows("123456789012", ""), "without a name the shared number identifies no account")
}
