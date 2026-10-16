//go:build integration

package config

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
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
