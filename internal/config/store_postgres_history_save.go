package config

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/jackc/pgx/v5"
)

// savePurchaseHistoryQuery inserts one purchase_history row and returns
// whether it was stored with a cloud_account_id. No row comes back when the
// purchase was already recorded.
//
// A NULL $17 is resolved to the registered account whose
// (provider, external_id), the cloud_accounts UNIQUE key, matches the row,
// with the same rules as the migration 000139 backfill (issue #1235):
//   - the provider is compared lower-cased: cloud_accounts.provider is
//     CHECK-constrained to lower case, and legacy rows say 'AWS';
//   - the account id is compared exactly, as the readers do;
//   - an empty account id never matches;
//   - no match stores NULL.
//
// The lookup locks the account FOR KEY SHARE. Without the lock, take an account
// that a transaction deletes and commits after the lookup but before the
// foreign-key check: the insert aborts with an FK violation, and a bought
// commitment loses its history row. With the lock, the insert waits for an
// in-flight delete, then finds no account and stores NULL, the value that
// ON DELETE SET NULL would have left. A delete that starts later waits for
// this insert, then clears the column itself. The lookup is not evaluated when
// the NOT EXISTS dedupe skips the insert.
const savePurchaseHistoryQuery = `
	INSERT INTO purchase_history (
		account_id, purchase_id, timestamp, provider, service, region,
		resource_type, count, term, payment, upfront_cost, monthly_cost,
		estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
		source, revocation_window_closes_at, offering_class
	)
	SELECT $1::varchar, $2::varchar, $3, $4::varchar, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
		COALESCE($17::uuid, (
			SELECT ca.id FROM cloud_accounts AS ca
			WHERE $1::varchar <> ''
			  AND ca.external_id = $1::varchar
			  AND ca.provider = LOWER($4::varchar)
			FOR KEY SHARE
		)),
		$18, $19, $20
	WHERE NOT EXISTS (
		SELECT 1 FROM purchase_history
		WHERE provider = $4 AND account_id = $1 AND purchase_id = $2
	)
	ON CONFLICT DO NOTHING
	RETURNING cloud_account_id IS NOT NULL
`

// SavePurchaseHistory saves a purchase history record. It is idempotent per
// (provider, account_id, purchase_id): a re-drive or root retry that adopts a
// commitment the first attempt already bought saves the same key again, and a
// second row would double-count spend and savings (#704). The NOT EXISTS
// covers rows that predate the unique index from migration 000108; the index
// and ON CONFLICT cover two concurrent saves. An existing row is left as is.
//
// cloud_account_id is the record's CloudAccountID when set. Otherwise it is
// resolved from (provider, account_id) in the same statement (see
// savePurchaseHistoryQuery), so the direct-execute, ambient and legacy
// single-account paths, which carry no CloudAccountID, store it too (#1235).
func (s *PostgresStore) SavePurchaseHistory(ctx context.Context, record *PurchaseHistoryRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}

	var attached bool
	err := s.db.QueryRow(ctx, savePurchaseHistoryQuery,
		record.AccountID,
		record.PurchaseID,
		record.Timestamp,
		record.Provider,
		record.Service,
		record.Region,
		record.ResourceType,
		record.Count,
		record.Term,
		record.Payment,
		record.UpfrontCost,
		record.MonthlyCost,
		record.EstimatedSavings,
		nullStringFromString(record.PlanID),
		nullStringFromString(record.PlanName),
		record.RampStep,
		record.CloudAccountID,
		record.Source,
		record.RevocationWindowClosesAt,
		nullStringFromString(record.OfferingClass),
	).Scan(&attached)

	if errors.Is(err, pgx.ErrNoRows) {
		logging.Infof("purchase history for %s/%s/%s already recorded; keeping the existing row", record.Provider, record.AccountID, record.PurchaseID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to save purchase history: %w", err)
	}
	if !attached {
		// The row is kept rather than refused: the commitment is already
		// bought, and a missing history row is an audit gap (#621). Readers
		// still match the row by its external account_id. The warning makes
		// the missing link visible instead of silent.
		logging.Warnf("purchase history for %s/%s/%s saved without a cloud account: no registered %s account has external id %q",
			record.Provider, record.AccountID, record.PurchaseID, strings.ToLower(record.Provider), record.AccountID)
	}
	return nil
}
