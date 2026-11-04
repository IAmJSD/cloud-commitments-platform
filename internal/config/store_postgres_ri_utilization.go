package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GetRIUtilizationCache returns the cached Cost Explorer utilization
// result for (cloud_account_id, region, lookback_days) or nil if no row
// exists. Staleness is evaluated by the caller — the query doesn't filter
// on fetched_at so a caller with a longer TTL than expected can still use
// the row.
func (s *PostgresStore) GetRIUtilizationCache(ctx context.Context, key RIUtilizationCacheKey) (*RIUtilizationCacheEntry, error) {
	var entry RIUtilizationCacheEntry
	err := s.db.QueryRow(ctx, `
		SELECT cloud_account_id, region, lookback_days, payload, fetched_at
		  FROM ri_utilization_cache
		 WHERE cloud_account_id = $1 AND region = $2 AND lookback_days = $3
	`, key.CloudAccountID, key.Region, key.LookbackDays).Scan(&entry.CloudAccountID, &entry.Region, &entry.LookbackDays, &entry.Payload, &entry.FetchedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read ri_utilization_cache: %w", err)
	}
	return &entry, nil
}

// UpsertRIUtilizationCache writes or overwrites the cached utilization
// payload for (cloud_account_id, region, lookback_days). fetchedAt should
// be "now" at the time of the underlying Cost Explorer call — readers
// compare this against their TTL to decide freshness.
func (s *PostgresStore) UpsertRIUtilizationCache(ctx context.Context, key RIUtilizationCacheKey, payload []byte, fetchedAt time.Time) error {
	if _, err := s.db.Exec(ctx, `
		INSERT INTO ri_utilization_cache (cloud_account_id, region, lookback_days, payload, fetched_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (cloud_account_id, region, lookback_days)
		DO UPDATE SET
		    payload    = EXCLUDED.payload,
		    fetched_at = EXCLUDED.fetched_at
	`, key.CloudAccountID, key.Region, key.LookbackDays, payload, fetchedAt); err != nil {
		return fmt.Errorf("failed to upsert ri_utilization_cache: %w", err)
	}
	return nil
}
