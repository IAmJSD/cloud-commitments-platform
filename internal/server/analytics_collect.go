package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/analytics"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
)

// AnalyticsConfig holds the savings-snapshot collector knobs, read from env at
// startup and validated at the boundary (see Validate).
type AnalyticsConfig struct {
	// Enabled gates the analytics_collect scheduled task. When false the task
	// returns a "disabled" status without touching the DB. Default true.
	Enabled bool
	// RetentionMonths is how many months of snapshot partitions to keep before
	// the retention job drops them. Default 24. Must be >= 1.
	RetentionMonths int
	// PartitionsAhead is how many future monthly partitions to keep provisioned
	// ahead of the current month so inserts never fall into the catch-all
	// default partition (M3). Default 3. Must be >= 1.
	PartitionsAhead int
}

const (
	defaultAnalyticsRetentionMonths = 24
	defaultAnalyticsPartitionsAhead = 3

	// analyticsDDLTimeout bounds each long-running partition/retention/refresh
	// DDL step. RDS Proxy does not honor a session statement_timeout, so a
	// runaway DDL (e.g. a CONCURRENTLY refresh blocked on a lock) could hang the
	// whole scheduled run indefinitely; a per-step deadline guarantees the
	// pipeline makes forward progress or fails fast (06-N3).
	analyticsDDLTimeout = 5 * time.Minute
)

// LoadAnalyticsConfig defaults unset/blank values and rejects malformed booleans.
// Integer validation remains in Validate.
func LoadAnalyticsConfig() (AnalyticsConfig, error) {
	enabled := true
	if raw := os.Getenv("ANALYTICS_COLLECTION_ENABLED"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return AnalyticsConfig{}, fmt.Errorf("invalid ANALYTICS_COLLECTION_ENABLED: %w", err)
		}
		enabled = value
	}
	return AnalyticsConfig{
		Enabled:         enabled,
		RetentionMonths: loadAnalyticsInt("ANALYTICS_RETENTION_MONTHS", defaultAnalyticsRetentionMonths),
		PartitionsAhead: loadAnalyticsInt("ANALYTICS_PARTITIONS_AHEAD", defaultAnalyticsPartitionsAhead),
	}, nil
}

// loadAnalyticsInt reads an integer collector knob from env. An unset/blank
// value falls back to defaultVal; a set-but-unparseable value is preserved as a
// 0 sentinel (rather than silently defaulting like getEnvInt) so Validate
// rejects the misconfiguration at startup instead of running with a default the
// operator never asked for (fail-fast at the boundary, feedback_strict_int_parse).
func loadAnalyticsInt(key string, defaultVal int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(raw)
	if err != nil {
		return 0 // out-of-range sentinel: Validate() rejects it (must be >= 1)
	}
	return val
}

// Validate rejects out-of-range analytics knobs so a misconfiguration fails
// fast at startup instead of silently producing a broken retention/partition
// policy at the first scheduled run.
func (c AnalyticsConfig) Validate() error {
	if c.RetentionMonths < 1 {
		return fmt.Errorf("ANALYTICS_RETENTION_MONTHS must be >= 1, got %d", c.RetentionMonths)
	}
	if c.PartitionsAhead < 1 {
		return fmt.Errorf("ANALYTICS_PARTITIONS_AHEAD must be >= 1, got %d", c.PartitionsAhead)
	}
	return nil
}

// withDDLTimeout runs an analytics DDL step that takes a single int arg under a
// bounded child context so it cannot hang the scheduled pipeline when no
// statement_timeout is enforced (e.g. under RDS Proxy, 06-N3).
func withDDLTimeout(ctx context.Context, fn func(context.Context, int) error, arg int) error {
	stepCtx, cancel := context.WithTimeout(ctx, analyticsDDLTimeout)
	defer cancel()
	return fn(stepCtx, arg)
}

// newAnalyticsCollector builds the savings-snapshot collector against the live
// DB connection. Kept here (not inline in reinitializeAfterConnect) so the
// app.go footprint is one localized call (eases the in-flight app.go rebase).
func newAnalyticsCollector(dbConn *database.Connection, configStore config.StoreInterface) (AnalyticsCollectorInterface, error) {
	store := analytics.NewPostgresAnalyticsStore(dbConn)
	return analytics.NewCollector(analytics.CollectorConfig{AnalyticsStore: store}, configStore)
}

