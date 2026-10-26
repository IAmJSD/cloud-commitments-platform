package auth

import "strings"

// AccountScope is the set of cloud accounts a principal may access.
//
// # The zero value MUST deny. This is a requirement, not an observation.
//
// AccountScope{} is {Unrestricted: false, Accounts: nil} -- restricted to
// nothing, granting access to no account at all. That is deliberate and
// load-bearing: a forgotten initialisation, a value returned on an error path,
// or a field a later change fails to populate all fail CLOSED.
//
// Do NOT invert the flag to `Restricted bool` for readability. That reads
// marginally better and is the entire bug this type exists to remove: it would
// make the zero value grant everything, so absence would once again mean
// unrestricted -- with a compiler now blessing it. Issue #1748 is precisely
// that failure in its untyped form: `[]string` used both for "no restriction
// configured" and for "we could not work out your scope", so every resolution
// failure silently granted access to every account.
//
// Unrestricted must therefore always be set POSITIVELY by a caller that has
// established the principal genuinely has unlimited access -- the stateless
// admin API key, or a group carrying the "*" wildcard.
type AccountScope struct {
	// Accounts is the explicit allow-list, meaningful only when
	// Unrestricted is false. Entries are account identifiers (the cloud
	// account ID, or a provider external ID) and are compared with the
	// identifier the caller passes, never with a display name (issue #1034).
	Accounts []string

	// byUniqueName maps each display name carried by exactly one registered
	// cloud account to that account. It lets a caller that knows an account by
	// its external ID (for example a legacy row) identify it: Allows resolves
	// the name to the account carrying it, checks that the identifier the
	// caller passed really is that account's external ID, and then checks the
	// account's ID against Accounts. A name shared by two accounts resolves to
	// neither (issue #433). nil (the zero value, and every scope built without
	// WithRegisteredAccounts) disables the lookup, so forgetting to populate
	// it fails closed.
	byUniqueName map[string]RegisteredAccount

	// internalIDs holds the ID of every registered account. External IDs are
	// free text an account editor sets, so one can equal another account's
	// ID; the name lookup never treats an internal ID as an external ID.
	internalIDs map[string]struct{}

	// Unrestricted grants access to every account. Set it only where
	// unlimited access has been positively established.
	Unrestricted bool
}

// RegisteredAccount is the identity of one registered cloud account, as
// WithRegisteredAccounts needs it.
type RegisteredAccount struct {
	ID         string
	Name       string
	ExternalID string
}

// UnrestrictedScope returns a scope granting every account. Use it only at a
// site that has positively established unlimited access; never as a fallback
// or a default.
func UnrestrictedScope() AccountScope {
	return AccountScope{Unrestricted: true}
}

// ScopeForAccounts returns a scope limited to the given accounts, treating
// every entry as a LITERAL account identifier.
//
// Two asymmetries with the legacy []string convention, and both invert its
// meaning rather than merely differing from it:
//
//   - An empty list grants NOTHING here; under the legacy convention empty
//     meant "all accounts".
//   - "*" is a literal identifier here, NOT a wildcard. ScopeForAccounts(["*"])
//     matches an account whose ID is the single character "*" and
//     denies everything else, where ScopeFromLegacyList(["*"]) is unrestricted.
//
// The second matters for the obvious future call. Passing a group's stored
// list straight through -- ScopeForAccounts(group.AllowedAccounts) -- silently
// produces a scope that DENIES EVERYTHING for any wildcard-carrying group, and
// all seven seeded groups ship allowed_accounts = ARRAY['*']. Use
// ScopeFromLegacyList for a value that came out of the resolver, and this
// constructor only for a list you have already decided is literal.
func ScopeForAccounts(accounts []string) AccountScope {
	return AccountScope{Accounts: accounts}
}

// ScopeFromLegacyList converts a resolved []string using the historical
// convention where empty or a "*" entry means unrestricted.
//
// ScopeFromLegacyList(nil) and ScopeFromLegacyList([]) both return
// UnrestrictedScope(). That is deliberate and it is also the dangerous part,
// so be precise about what makes it safe.
//
// This function CANNOT distinguish "no restriction configured" from "we could
// not work out this principal's scope" -- they are the same empty value, which
// is issue #1748 in its entirety. It therefore carries no safety of its own.
// It is safe only because Service.ResolveAllowedAccounts has ALREADY refused
// every input that would make empty mean the wrong thing:
//
//   - a store error, or a missing user row -> error, never reaches here
//   - every group unresolvable -> error
//   - SOME group unresolvable AND the surviving union empty -> error
//
// That third condition is the one to keep in view. An earlier version of the
// resolver refused only total failure, and under it this function WOULD have
// converted a widened empty union into UnrestrictedScope() -- carrying the bug
// forward inside the new type, with the compiler blessing it. The guarantee
// this depends on is not "the resolver returns successfully"; it is "the
// resolver refuses any empty union that a skipped group could have caused".
//
// Do not call this on a []string from any other source. If a new producer
// appears, give it the same guarantee first or build the AccountScope
// directly with ScopeForAccounts / UnrestrictedScope.
func ScopeFromLegacyList(accounts []string) AccountScope {
	if IsUnrestrictedAccess(accounts) {
		return UnrestrictedScope()
	}
	return ScopeForAccounts(accounts)
}

