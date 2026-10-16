package config

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
)

// ErrLadderRunCancelActorRequired is returned by CancelLadderRun when no actor
// is supplied, so a ladder run can never be canceled with a NULL canceled-by
// audit column.
var ErrLadderRunCancelActorRequired = errors.New("canceling a ladder run requires the actor's email")

// ErrLadderRunCancelViaTransition is returned by TransitionLadderRunStatus for
// a transition to RunStatusCancelled: that path cannot stamp the canceled-by
// audit column, so cancels must go through CancelLadderRun (issue #542).
var ErrLadderRunCancelViaTransition = errors.New("ladder run cancel must use CancelLadderRun so the actor is recorded")

// ladderRunCancelQuery is the CAS UPDATE behind CancelLadderRun. It sets the
// status and the canceled-by audit column in one statement so the actor can
// never lag the status change. The column keeps its legacy UK spelling
// (migration 000080).
//
//nolint:misspell // SQL column name, not prose
const ladderRunCancelQuery = `
		UPDATE ladder_runs
		SET status = $2, cancelled_by = $4, updated_at = NOW()
		WHERE id = $1 AND status = ANY($3)
		RETURNING id, config_id, started_at, completed_at, status, mode, cadence,
		          baseline_usd_hr, target_usd_hr, existing_usd_hr, gap_usd_hr,
		          plan, total_hourly_commit, total_upfront_cost, estimated_savings,
		          approval_token_hash, approval_token_expires_at,
		          approved_by, cancelled_by, fire_at,
		          created_at, updated_at
	`

// ladderRunStatusStrings converts typed statuses to the plain []string pgx
// encodes for an ANY($n) text[] comparison.
func ladderRunStatusStrings(statuses []ladder.RunStatus) []string {
	out := make([]string, len(statuses))
	for i, st := range statuses {
		out[i] = string(st)
	}
	return out
}

// CancelLadderRun atomically transitions a ladder_runs row from one of
// fromStatuses to RunStatusCancelled and stamps the canceled-by audit column
// with the actor's email in the same UPDATE. Returns the updated row on success, or (nil, nil)
// when zero rows are affected (race lost or unexpected current status). An
// empty actor is rejected with ErrLadderRunCancelActorRequired before any
// write.
func (s *PostgresStore) CancelLadderRun(ctx context.Context, id string, fromStatuses []ladder.RunStatus, cancelledBy string) (*LadderRunDB, error) {
	actor := strings.TrimSpace(cancelledBy)
	if actor == "" {
		return nil, ErrLadderRunCancelActorRequired
	}
	row := s.db.QueryRow(ctx, ladderRunCancelQuery, id, string(ladder.RunStatusCancelled), ladderRunStatusStrings(fromStatuses), actor)
	result, err := scanLadderRun(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // CAS race lost
		}
		return nil, fmt.Errorf("failed to cancel ladder_run id=%s: %w", id, err)
	}
	return &result, nil
}
