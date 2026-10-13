package config

import (
	"context"
	"fmt"
	"time"
)

// settledLadderCommit sums the config's completed tranches that settled after
// anchor (strictly: settled_at == anchor is not counted). Rows completed before
// the column existed were backfilled with the migration time, so they are
// counted for any anchor older than it. failed and canceled rows are
// never counted. Consumers add it to the in-flight sum when the
// existing commitment they compare against was read at or before anchor.
func settledLadderCommit(ctx context.Context, q queryRower, configID string, anchor time.Time) (float64, error) {
	var total float64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_usd_hr), 0)
		FROM ladder_tranches
		WHERE config_id = $1
		  AND status = 'completed'
		  AND settled_at > $2
	`, configID, anchor).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("settled ladder commit config_id=%s: %w", configID, err)
	}
	return total, nil
}
