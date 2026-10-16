package config

import (
	"context"
	"time"
)

// settledLadderRows are the config's completed tranches that settled after
// anchor (strictly: settled_at == anchor is not counted). Rows completed before
// the column existed were backfilled with the migration time, so they are
// counted for any anchor older than it. failed and canceled rows are
// never counted. Consumers add them to the in-flight rows when the
// existing commitment they compare against was read at or before anchor.
func settledLadderRows(ctx context.Context, q dbConn, assumeOD bool, configID string, anchor time.Time) ([]LadderInFlightRow, error) {
	return ladderNettingRows(ctx, q, assumeOD, "status = 'completed' AND settled_at > $2", configID, anchor)
}
