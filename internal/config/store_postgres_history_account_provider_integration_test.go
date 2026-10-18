//go:build integration

package config

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedProviderVariantHistory inserts purchase_history rows that all carry
// cloud_account_id IS NULL, so only the external-id half of the account
// predicate can match them. The rows differ in provider spelling and account
// number. Raw SQL is used because the only Go writer always stores a lowercase
// provider, and these rows model legacy or hand-written data.
func seedProviderVariantHistory(ctx context.Context, t *testing.T, conn *database.Connection) {
	t.Helper()
	rows := []struct{ purchaseID, provider, accountID string }{
		{"ri-lower", "aws", "111122223333"},
		{"ri-upper", "AWS", "111122223333"},
		{"ri-mixed", "Aws", "111122223333"},
		{"ri-empty", "", "111122223333"},
		{"ri-azure", "azure", "111122223333"},
		{"ri-azure-upper", "Azure", "111122223333"},
		{"ri-other-acct", "AWS", "999988887777"},
	}
	for _, r := range rows {
		_, err := conn.Pool().Exec(ctx, `
			INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment, ramp_step)
			VALUES ($1, $2, now(), $3, 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront', 0)`,
			r.accountID, r.purchaseID, r.provider)
		require.NoError(t, err, "seed %s", r.purchaseID)
	}
}

func purchaseIDs(recs []PurchaseHistoryRecord) []string {
	ids := make([]string, 0, len(recs))
	for _rvc := range recs {
		ids = append(ids, recs[_rvc].PurchaseID)
	}
	sort.Strings(ids)
	return ids
}

// TestAccountPredicate_ProviderCaseAndEmpty_Integration is the regression test
// for issue #24: the external-id half of the account predicate compared
// provider with exact equality, so a NULL-cloud_account_id row whose provider
// was stored as "" or in another case ("AWS") was dropped even though its
// account_id matched. The fix compares case-insensitively and lets an empty
// (unknown) provider match on account_id alone. Rows of a different provider
// (azure, in any case) must still be excluded, so the aws/123 vs azure/123
// pairing guarantee is unchanged.
func TestAccountPredicate_ProviderCaseAndEmpty_Integration(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	ctx := context.Background()
	seedProviderVariantHistory(ctx, t, conn)

	want := []string{"ri-empty", "ri-lower", "ri-mixed", "ri-upper"}

	for _, key := range []string{"aws", "AWS"} {
		filter := map[string][]string{key: {"111122223333"}}

		t.Run("GetPurchaseHistoryFiltered key="+key, func(t *testing.T) {
			recs, err := store.GetPurchaseHistoryFiltered(ctx, PurchaseHistoryFilter{
				ExternalIDsByProvider: filter,
				Limit:                 100,
			})
			require.NoError(t, err)
			assert.Equal(t, want, purchaseIDs(recs))
		})

		t.Run("GetActivePurchaseHistory key="+key, func(t *testing.T) {
			recs, err := store.GetActivePurchaseHistory(ctx, time.Now(), nil, filter)
			require.NoError(t, err)
			assert.Equal(t, want, purchaseIDs(recs))
		})
	}

	t.Run("azure filter does not pick up aws rows", func(t *testing.T) {
		recs, err := store.GetPurchaseHistoryFiltered(ctx, PurchaseHistoryFilter{
			ExternalIDsByProvider: map[string][]string{"azure": {"111122223333"}},
			Limit:                 100,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"ri-azure", "ri-azure-upper", "ri-empty"}, purchaseIDs(recs))
	})
}

