package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #1429: the analytics routes read the plural account_ids as well as the
// legacy account_id, with the same per-id scope check. Scope entries are
// account IDs (#1034), so the scoped fixtures list IDs, not names.
const (
	idsAcctA = "11111111-1111-4111-8111-111111111111"
	idsAcctB = "22222222-2222-4222-8222-222222222222"
	idsAcctC = "33333333-3333-4333-8333-333333333333"
)

func idsAccounts() []config.CloudAccount {
	return []config.CloudAccount{
		{ID: idsAcctA, Name: "a", Provider: "aws", ExternalID: "111111111111"},
		{ID: idsAcctB, Name: "b", Provider: "aws", ExternalID: "222222222222"},
		{ID: idsAcctC, Name: "c", Provider: "gcp", ExternalID: "proj-c"},
	}
}

func TestAnalytics_AccountIDs_AllReachFilter(t *testing.T) {
	t.Parallel()
	for _, path := range analyticsPaths {
		for name, scope := range map[string][]string{"unrestricted": nil, "scoped": {idsAcctA, idsAcctB, idsAcctC}} {
			t.Run(path+" "+name, func(t *testing.T) {
				t.Parallel()
				params := map[string]string{"account_ids": idsAcctA + "," + idsAcctB + "," + idsAcctC}
				status, uuids, externals := analyticsParamsCall(t, path, scope, params, idsAccounts())
				assert.Equal(t, 200, status)
				assert.Equal(t, []string{idsAcctA, idsAcctB, idsAcctC}, uuids)
				assert.Equal(t, map[string][]string{
					"aws": {"111111111111", "222222222222"},
					"gcp": {"proj-c"},
				}, externals)
			})
		}
	}
}

func TestAnalytics_AccountIDs_ForeignIDRefusedLikeSingle(t *testing.T) {
	t.Parallel()
	scope := []string{idsAcctA} // C and B are out of scope
	for _, path := range analyticsPaths {
		single, _, _ := analyticsParamsCall(t, path, scope, map[string]string{"account_id": idsAcctC}, idsAccounts())
		for name, ids := range map[string]string{
			"allowed then foreign": idsAcctA + "," + idsAcctC,
			"foreign then allowed": idsAcctC + "," + idsAcctA,
			"only foreign":         idsAcctC + "," + idsAcctB,
		} {
			t.Run(path+" "+name, func(t *testing.T) {
				t.Parallel()
				status, uuids, externals := analyticsParamsCall(t, path, scope, map[string]string{"account_ids": ids}, idsAccounts())
				assert.Equal(t, single, status, "same outcome as a single out-of-scope account_id")
				assert.Equal(t, 404, status)
				assert.Nil(t, uuids, "no query may run")
				assert.Nil(t, externals, "no query may run")
			})
		}
	}
}

func TestAnalytics_AccountIDs_ScopedAllowedSubset(t *testing.T) {
	t.Parallel()
	status, uuids, externals := analyticsParamsCall(t, "/api/history/analytics", []string{idsAcctA, idsAcctB},
		map[string]string{"account_ids": idsAcctA + "," + idsAcctB}, idsAccounts())
	assert.Equal(t, 200, status)
	assert.Equal(t, []string{idsAcctA, idsAcctB}, uuids)
	assert.Equal(t, map[string][]string{"aws": {"111111111111", "222222222222"}}, externals)
}

func TestAnalytics_AccountIDs_InvalidRejected(t *testing.T) {
	t.Parallel()
	tooMany := strings.Repeat(idsAcctA+",", MaxAccountIDsPerRequest+1)
	for _, path := range analyticsPaths {
		for name, ids := range map[string]string{"garbage": "not-a-uuid", "mixed": idsAcctA + ",nope", "too many": tooMany} {
			t.Run(path+" "+name, func(t *testing.T) {
				t.Parallel()
				status, uuids, _ := analyticsParamsCall(t, path, nil, map[string]string{"account_ids": ids}, idsAccounts())
				assert.Equal(t, 400, status)
				assert.Nil(t, uuids)
			})
		}
	}
}

func TestAnalytics_AccountIDs_AbsentUnchanged(t *testing.T) {
	t.Parallel()
	for _, path := range analyticsPaths {
		status, uuids, externals := analyticsParamsCall(t, path, nil, map[string]string{}, idsAccounts())
		assert.Equal(t, 200, status, path)
		assert.Nil(t, uuids, path)
		assert.Nil(t, externals, path)

		status, _, _ = analyticsParamsCall(t, path, []string{idsAcctA}, map[string]string{}, idsAccounts())
		assert.Equal(t, 400, status, "%s: scoped user must still name an account", path)

		status, uuids, externals = analyticsParamsCall(t, path, nil, map[string]string{"account_id": idsAcctA}, idsAccounts())
		assert.Equal(t, 200, status, path)
		assert.Equal(t, []string{idsAcctA}, uuids, path)
		assert.Equal(t, map[string][]string{"aws": {"111111111111"}}, externals, path)
	}
}

// Both params sent (older frontends send account_id = first of account_ids):
// the plural wins and the singular is ignored, so a singular that is out of
// scope cannot widen or break an otherwise in-scope plural request.
func TestAnalytics_AccountIDs_PluralWinsOverSingular(t *testing.T) {
	t.Parallel()
	status, uuids, _ := analyticsParamsCall(t, "/api/history/analytics", []string{idsAcctA, idsAcctB},
		map[string]string{"account_ids": idsAcctA + "," + idsAcctB, "account_id": idsAcctC}, idsAccounts())
	assert.Equal(t, 200, status)
	assert.Equal(t, []string{idsAcctA, idsAcctB}, uuids, "singular account_id is ignored when account_ids is present")
}

func TestAnalytics_AccountIDs_DuplicatesCollapse(t *testing.T) {
	t.Parallel()
	_, uuids, externals := analyticsParamsCall(t, "/api/history/analytics", nil,
		map[string]string{"account_ids": idsAcctA + "," + idsAcctA}, idsAccounts())
	assert.Equal(t, []string{idsAcctA}, uuids)
	assert.Equal(t, map[string][]string{"aws": {"111111111111"}}, externals)
}

// Issue #1452: parseAccountIDs canonicalises (trim + lowercase), so an
// uppercase or padded id behaves exactly like its lowercase form, including
// for a scoped user's own account.
func TestAnalytics_AccountIDs_UppercasePaddedCanonical(t *testing.T) {
	t.Parallel()
	// Hex letters so ToUpper really changes the ids.
	const idA, idB = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	accounts := []config.CloudAccount{
		{ID: idA, Name: "a", Provider: "aws", ExternalID: "111111111111"},
		{ID: idB, Name: "b", Provider: "aws", ExternalID: "222222222222"},
	}
	upper := " " + strings.ToUpper(idA) + " , " + strings.ToUpper(idB)
	lower := idA + "," + idB
	for _, path := range analyticsPaths {
		for name, scope := range map[string][]string{"unrestricted": nil, "scoped": {idA, idB}} {
			t.Run(path+" "+name, func(t *testing.T) {
				t.Parallel()
				wantStatus, wantUUIDs, wantExt := analyticsParamsCall(t, path, scope, map[string]string{"account_ids": lower}, accounts)
				status, uuids, externals := analyticsParamsCall(t, path, scope, map[string]string{"account_ids": upper}, accounts)
				assert.Equal(t, 200, wantStatus)
				assert.Equal(t, wantStatus, status)
				assert.Equal(t, []string{idA, idB}, uuids)
				assert.Equal(t, wantUUIDs, uuids)
				assert.Equal(t, wantExt, externals)
			})
		}
	}
}
