//go:build integration
// +build integration

package analytics_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/analytics"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #32: the account-filter integration tests were remapped mechanically
// from the removed single-string account argument, and every row they saved
// belonged to one account, so a filter that matched the wrong column, dropped a
// column, or matched everything still passed them. This suite seeds rows that
// each hit exactly one half of the dual-column predicate (cloud_account_id UUID
// vs per-provider account_id) plus decoys that only a broken predicate would
// return, and asserts the exact matched set through every Query* method.
//
// Each row gets its own service so it is its own bucket in every query's
// output (QuerySavings rows, QueryByProvider's (provider, service),
// QueryByService's (service, region), and monthly_savings_summary's
// (month, account, cloud account, provider, service)), which lets one service
// set identify exactly which rows each method matched.

// External account numbers. Named acct* rather than account* so these
// placeholder literals don't look like AWS account-ID assignments to the
// git-secrets scanner.
const (
	acctUnderTest = "111111111111" // the account being filtered for
	acctOther     = "222222222222" // another tenant's AWS account
	acctStale     = "333333333333" // what a UUID-only row carries in account_id; no filter names it
)

// Fixture services, one per seeded row.
const (
	svcBoth      = "both-columns"       // aws, acctUnderTest, cloud_account_id = its UUID
	svcExtOnly   = "external-id-only"   // aws, acctUnderTest, cloud_account_id NULL (ambient/legacy row)
	svcUUIDOnly  = "uuid-only"          // aws, acctStale, cloud_account_id = acctUnderTest's UUID
	svcOtherProv = "gcp-same-number"    // gcp, acctUnderTest, NULL: same external number, other provider
	svcOtherExt  = "other-external-id"  // aws, acctOther, NULL
	svcOtherUUID = "other-account-uuid" // aws, acctOther, cloud_account_id = acctOther's UUID
)

// accountFilterWindow is the half-width of the query window around the seeded
// timestamp.
const accountFilterWindow = time.Hour

// accountFilterFixture holds the seeded state the cases query against.
type accountFilterFixture struct {
	store     *analytics.PostgresAnalyticsStore
	uuidUnder string // cloud_accounts.id of aws/acctUnderTest
	ts        time.Time
}

func TestPostgresAnalyticsStore_AccountFilterSemantics_DB(t *testing.T) {
	skipIfDBTestsOptedOut(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())

	fx := seedAccountFilterFixture(ctx, t, container)

	cases := []struct {
		name  string
		uuids []string
		ext   map[string][]string
		want  []string
	}{
		{
			// What resolveSingleAccountFilterIDs returns for a known account
			// UUID that has an external id: both columns, the external half
			// scoped to the account's provider. Fails if either half is
			// dropped, if the UUIDs are bound against account_id (the
			// documented wrong-column trap), or if the provider scope is lost
			// (the GCP row with the same number would leak).
			name:  "known account UUID plus its provider-scoped external id",
			uuids: []string{fx.uuidUnder},
			ext:   map[string][]string{"aws": {acctUnderTest}},
			want:  []string{svcBoth, svcExtOnly, svcUUIDOnly},
		},
		{
			// A known account with no external id resolves to UUIDs only. Only
			// cloud_account_id can match; binding the UUIDs against account_id
			// would return nothing.
			name:  "account UUID only matches cloud_account_id",
			uuids: []string{fx.uuidUnder},
			want:  []string{svcBoth, svcUUIDOnly},
		},
		{
			name: "provider-scoped external id matches account_id for that provider only",
			ext:  map[string][]string{"aws": {acctUnderTest}},
			want: []string{svcBoth, svcExtOnly},
		},
		{
			// The blanket {"aws","gcp"} shape the compile-fix remap used: it
			// matches each provider's rows for the number, and nothing else.
			name: "external id keyed under two providers matches both, nothing else",
			ext:  map[string][]string{"aws": {acctUnderTest}, "gcp": {acctUnderTest}},
			want: []string{svcBoth, svcExtOnly, svcOtherProv},
		},
		{
			// An unknown raw account number resolves to the "" key, which
			// matches account_id on any provider. Skipping the "" key would
			// leave no predicate and fall through to TRUE (every row);
			// treating it as provider = '' would match nothing.
			name: "legacy external id under the empty provider key matches any provider",
			ext:  map[string][]string{"": {acctUnderTest}},
			want: []string{svcBoth, svcExtOnly, svcOtherProv},
		},
		{
			name: "external id under a provider that has no such rows matches nothing",
			ext:  map[string][]string{"azure": {acctUnderTest}},
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx.assertAllQueriesMatch(ctx, t, tc.uuids, tc.ext, tc.want)
		})
	}
}