// ensureCurrentAnalyticsPartition idempotently creates the savings_snapshots
// partition for the current UTC month (the month Collect stamps rows with)
// under a bounded DDL context (06-N3).
func (app *Application) ensureCurrentAnalyticsPartition(ctx context.Context) error {
	stepCtx, cancel := context.WithTimeout(ctx, analyticsDDLTimeout)
	defer cancel()
	return app.Analytics.CreatePartition(stepCtx, time.Now().UTC())
}

// handleCollectAnalytics runs the scheduled analytics pipeline end to end:
// ensure upcoming partitions exist, collect a snapshot across all tenants,
// apply retention, then refresh the materialized views over the fresh data.
// The current month's partition is a hard prerequisite: if it cannot be ensured
// the run aborts with an error before writing anything (#271). The remaining
// steps, including provisioning future partitions, are best-effort and
// recorded in the result so a single failure (e.g. a transient retention lock)
// doesn't abort the rest of the pipeline, but a hard failure still flips status
// to "partial" for observability.
func (app *Application) handleCollectAnalytics(ctx context.Context) (map[string]any, error) {
	cfg := app.appConfig.Analytics
	result := map[string]any{
		"status":             "success",
		"snapshots_written":  false,
		"partitions_ensured": false,
		"partitions_dropped": false,
		"views_refreshed":    false,
	}

	if !cfg.Enabled {
		result["status"] = "disabled"
		log.Println("Analytics collection disabled (ANALYTICS_COLLECTION_ENABLED=false)")
		return result, nil
	}
	if app.AnalyticsCollector == nil || app.Analytics == nil {
		result["status"] = "skipped"
		log.Println("Analytics collector/store not available, skipping collection")
		return result, nil
	}

	// 1a. Ensure the CURRENT month's partition exists BEFORE writing. This is
	//     the hard prerequisite: without it Collect would COPY every tenant's
	//     snapshot into savings_snapshots_default, which retention never drops,
	//     so abort and surface the error (#271). CreatePartition is idempotent,
	//     and Collect stamps rows with the current UTC time.
	if err := app.ensureCurrentAnalyticsPartition(ctx); err != nil {
		log.Printf("Error: failed to ensure current-month savings partition, skipping analytics collection: %v", err)
		result["status"] = "failed"
		return result, fmt.Errorf("analytics collection aborted: failed to ensure current-month partition: %w", err)
	}

	// 1b. Provision upcoming partitions ahead of time. Best-effort: today's
	//     snapshot already has a real partition, so a failure on a future month
	//     (lock timeout, or a stray future-dated row in the default partition
	//     blocking its creation) must not stop collection, otherwise the run
	//     would fail every day without ever healing. Logged loudly and flagged
	//     "partial" so it is visible.
	if err := withDDLTimeout(ctx, app.Analytics.CreateFuturePartitions, cfg.PartitionsAhead); err != nil {
		log.Printf("ERROR: failed to ensure future savings partitions (%d months ahead); "+
			"upcoming months' snapshots will land in savings_snapshots_default if their partition is still missing when they start: %v", cfg.PartitionsAhead, err)
		result["status"] = "partial"
	} else {
		result["partitions_ensured"] = true
	}

	// 2. Collect a snapshot across all tenants.
	if err := app.AnalyticsCollector.Collect(ctx); err != nil {
		// A canceled context is terminal: stop the pipeline and surface it.
		if ctx.Err() != nil {
			return result, fmt.Errorf("analytics collection canceled: %w", err)
		}
		log.Printf("Warning: analytics collection failed: %v", err)
		result["status"] = "partial"
	} else {
		result["snapshots_written"] = true
	}

	// 3. Apply retention.
	if err := withDDLTimeout(ctx, app.Analytics.DropOldPartitions, cfg.RetentionMonths); err != nil {
		log.Printf("Warning: failed to drop old partitions: %v", err)
		result["status"] = "partial"
	} else {
		result["partitions_dropped"] = true
	}

	// 4. Refresh materialized views over the fresh snapshot data.
	refreshCtx, cancelRefresh := context.WithTimeout(ctx, analyticsDDLTimeout)
	err := app.Analytics.RefreshMaterializedViews(refreshCtx)
	cancelRefresh()
	if err != nil {
		log.Printf("Warning: failed to refresh materialized views: %v", err)
		result["status"] = "partial"
	} else {
		result["views_refreshed"] = true
	}

	log.Printf("Analytics collection complete: %v", result)
	return result, nil
}