// seedDualColumnHistory inserts two aws cloud_accounts and purchase_history
// rows that populate the two account columns in every combination the
// account predicate has to handle. It returns the UUIDs of account A
// (aws/111122223333) and account C (aws/444455556666).
func seedDualColumnHistory(ctx context.Context, t *testing.T, conn *database.Connection) (acctA, acctC string) {
	t.Helper()
	acctA, acctC = uuid.New().String(), uuid.New().String()
	for _, a := range []struct{ id, ext string }{{acctA, "111122223333"}, {acctC, "444455556666"}} {
		_, err := conn.Exec(ctx, `
			INSERT INTO cloud_accounts (id, name, enabled, provider, external_id, aws_is_org_root, created_at, updated_at)
			VALUES ($1, $2, true, 'aws', $3, false, now(), now())`, a.id, "acct-"+a.ext, a.ext)
		require.NoError(t, err, "seed cloud_accounts %s", a.ext)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = conn.Exec(cleanupCtx, "DELETE FROM purchase_history")
		_, _ = conn.Exec(cleanupCtx, "DELETE FROM cloud_accounts WHERE id = ANY($1::uuid[])", []string{acctA, acctC})
	})

	rows := []struct {
		purchaseID, provider, accountID string
		cloudAccountID                  *string
	}{
		// UUID set, external number not in any filter: only the UUID half matches.
		{"uuid-only", "aws", "000000000000", &acctA},
		// UUID and external both match: must come back exactly once.
		{"both", "aws", "111122223333", &acctA},
		// NULL UUID (direct-execute / pre-000011 row): only the external half matches.
		{"ext-only", "aws", "111122223333", nil},
		// NULL UUID, unknown provider: matches on account_id alone (#931).
		{"ext-empty-provider", "", "111122223333", nil},
		// Same external number under another provider: must never leak into an aws filter.
		{"azure-same-ext", "azure", "111122223333", nil},
		// Other account, by UUID and by external number.
		{"other-uuid", "aws", "444455556666", &acctC},
		{"other-ext", "aws", "444455556666", nil},
	}
	for _, r := range rows {
		_, err := conn.Exec(ctx, `
			INSERT INTO purchase_history (account_id, purchase_id, timestamp, provider, service, region, resource_type, term, payment, ramp_step, cloud_account_id)
			VALUES ($1, $2, now(), $3, 'ec2', 'us-east-1', 'm5.large', 1, 'all-upfront', 0, $4)`,
			r.accountID, r.purchaseID, r.provider, r.cloudAccountID)
		require.NoError(t, err, "seed %s", r.purchaseID)
	}
	return acctA, acctC
}

// TestGetPurchaseHistoryFiltered_DualColumnAccount_Integration is the real-DB
// test for issue #23. It runs the dual-column account predicate
//
//	(cloud_account_id = ANY($uuids) OR ((provider = '' OR LOWER(provider) = LOWER($p)) AND account_id = ANY($exts)))
//
// against Postgres. The pgxmock tests only check the generated SQL text, so
// they stay green if the predicate is wrong. Each case below fails when one
// part of the predicate is removed or changed: the UUID half, the external
// half, the provider gate, the OR between the halves, or the parentheses
// around the OR.
func TestGetPurchaseHistoryFiltered_DualColumnAccount_Integration(t *testing.T) {
	conn := setupTestContainerDB(t)
	cleanupTestData(t, conn)
	store := NewPostgresStore(conn)
	ctx := context.Background()
	acctA, acctC := seedDualColumnHistory(ctx, t, conn)

	cases := []struct {
		name   string
		filter PurchaseHistoryFilter
		want   []string
	}{
		{
			// What the API sends for the account-A chip: its UUID plus its
			// resolved (aws, 111122223333) pair. NULL-UUID rows of account A
			// come back with UUID-only rows, and azure/111122223333 does not.
			name: "uuid and external halves are ORed",
			filter: PurchaseHistoryFilter{
				AccountIDs:            []string{acctA},
				ExternalIDsByProvider: map[string][]string{"aws": {"111122223333"}},
			},
			want: []string{"both", "ext-empty-provider", "ext-only", "uuid-only"},
		},
		{
			name:   "uuid only matches cloud_account_id and ignores account_id",
			filter: PurchaseHistoryFilter{AccountIDs: []string{acctA}},
			want:   []string{"both", "uuid-only"},
		},
		{
			name: "external only stays provider scoped",
			filter: PurchaseHistoryFilter{
				ExternalIDsByProvider: map[string][]string{"aws": {"111122223333"}},
			},
			want: []string{"both", "ext-empty-provider", "ext-only"},
		},
		{
			// Same external number, other provider: the azure pair picks up
			// only the azure row and the unknown-provider row, never the aws
			// NULL-UUID row. The UUID half still matches account A's rows.
			name: "provider mismatch on external half",
			filter: PurchaseHistoryFilter{
				AccountIDs:            []string{acctA},
				ExternalIDsByProvider: map[string][]string{"azure": {"111122223333"}},
			},
			want: []string{"azure-same-ext", "both", "ext-empty-provider", "uuid-only"},
		},
		{
			// The OR group must stay inside the surrounding AND chain. Without
			// the parentheses, "provider = 'aws' AND uuid OR ext" would let the
			// empty-provider row through the ext half and skip the provider clause.
			name: "account OR group composes with the provider clause",
			filter: PurchaseHistoryFilter{
				Provider:              "aws",
				AccountIDs:            []string{acctC},
				ExternalIDsByProvider: map[string][]string{"aws": {"111122223333"}},
			},
			want: []string{"both", "ext-only", "other-uuid"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.filter.Limit = 100
			recs, err := store.GetPurchaseHistoryFiltered(ctx, tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.want, purchaseIDs(recs))
		})
	}
}
