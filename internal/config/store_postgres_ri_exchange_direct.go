package config

import (
	"context"
	"fmt"
)

// SaveRIExchangeDirectExecution inserts the audit row for a direct-API RI
// exchange as "submitted", before the provider call (issue #82). The status
// and created_at are not taken from the record: every row starts submitted,
// stamped by the database clock.
func (s *PostgresStore) SaveRIExchangeDirectExecution(ctx context.Context, record *RIExchangeDirectExecution) error {
	if err := record.Validate(); err != nil {
		return err
	}
	var sources *string
	if record.Sources != nil {
		raw := string(record.Sources)
		sources = &raw
	}
	query := `
		INSERT INTO ri_exchange_direct_executions (
			id, provider, cloud_account_id, provider_account_id, region,
			source_ids, sources, targets, max_payment_due, max_payment_currency,
			quoted_net_payable, provider_reference,
			executed_by_user_id, executed_by, executed_by_api_key_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9::numeric, $10, $11::numeric, $12, $13, $14, $15)
	`
	_, err := s.db.Exec(ctx, query,
		record.ID,
		record.Provider,
		record.CloudAccountID,
		record.ProviderAccountID,
		record.Region,
		record.SourceIDs,
		sources,
		string(record.Targets),
		record.MaxPaymentDue,
		record.MaxPaymentCurrency,
		record.QuotedNetPayable,
		nullStringFromString(record.ProviderReference),
		record.ExecutedByUserID,
		record.ExecutedBy,
		record.ExecutedByAPIKeyID,
	)
	if err != nil {
		return fmt.Errorf("failed to save ri exchange direct execution: %w", err)
	}
	return nil
}

// FinalizeRIExchangeDirectExecution records the provider outcome on a
// submitted audit row. It is a compare-and-set on status = 'submitted', so a
// row is finalized at most once: a second finalize, concurrent or later,
// updates nothing and returns ErrRIExchangeDirectExecutionNotSubmitted rather
// than overwriting the outcome already recorded.
func (s *PostgresStore) FinalizeRIExchangeDirectExecution(ctx context.Context, id string, outcome RIExchangeDirectOutcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	query := `
		UPDATE ri_exchange_direct_executions
		   SET status               = $2,
		       provider_reference   = COALESCE($3, provider_reference),
		       net_payable          = $4::numeric,
		       net_payable_currency = $5,
		       provider_status      = $6,
		       error                = $7,
		       completed_at         = now()
		 WHERE id = $1
		   AND status = 'submitted'
	`
	tag, err := s.db.Exec(ctx, query,
		id,
		outcome.Status,
		nullStringFromString(outcome.ProviderReference),
		outcome.NetPayable,
		nullStringFromString(outcome.NetPayableCurrency),
		nullStringFromString(outcome.ProviderStatus),
		nullStringFromString(outcome.Error),
	)
	if err != nil {
		return fmt.Errorf("failed to finalize ri exchange direct execution %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finalize ri exchange direct execution %s: %w", id, ErrRIExchangeDirectExecutionNotSubmitted)
	}
	return nil
}