// seedAccountFilterFixture inserts two cloud accounts and one snapshot per
// fixture service, then refreshes the materialized views so
// QueryMonthlyTotals sees them.
func seedAccountFilterFixture(ctx context.Context, t *testing.T, c *testhelpers.PostgresContainer) *accountFilterFixture {
	t.Helper()

	insertAccount := func(name, external string) string {
		var id string
		require.NoError(t, c.DB.QueryRow(ctx,
			`INSERT INTO cloud_accounts (name, provider, external_id) VALUES ($1, 'aws', $2) RETURNING id::text`,
			name, external).Scan(&id))
		return id
	}
	uuidUnder := insertAccount("account under test", acctUnderTest)
	uuidOther := insertAccount("other tenant", acctOther)

	fx := &accountFilterFixture{
		store:     analytics.NewPostgresAnalyticsStore(c.DB),
		uuidUnder: uuidUnder,
		ts:        time.Now().UTC().Truncate(time.Microsecond),
	}

	rows := []struct {
		service, provider, external string
		cloud                       *string
		savings                     float64
	}{
		{svcBoth, "aws", acctUnderTest, &uuidUnder, 1},
		{svcExtOnly, "aws", acctUnderTest, nil, 2},
		{svcUUIDOnly, "aws", acctStale, &uuidUnder, 4},
		{svcOtherProv, "gcp", acctUnderTest, nil, 8},
		{svcOtherExt, "aws", acctOther, nil, 16},
		{svcOtherUUID, "aws", acctOther, &uuidOther, 32},
	}
	for _, r := range rows {
		require.NoError(t, fx.store.SaveSnapshot(ctx, &analytics.SavingsSnapshot{
			AccountID:      r.external,
			CloudAccountID: r.cloud,
			Timestamp:      fx.ts,
			Provider:       r.provider,
			Service:        r.service,
			Region:         "us-east-1",
			CommitmentType: "RI",
			TotalSavings:   r.savings,
		}))
	}

	require.NoError(t, fx.store.RefreshMaterializedViews(ctx))
	return fx
}

// assertAllQueriesMatch runs one account filter through every Query* method
// and asserts each returns exactly the want services.
func (fx *accountFilterFixture) assertAllQueriesMatch(ctx context.Context, t *testing.T, uuids []string, ext map[string][]string, want []string) {
	t.Helper()

	start, end := fx.ts.Add(-accountFilterWindow), fx.ts.Add(accountFilterWindow)
	want = sortedCopy(want)

	snaps, err := fx.store.QuerySavings(ctx, analytics.QueryRequest{
		AccountUUIDs:                 uuids,
		AccountExternalIDsByProvider: ext,
		StartDate:                    start,
		EndDate:                      end,
	})
	require.NoError(t, err)
	got := make([]string, 0, len(snaps))
	for _, s := range snaps {
		got = append(got, s.Service)
	}
	assert.Equal(t, want, sortedCopy(got), "QuerySavings")

	byProvider, err := fx.store.QueryByProvider(ctx, uuids, ext, start, end)
	require.NoError(t, err)
	got = got[:0]
	for _, b := range byProvider {
		got = append(got, b.Service)
	}
	assert.Equal(t, want, sortedCopy(got), "QueryByProvider")

	byService, err := fx.store.QueryByService(ctx, uuids, ext, "", start, end)
	require.NoError(t, err)
	got = got[:0]
	for _, b := range byService {
		got = append(got, b.Service)
	}
	assert.Equal(t, want, sortedCopy(got), "QueryByService")

	// Two months, not one, so a run in the first hour of a month still sees
	// the seeded row if NOW() has crossed into the next month since seeding.
	monthly, err := fx.store.QueryMonthlyTotals(ctx, uuids, ext, 2)
	require.NoError(t, err)
	got = got[:0]
	for _, m := range monthly {
		got = append(got, m.Service)
	}
	assert.Equal(t, want, sortedCopy(got), "QueryMonthlyTotals")
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
