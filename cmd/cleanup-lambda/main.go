package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/aws/aws-lambda-go/lambda"
)

// CleanupEvent represents the input to the cleanup function.
type CleanupEvent struct {
	DryRun bool `json:"dryRun,omitempty"`
}

// CleanupResult represents the cleanup operation results.
type CleanupResult struct {
	SessionsDeleted   int64 `json:"sessionsDeleted"`
	ExecutionsDeleted int64 `json:"executionsDeleted"`
	DryRun            bool  `json:"dryRun"`
	Timestamp         int64 `json:"timestamp"`
}

// expiredExecutionsWhere selects the purchase_executions rows this job may
// delete: the row's own TTL (expires_at) has lapsed AND the row is settled.
//
// The TTL alone is not enough (issue #998). `expires_at` is written once at
// insert by SavePurchaseExecution and never rewritten when the row changes
// status, so a lapsed TTL says nothing about whether an executor, scheduler
// or reaper still needs the row. The status half therefore mirrors the rules
// the rest of the store already enforces:
//
//   - status = ANY(TerminalExecutionStatuses): only rows nothing will act on
//     again. It is an allowlist on purpose, so a status added later is kept
//     until someone classifies it, the same fail-closed choice
//     DeleteCloudAccount makes.
//   - status <> ALL(HealthScoredExecutionStatuses): failed and canceled rows
//     are left to CleanupOldExecutions, whose own branches retain them for
//     the plan health score's window (see that function's branch 3).
//
// Both lists come from the config package so this job cannot drift from the
// store. `status` is NOT NULL (migration 000001), so neither array test is
// exposed to three-valued logic. NULL expires_at never matches, so rows that
// never had a TTL (every row current code writes) are untouched.
//
// Under READ COMMITTED a row another transaction is updating is re-checked
// against this whole predicate after that transaction commits, so a row that
// stops being settled while the DELETE waits for it survives.
const expiredExecutionsWhere = `expires_at < $1 AND status = ANY($2) AND status <> ALL($3)`

// expiredExecutionsArgs are the arguments for expiredExecutionsWhere.
func expiredExecutionsArgs(now time.Time) []any {
	return []any{now, config.TerminalExecutionStatuses, config.HealthScoredExecutionStatuses}
}

func cleanupExpiredRecords(ctx context.Context, event CleanupEvent) (*CleanupResult, error) {
	log.Printf("Starting cleanup job (dryRun=%v)", event.DryRun)

	// A new DB connection is opened per invocation (no connection reuse across warm starts).
	// This is intentional: the cleanup Lambda runs infrequently and the simpler, stateless
	// design is preferred over the shared-connection pattern used in cmd/lambda/main.go.
	db, err := database.OpenFromEnv(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	now := time.Now()
	result := &CleanupResult{
		DryRun:    event.DryRun,
		Timestamp: now.Unix(),
	}

	if event.DryRun {
		if err := dryRunCount(ctx, db, now, result); err != nil {
			return nil, err
		}
	} else {
		if err := deleteExpired(ctx, db, now, result); err != nil {
			return nil, err
		}
	}

	log.Printf("Cleanup job completed: %+v", result)
	return result, nil
}

// dryRunCount counts records that would be deleted without actually deleting
// them. It uses the same execution predicate as deleteExpired.
func dryRunCount(ctx context.Context, db *database.Connection, now time.Time, result *CleanupResult) error {
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM sessions WHERE expires_at < $1", now).Scan(&result.SessionsDeleted); err != nil {
		return fmt.Errorf("failed to count expired sessions: %w", err)
	}
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM purchase_executions WHERE "+expiredExecutionsWhere, expiredExecutionsArgs(now)...).Scan(&result.ExecutionsDeleted); err != nil {
		return fmt.Errorf("failed to count expired executions: %w", err)
	}
	log.Printf("DRY RUN: Would delete %d sessions and %d executions", result.SessionsDeleted, result.ExecutionsDeleted)
	return nil
}

// deleteExpired deletes expired sessions and settled expired executions
// (expiredExecutionsWhere) in a single transaction.
func deleteExpired(ctx context.Context, db *database.Connection, now time.Time, result *CleanupResult) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			if rErr := tx.Rollback(ctx); rErr != nil {
				log.Printf("rollback failed: %v", rErr)
			}
		}
	}()

	tag, err := tx.Exec(ctx, "DELETE FROM sessions WHERE expires_at < $1", now)
	if err != nil {
		return fmt.Errorf("failed to cleanup sessions: %w", err)
	}
	result.SessionsDeleted = tag.RowsAffected()
	log.Printf("Deleted %d expired sessions", result.SessionsDeleted)

	tag, err = tx.Exec(ctx, "DELETE FROM purchase_executions WHERE "+expiredExecutionsWhere, expiredExecutionsArgs(now)...)
	if err != nil {
		return fmt.Errorf("failed to cleanup executions: %w", err)
	}
	result.ExecutionsDeleted = tag.RowsAffected()
	log.Printf("Deleted %d expired executions", result.ExecutionsDeleted)

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit cleanup transaction: %w", err)
	}
	return nil
}

func main() {
	lambda.Start(cleanupExpiredRecords)
}