// WithRegisteredAccounts returns a copy of the scope that can identify an
// account by its display name. accounts must hold EVERY registered cloud
// account (all providers, enabled or not), with its ID, name and external ID,
// so that a shared name is visible as shared and no account's ID can pass for
// another account's external ID. Only names that exactly one account carries
// are kept.
//
// This never makes a scope entry match by name. Entries are account IDs, and
// a stored name could otherwise outlive its account and grant whichever
// account later takes that name (issue #1034). The name only tells Allows
// which account the caller means; the scope must still hold that account's
// ID. Pass a partial list and a name could look unique when it is not, so
// callers that cannot load the full list must leave the scope without it.
func (s AccountScope) WithRegisteredAccounts(accounts []RegisteredAccount) AccountScope {
	if s.Unrestricted {
		return s
	}
	counts := make(map[string]int, len(accounts))
	byName := make(map[string]RegisteredAccount, len(accounts))
	s.internalIDs = make(map[string]struct{}, len(accounts))
	for _, a := range accounts {
		if a.Name != "" {
			counts[a.Name]++
			byName[a.Name] = a
		}
		s.internalIDs[a.ID] = struct{}{}
	}
	s.byUniqueName = make(map[string]RegisteredAccount, len(counts))
	for n, c := range counts {
		if c == 1 && byName[n].ID != "" {
			s.byUniqueName[n] = byName[n]
		}
	}
	return s
}

// Allows reports whether the scope permits the given cloud account. The
// account matches when accountID is in the allow-list, or when the scope was
// built with WithRegisteredAccounts and the name identifies the same account
// as accountID: exactly one registered account carries accountName, accountID
// is THAT account's external ID and no registered account's ID, and the
// account's ID is in the allow-list. Pass "" for the name when unavailable.
//
// The name is bound to the identifier because the two can come from different
// accounts. External IDs are free text, so an editor of an in-scope account
// can set its external ID to another account's ID; a caller that then looks
// the name up by that identifier gets the in-scope account's name for the
// other account. Without the binding the scope would grant the other account.
// The decision is therefore always on the ID of the account accountID
// denotes, never on the name alone.
//
// External IDs are unique per provider only, so the scope cannot tell which
// provider's account an external ID means. A caller that passes a name for an
// external ID must have resolved it unambiguously: within the record's
// provider, or across all providers only when exactly one account carries it
// (see historyRowAccountName and uniqueAccountByExternalID in package api).
// A caller that can resolve the account itself should pass its ID instead.
//
// A display name in the allow-list matches nothing: it is not bound to any
// account, so it would grant whichever account carries the name today
// (issue #1034). Group writes store IDs (see the group handlers), and
// migration 000145 rewrote the name entries that still identified one
// account.
//
// A restricted scope with no accounts allows nothing, which is what makes the
// zero value deny.
func (s AccountScope) Allows(accountID, accountName string) bool {
	if s.Unrestricted {
		return true
	}
	if s.contains(accountID) {
		return true
	}
	if accountName == "" {
		return false
	}
	named, ok := s.byUniqueName[accountName]
	return ok && s.identifies(named, accountID) && s.contains(named.ID)
}

// identifies reports whether id denotes the registered account a through a's
// external ID: it equals that external ID and is not the ID of any registered
// account. a's own ID needs no check here, because Allows has already matched
// the identifier itself against the allow-list.
func (s AccountScope) identifies(a RegisteredAccount, id string) bool {
	if id == "" || id != a.ExternalID {
		return false
	}
	_, isInternalID := s.internalIDs[id]
	return !isInternalID
}

// contains reports whether id is a non-empty entry of the allow-list.
func (s AccountScope) contains(id string) bool {
	if id == "" {
		return false
	}
	for _, a := range s.Accounts {
		if a == id {
			return true
		}
	}
	return false
}

// AllowsAll reports whether the scope is unrestricted. Prefer Allows; this
// exists for the few sites that must branch on unrestricted-ness itself, such
// as skipping a filter entirely.
func (s AccountScope) AllowsAll() bool {
	return s.Unrestricted
}

// FilterIDs returns the subset of ids the scope permits. An unrestricted scope
// returns ids unchanged.
func (s AccountScope) FilterIDs(ids []string) []string {
	if s.Unrestricted {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if s.Allows(id, "") {
			out = append(out, id)
		}
	}
	return out
}

// String renders the scope for error messages and logs.
func (s AccountScope) String() string {
	if s.Unrestricted {
		return "all accounts"
	}
	if len(s.Accounts) == 0 {
		return "no accounts"
	}
	return strings.Join(s.Accounts, ", ")
}
