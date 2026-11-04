package server

import (
	"context"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
)

// SchedulerInterface defines the methods required for the scheduler component.
type SchedulerInterface interface {
	CollectRecommendations(ctx context.Context, ownerToken string) (*scheduler.CollectResult, error)
	ListRecommendations(ctx context.Context, filter config.RecommendationFilter) ([]config.RecommendationRecord, error)
	// ScanRecommendations walks the same rows as ListRecommendations in
	// ID-ordered chunks for the paginated list endpoint (issue #981).
	ScanRecommendations(ctx context.Context, filter config.RecommendationFilter, chunkSize int, fn func([]config.RecommendationRecord) error) error
	// PageRecommendations reads one list page and the full-set summary in
	// SQL for the paginated list endpoint (issue #981).
	PageRecommendations(ctx context.Context, scope config.RecommendationListScope, afterID string, limit int) (*config.RecommendationPage, error)
	// GetRecommendationByID fetches a single rec by application-level id,
	// bypassing account-override filtering. hiddenBy is non-nil when the rec
	// exists but would be dropped by the override filter. Returns nil, nil,
	// nil when absent or fully suppressed.
	GetRecommendationByID(ctx context.Context, id string) (rec *config.RecommendationRecord, hiddenBy []string, err error)
}

// PurchaseManagerInterface defines the methods required for the purchase manager component.
type PurchaseManagerInterface interface {
	ProcessScheduledPurchases(ctx context.Context) (*purchase.ProcessResult, error)
	SendUpcomingPurchaseNotifications(ctx context.Context) (*purchase.NotificationResult, error)
	ProcessMessage(ctx context.Context, body string) error
	// ApproveExecution, ApproveAndExecute and RunPlannedPurchaseNow return the
	// raw revocation token minted on a successful execute (issue #103: only
	// its hash is stored, so a re-read cannot yield an emailable token).
	// Empty on failure. Each also returns the row with its final status, nil
	// when the run never started (issue #532), and takes the caller's
	// ClaimedRowCheck, run against the claimed row before any provider call
	// (issue #415).
	ApproveExecution(ctx context.Context, execID, token, actor string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error)
	ApproveAndExecute(ctx context.Context, execID, actor string, transitionedBy *string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error)
	// DirectExecute is ApproveAndExecute for the direct-execute permission; it
	// stamps the executed_* audit fields only after the claim is won.
	DirectExecute(ctx context.Context, execID, actor string, transitionedBy *string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error)
	// RunPlannedPurchaseNow forces a pending or paused scheduled purchase to
	// execute immediately (the "Run now" button), sharing ApproveAndExecute's
	// 4-eyes-gated, CAS-guarded funnel instead of a bare status flip that
	// nothing else consumes (issue #218).
	RunPlannedPurchaseNow(ctx context.Context, execID, actor string, transitionedBy *string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error)
	CancelExecution(ctx context.Context, execID, token, actor string) error
	// ReapStuckExecutions sweeps purchase_executions stuck in
	// approved/running longer than reapAfter and flips them to "failed"
	// via the existing TransitionExecutionStatus CAS. Wired into the
	// "reap_stuck_purchases" scheduled task. See issue #678.
	ReapStuckExecutions(ctx context.Context, reapAfter time.Duration) (*purchase.ReapResult, error)
	// FireScheduledDelayedPurchases fires purchase_executions in status=scheduled
	// whose scheduled_execution_at is in the past (Gmail-style pre-fire delay,
	// issue #291 wave-2). Called on the "fire_scheduled_purchases" scheduler tick.
	FireScheduledDelayedPurchases(ctx context.Context) (*purchase.FireResult, error)
	// FinalizeInFlightRevocations sweeps unrevoked purchase_history rows with a
	// recorded Azure Return and retries MarkPurchaseRevoked for each, and
	// releases flags that stayed set with no recorded Return (issue #1523). Handles
	// the partial-success case where Azure Return succeeded but the DB write
	// failed (issue #290 Finding #6). Called on the "finalize_revocations" tick.
	FinalizeInFlightRevocations(ctx context.Context) (*purchase.FinalizeResult, error)
}

// AnalyticsStoreInterface defines the methods required for analytics storage.
// Beyond the materialized-view refresh, the scheduled analytics task also keeps
// monthly partitions provisioned ahead of time and applies retention.
type AnalyticsStoreInterface interface {
	RefreshMaterializedViews(ctx context.Context) error
	// CreatePartition idempotently ensures the monthly partition containing
	// forMonth exists. The collect task requires the current month's partition
	// before writing so snapshots never land in the default partition (#271).
	CreatePartition(ctx context.Context, forMonth time.Time) error
	// CreateFuturePartitions ensures partitions exist for the current month
	// plus monthsAhead months ahead (M3: partitions otherwise stop after the
	// seeded months and every insert falls into the catch-all default).
	CreateFuturePartitions(ctx context.Context, monthsAhead int) error
	// DropOldPartitions drops partitions older than retentionMonths (retention).
	DropOldPartitions(ctx context.Context, retentionMonths int) error
}

// AnalyticsCollectorInterface aggregates current savings into a point-in-time
// snapshot row per (tenant, provider, service, region, commitment_type) bucket.
type AnalyticsCollectorInterface interface {
	Collect(ctx context.Context) error
}
