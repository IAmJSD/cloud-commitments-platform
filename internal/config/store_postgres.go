package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// dbConn is the minimal interface used by PostgresStore.
// Both *database.Connection and pgxmock.PgxPoolIface satisfy this interface.
type dbConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PostgresStore implements StoreInterface using PostgreSQL.
type PostgresStore struct {
	db dbConn
}

// NewPostgresStore creates a new PostgreSQL-backed config store.
func NewPostgresStore(db *database.Connection) *PostgresStore {
	return &PostgresStore{db: db}
}

// Verify PostgresStore implements StoreInterface.
var _ StoreInterface = (*PostgresStore)(nil)

// ==========================================
// GLOBAL CONFIGURATION
// ==========================================

// globalConfigExecutor is the subset of query methods needed to read and write
// the global_config singleton. It is satisfied by both the pool (dbConn) and a
// pgx.Tx, so the read/write helpers can run either on the pool directly or
// inside the locked transaction opened by UpdateGlobalConfigAtomic.
type globalConfigExecutor interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// globalConfigLockKey is the advisory-lock key that serializes concurrent
// read-modify-write updates to the global_config singleton (updateConfig
// partial PUTs). Derived like the scheduled-task locks (FNV-64a of a
// namespaced string) so it cannot collide with those or the migration lock.
var globalConfigLockKey = func() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("cudly:global_config:singleton"))
	return int64(h.Sum64())
}()

var riExchangeDailySpendLockKey = func() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("cudly:ri_exchange:global_daily_spend"))
	return int64(h.Sum64())
}()

// GetGlobalConfig retrieves the global configuration.
func (s *PostgresStore) GetGlobalConfig(ctx context.Context) (*GlobalConfig, error) {
	return getGlobalConfigFrom(ctx, s.db)
}

// getGlobalConfigFrom reads the global_config singleton via q (the pool or a
// transaction), returning defaults when no row exists yet.
func getGlobalConfigFrom(ctx context.Context, q globalConfigExecutor) (*GlobalConfig, error) {
	query := `
		SELECT enabled_providers, notification_email, approval_required,
		       default_term, default_payment, default_coverage, default_ramp_schedule,
		       ri_exchange_enabled, ri_exchange_mode, ri_exchange_utilization_threshold,
		       ri_exchange_max_per_exchange_usd, ri_exchange_max_daily_usd, ri_exchange_lookback_days,
		       auto_collect, collection_schedule, notification_days_before,
		       grace_period_days,
		       recommendations_cache_stale_hours, recommendations_lookback_days,
		       COALESCE(purchase_delay_hours, 0),
		       COALESCE(laddering_enabled, false),
		       COALESCE(ladder_execution_enabled, false),
		       offering_class,
		       require_different_approver,
		       auto_purchase_enabled,
		       auto_purchase_changed_by, auto_purchase_changed_at, auto_purchase_changed_via,
		       ladder_execution_changed_by, ladder_execution_changed_at, ladder_execution_changed_via
		FROM global_config
		WHERE id = 1
	`

	var config GlobalConfig
	var enabledProviders []string
	var gracePeriodJSON string

	err := q.QueryRow(ctx, query).Scan(
		&enabledProviders,
		&config.NotificationEmail,
		&config.ApprovalRequired,
		&config.DefaultTerm,
		&config.DefaultPayment,
		&config.DefaultCoverage,
		&config.DefaultRampSchedule,
		&config.RIExchangeEnabled,
		&config.RIExchangeMode,
		&config.RIExchangeUtilizationThreshold,
		&config.RIExchangeMaxPerExchangeUSD,
		&config.RIExchangeMaxDailyUSD,
		&config.RIExchangeLookbackDays,
		&config.AutoCollect,
		&config.CollectionSchedule,
		&config.NotificationDaysBefore,
		&gracePeriodJSON,
		&config.RecommendationsCacheStaleHours,
		&config.RecommendationsLookbackDays,
		&config.PurchaseDelayHours,
		&config.LadderingEnabled,
		&config.LadderExecutionEnabled,
		&config.OfferingClass,
		&config.RequireDifferentApprover,
		&config.AutoPurchaseEnabled,
		&config.AutoPurchaseChangedBy,
		&config.AutoPurchaseChangedAt,
		&config.AutoPurchaseChangedVia,
		&config.LadderExecutionChangedBy,
		&config.LadderExecutionChangedAt,
		&config.LadderExecutionChangedVia,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Return default config if none exists.
			// Values must align with DefaultSettings in defaults.go and DB DEFAULT clauses.
			return &GlobalConfig{
				EnabledProviders:               []string{},
				ApprovalRequired:               true,
				DefaultTerm:                    3,
				DefaultPayment:                 "no-upfront",
				DefaultCoverage:                float64(DefaultCoveragePercent),
				DefaultRampSchedule:            RampImmediate,
				RIExchangeMode:                 "manual",
				RIExchangeUtilizationThreshold: 95.0,
				RIExchangeLookbackDays:         30,
				AutoCollect:                    true,
				CollectionSchedule:             "daily",
				NotificationDaysBefore:         3,
				RecommendationsCacheStaleHours: DefaultRecommendationsCacheStaleHours,
				RecommendationsLookbackDays:    DefaultRecommendationsLookbackDays,
				PurchaseDelayHours:             DefaultPurchaseDelayHours,
				OfferingClass:                  "convertible",
			}, nil
		}
		return nil, fmt.Errorf("failed to get global config: %w", err)
	}

	config.EnabledProviders = enabledProviders
	if gracePeriodJSON != "" && gracePeriodJSON != "{}" {
		var gp map[string]int
		if err := json.Unmarshal([]byte(gracePeriodJSON), &gp); err != nil {
			return nil, fmt.Errorf("failed to decode grace_period_days JSON: %w", err)
		}
		config.GracePeriodDays = gp
	}
	return &config, nil
}

// SaveGlobalConfig saves the global configuration.
func (s *PostgresStore) SaveGlobalConfig(ctx context.Context, config *GlobalConfig) error {
	return saveGlobalConfigWith(ctx, s.db, config)
}

// UpdateGlobalConfigAtomic performs a serialized read-modify-write of the
// global_config singleton. It opens a transaction, takes a transaction-scoped
// advisory lock (which serializes even the first insert, since a row-level
// FOR UPDATE cannot lock a not-yet-existing singleton row), reads the current
// config, applies the caller's in-place mutation, and upserts the result in
// the SAME transaction. This eliminates the lost-update race where two
// concurrent partial PUTs each read the same stale base and the later save
// silently drops the earlier change.
//
// apply mutates the loaded config in place (e.g. json.Unmarshal the request
// body over it and validate); an error it returns aborts the transaction and
// is propagated unchanged (so callers can surface a 400 from validation while
// transport/DB errors surface as 500).
func (s *PostgresStore) UpdateGlobalConfigAtomic(ctx context.Context, apply func(*GlobalConfig) error) (*GlobalConfig, error) {
	var merged *GlobalConfig
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, lockErr := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", globalConfigLockKey); lockErr != nil {
			return fmt.Errorf("failed to acquire global_config advisory lock: %w", lockErr)
		}
		cfg, readErr := getGlobalConfigFrom(ctx, tx)
		if readErr != nil {
			return readErr
		}
		before := *cfg
		if applyErr := apply(cfg); applyErr != nil {
			return applyErr
		}
		if saveErr := saveGlobalConfigWith(ctx, tx, cfg); saveErr != nil {
			return saveErr
		}
		if propErr := propagateChangedDefaults(ctx, tx, &before, cfg); propErr != nil {
			return propErr
		}
		merged = cfg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return merged, nil
}

// propagateChangedDefaults overwrites a service-config column only when its
// global default changed, so a round-tripped or partial PUT keeps per-service
// customisations.
func propagateChangedDefaults(ctx context.Context, tx pgx.Tx, before, after *GlobalConfig) error {
	termChanged := after.DefaultTerm != before.DefaultTerm
	paymentChanged := after.DefaultPayment != before.DefaultPayment
	coverageChanged := after.DefaultCoverage != before.DefaultCoverage
	rampChanged := after.DefaultRampSchedule != before.DefaultRampSchedule
	if !termChanged && !paymentChanged && !coverageChanged && !rampChanged {
		return nil
	}
	const query = `
		UPDATE service_configs SET
			term = CASE WHEN $1 THEN $2 ELSE term END,
			payment = CASE WHEN $3 THEN $4 ELSE payment END,
			coverage = CASE WHEN $5 THEN $6 ELSE coverage END,
			ramp_schedule = CASE WHEN $7 THEN $8 ELSE ramp_schedule END
	`
	if _, err := tx.Exec(ctx, query,
		termChanged, after.DefaultTerm,
		paymentChanged, after.DefaultPayment,
		coverageChanged, after.DefaultCoverage,
		rampChanged, after.DefaultRampSchedule,
	); err != nil {
		return fmt.Errorf("failed to propagate global defaults to service configs: %w", err)
	}
	return nil
}

// saveGlobalConfigWith upserts the global_config singleton via q (the pool or a
// transaction).
func saveGlobalConfigWith(ctx context.Context, q globalConfigExecutor, config *GlobalConfig) error {
	// Ensure EnabledProviders is never nil (empty slice is ok, nil is not)
	if config.EnabledProviders == nil {
		config.EnabledProviders = []string{}
	}

	query := `
		INSERT INTO global_config (
			id, enabled_providers, notification_email, approval_required,
			default_term, default_payment, default_coverage, default_ramp_schedule,
			ri_exchange_enabled, ri_exchange_mode, ri_exchange_utilization_threshold,
			ri_exchange_max_per_exchange_usd, ri_exchange_max_daily_usd, ri_exchange_lookback_days,
			auto_collect, collection_schedule, notification_days_before,
			grace_period_days,
			recommendations_cache_stale_hours, recommendations_lookback_days,
			purchase_delay_hours, laddering_enabled, ladder_execution_enabled, offering_class,
			require_different_approver, auto_purchase_enabled,
			auto_purchase_changed_by, auto_purchase_changed_at, auto_purchase_changed_via,
			ladder_execution_changed_by, ladder_execution_changed_at, ladder_execution_changed_via
		) VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31)
		ON CONFLICT (id) DO UPDATE SET
			enabled_providers = $1,
			notification_email = $2,
			approval_required = $3,
			default_term = $4,
			default_payment = $5,
			default_coverage = $6,
			default_ramp_schedule = $7,
			ri_exchange_enabled = $8,
			ri_exchange_mode = $9,
			ri_exchange_utilization_threshold = $10,
			ri_exchange_max_per_exchange_usd = $11,
			ri_exchange_max_daily_usd = $12,
			ri_exchange_lookback_days = $13,
			auto_collect = $14,
			collection_schedule = $15,
			notification_days_before = $16,
			grace_period_days = $17,
			recommendations_cache_stale_hours = $18,
			recommendations_lookback_days = $19,
			purchase_delay_hours = $20,
			laddering_enabled = $21,
			ladder_execution_enabled = $22,
			offering_class = $23,
			require_different_approver = $24,
			auto_purchase_enabled = $25,
			auto_purchase_changed_by = $26,
			auto_purchase_changed_at = $27,
			auto_purchase_changed_via = $28,
			ladder_execution_changed_by = $29,
			ladder_execution_changed_at = $30,
			ladder_execution_changed_via = $31,
			updated_at = NOW()
	`

	// Use local copies for defaults so we don't mutate the caller's struct.
	riExchangeMode := config.RIExchangeMode
	if riExchangeMode == "" {
		riExchangeMode = "manual"
	}
	riExchangeLookbackDays := config.RIExchangeLookbackDays
	if riExchangeLookbackDays == 0 {
		riExchangeLookbackDays = 30
	}
	recommendationsLookbackDays := config.RecommendationsLookbackDays
	if recommendationsLookbackDays == 0 {
		// Validate() treats 0 as "unset → use default". Mirror that here so
		// the DB never stores 0, matching the ErrNoRows default path.
		recommendationsLookbackDays = DefaultRecommendationsLookbackDays
	}
	riExchangeUtilizationThreshold := config.RIExchangeUtilizationThreshold
	if riExchangeUtilizationThreshold == 0 {
		riExchangeUtilizationThreshold = 95.0
	}

	// Marshal GracePeriodDays -> JSON text column. Empty map encodes as
	// "{}" so the DB column is never NULL and GetGlobalConfig can
	// treat "{}" and "" uniformly as "no explicit entries".
	gracePeriodJSON := "{}"
	if len(config.GracePeriodDays) > 0 {
		gpBytes, err := json.Marshal(config.GracePeriodDays)
		if err != nil {
			return fmt.Errorf("failed to encode grace_period_days JSON: %w", err)
		}
		gracePeriodJSON = string(gpBytes)
	}

	// Default offering_class to "convertible" when unset so the DB column
	// never stores an empty string (the NOT NULL DEFAULT 'convertible'
	// column handles inserts, but upserts overwrite with whatever we pass).
	offeringClass := config.OfferingClass
	if offeringClass == "" {
		offeringClass = "convertible"
	}

	_, err := q.Exec(ctx, query,
		config.EnabledProviders,
		config.NotificationEmail,
		config.ApprovalRequired,
		config.DefaultTerm,
		config.DefaultPayment,
		config.DefaultCoverage,
		config.DefaultRampSchedule,
		config.RIExchangeEnabled,
		riExchangeMode,
		riExchangeUtilizationThreshold,
		config.RIExchangeMaxPerExchangeUSD,
		config.RIExchangeMaxDailyUSD,
		riExchangeLookbackDays,
		config.AutoCollect,
		config.CollectionSchedule,
		config.NotificationDaysBefore,
		gracePeriodJSON,
		config.RecommendationsCacheStaleHours,
		recommendationsLookbackDays,
		config.PurchaseDelayHours,
		config.LadderingEnabled,
		config.LadderExecutionEnabled,
		offeringClass,
		config.RequireDifferentApprover,
		config.AutoPurchaseEnabled,
		config.AutoPurchaseChangedBy,
		config.AutoPurchaseChangedAt,
		config.AutoPurchaseChangedVia,
		config.LadderExecutionChangedBy,
		config.LadderExecutionChangedAt,
		config.LadderExecutionChangedVia,
	)

	if err != nil {
		return fmt.Errorf("failed to save global config: %w", err)
	}

	return nil
}

// ==========================================
// SERVICE CONFIGURATION
// ==========================================

// GetServiceConfig retrieves configuration for a specific service.
func (s *PostgresStore) GetServiceConfig(ctx context.Context, provider, service string) (*ServiceConfig, error) {
	query := `
		SELECT provider, service, enabled, term, payment, coverage, ramp_schedule,
		       include_engines, exclude_engines, include_regions, exclude_regions,
		       include_types, exclude_types, min_count
		FROM service_configs
		WHERE provider = $1 AND service = $2
	`

	var config ServiceConfig
	var includeEngines, excludeEngines, includeRegions, excludeRegions, includeTypes, excludeTypes []string

	err := s.db.QueryRow(ctx, query, provider, service).Scan(
		&config.Provider,
		&config.Service,
		&config.Enabled,
		&config.Term,
		&config.Payment,
		&config.Coverage,
		&config.RampSchedule,
		&includeEngines,
		&excludeEngines,
		&includeRegions,
		&excludeRegions,
		&includeTypes,
		&excludeTypes,
		&config.MinCount,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("service config not found for %s:%s: %w", provider, service, ErrNotFound)
		}
		return nil, fmt.Errorf("failed to get service config: %w", err)
	}

	// Map arrays (handle nil)
	config.IncludeEngines = includeEngines
	config.ExcludeEngines = excludeEngines
	config.IncludeRegions = includeRegions
	config.ExcludeRegions = excludeRegions
	config.IncludeTypes = includeTypes
	config.ExcludeTypes = excludeTypes

	return &config, nil
}

// SaveServiceConfig saves configuration for a service.
func (s *PostgresStore) SaveServiceConfig(ctx context.Context, config *ServiceConfig) error {
	query := `
		INSERT INTO service_configs (
			provider, service, enabled, term, payment, coverage, ramp_schedule,
			include_engines, exclude_engines, include_regions, exclude_regions,
			include_types, exclude_types, min_count
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (provider, service) DO UPDATE SET
			enabled = $3,
			term = $4,
			payment = $5,
			coverage = $6,
			ramp_schedule = $7,
			include_engines = $8,
			exclude_engines = $9,
			include_regions = $10,
			exclude_regions = $11,
			include_types = $12,
			exclude_types = $13,
			min_count = $14,
			updated_at = NOW()
	`

	_, err := s.db.Exec(ctx, query,
		config.Provider,
		config.Service,
		config.Enabled,
		config.Term,
		config.Payment,
		config.Coverage,
		config.RampSchedule,
		config.IncludeEngines,
		config.ExcludeEngines,
		config.IncludeRegions,
		config.ExcludeRegions,
		config.IncludeTypes,
		config.ExcludeTypes,
		config.MinCount,
	)

	if err != nil {
		return fmt.Errorf("failed to save service config: %w", err)
	}

	return nil
}

// ListServiceConfigs lists all service configurations.
//
// LIMIT 1000 caps the result set at three orders of magnitude above the
// realistic upper bound (each cloud has a bounded set of services, so the
// total is roughly (providers × service-types × per-service-variants),
// which stays under ~150 even with generous provider growth). The cap is
// defense-in-depth against a compromised admin inserting millions of rows
// and matches the sibling GetPendingExecutions limit.
func (s *PostgresStore) ListServiceConfigs(ctx context.Context) ([]ServiceConfig, error) {
	query := `
		SELECT provider, service, enabled, term, payment, coverage, ramp_schedule,
		       include_engines, exclude_engines, include_regions, exclude_regions,
		       include_types, exclude_types, min_count
		FROM service_configs
		ORDER BY provider, service
		LIMIT 1000
	`

	rows, err := s.db.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list service configs: %w", err)
	}
	defer rows.Close()

	configs := make([]ServiceConfig, 0)
	for rows.Next() {
		var config ServiceConfig
		var includeEngines, excludeEngines, includeRegions, excludeRegions, includeTypes, excludeTypes []string

		err := rows.Scan(
			&config.Provider,
			&config.Service,
			&config.Enabled,
			&config.Term,
			&config.Payment,
			&config.Coverage,
			&config.RampSchedule,
			&includeEngines,
			&excludeEngines,
			&includeRegions,
			&excludeRegions,
			&includeTypes,
			&excludeTypes,
			&config.MinCount,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan service config: %w", err)
		}

		config.IncludeEngines = includeEngines
		config.ExcludeEngines = excludeEngines
		config.IncludeRegions = includeRegions
		config.ExcludeRegions = excludeRegions
		config.IncludeTypes = includeTypes
		config.ExcludeTypes = excludeTypes

		configs = append(configs, config)
	}

	return configs, rows.Err()
}

// ==========================================
// PURCHASE PLANS
// ==========================================

// CreatePurchasePlan creates a new purchase plan.
func (s *PostgresStore) CreatePurchasePlan(ctx context.Context, plan *PurchasePlan) error {
	return s.createPurchasePlan(ctx, s.db, plan)
}

const purchasePlanSelectCols = `
	SELECT id, name, enabled, auto_purchase, notification_days_before,
	       services, ramp_schedule, created_at, updated_at,
	       next_execution_date, last_execution_date, last_notification_sent,
	       max_step_commitment
	FROM purchase_plans`

// scanPurchasePlanRow deserialises one purchase_plans row returned by QueryRow
// or the first row of a query with FOR UPDATE. Extracted so GetPurchasePlan
// and CompletePlanStep share the same scan logic.
func scanPurchasePlanRow(row pgx.Row) (*PurchasePlan, error) {
	var plan PurchasePlan
	var servicesJSON, rampScheduleJSON []byte
	var nextExecDate, lastExecDate, lastNotifSent sql.NullTime

	err := row.Scan(
		&plan.ID,
		&plan.Name,
		&plan.Enabled,
		&plan.AutoPurchase,
		&plan.NotificationDaysBefore,
		&servicesJSON,
		&rampScheduleJSON,
		&plan.CreatedAt,
		&plan.UpdatedAt,
		&nextExecDate,
		&lastExecDate,
		&lastNotifSent,
		&plan.MaxStepCommitment,
	)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(servicesJSON, &plan.Services); err != nil {
		return nil, fmt.Errorf("failed to unmarshal services: %w", err)
	}
	if err := json.Unmarshal(rampScheduleJSON, &plan.RampSchedule); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ramp_schedule: %w", err)
	}

	if nextExecDate.Valid {
		plan.NextExecutionDate = &nextExecDate.Time
	}
	if lastExecDate.Valid {
		plan.LastExecutionDate = &lastExecDate.Time
	}
	if lastNotifSent.Valid {
		plan.LastNotificationSent = &lastNotifSent.Time
	}
	return &plan, nil
}

// GetPurchasePlan retrieves a purchase plan by ID.
func (s *PostgresStore) GetPurchasePlan(ctx context.Context, planID string) (*PurchasePlan, error) {
	query := purchasePlanSelectCols + ` WHERE id = $1`
	plan, err := scanPurchasePlanRow(s.db.QueryRow(ctx, query, planID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: purchase plan %s", ErrNotFound, planID)
		}
		return nil, fmt.Errorf("failed to get purchase plan: %w", err)
	}
	return plan, nil
}

// CompletePlanStep records that ramp step stepNumber of planID completed and
// advances the schedule to it, inside a transaction. The row is locked with
// SELECT FOR UPDATE so concurrent callers (overlapping Lambda invocations,
// multi-tick cron) cannot both read the same CurrentStep value and both write
// CurrentStep+1, skipping a step (issue #1071).
//
// The operation is idempotent in stepNumber rather than a blind increment: a
// multi-account plan produces one execution per account per ramp step, and
// retrying two separately-failed accounts of the same step used to advance the
// ramp twice, so the plan reported itself a step further along than the
// commitment it had actually bought (issue #1669). Completing a step at or
// below CurrentStep therefore writes nothing and reports
// ErrRampStepAlreadyCounted.
//
// Completing a step more than one beyond CurrentStep is refused for the same
// reason in the other direction: the steps in between never completed, and
// advancing over them would silently overstate how much the plan has bought.
//
// Granularity: a step is complete when EVERY cloud account it fanned out to has
// bought, not when any one of them has (issue #1861) -- see
// requireRampStepBought. Until then the advance reports ErrRampStepIncomplete
// and the plan row is left untouched.
//
// Returns nil when the plan no longer exists (deleted between execution and
// progress update) so the caller is not penalized for a race it cannot control.
func (s *PostgresStore) CompletePlanStep(ctx context.Context, planID string, stepNumber int) error {
	if stepNumber <= 0 {
		return fmt.Errorf("cannot complete ramp step %d of plan %s: step numbers are 1-based", stepNumber, planID)
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		plan, err := s.LockPurchasePlanTx(ctx, tx, planID)
		if err != nil {
			return err
		}
		if plan == nil {
			return nil
		}

		// A finished ramp cannot be extended by a trailing execution, but it
		// still falls through to the write so next_execution_date always ends
		// up cleared (the invariant issue #1071 established).
		if !plan.RampSchedule.IsComplete() {
			if err := advanceRampStep(ctx, tx, plan, stepNumber); err != nil {
				return err
			}
		}
		return s.persistRampPosition(ctx, tx, plan)
	})
}

// LockPurchasePlanTx reads a purchase plan under a row lock held for the rest
// of tx. It is the single definition of "the per-plan ramp lock": every path
// that reads a plan's ramp position in order to write against it calls THIS
// method, so completions and creations serialize against each other rather than
// each racing on its own read. CompletePlanStep below is one caller; the
// create-planned-purchases handler is the other.
//
// Returns (nil, nil) when the plan does not exist, which callers must handle
// explicitly: CompletePlanStep treats it as a benign race it cannot control,
// the create path turns it into a 404.
func (s *PostgresStore) LockPurchasePlanTx(ctx context.Context, tx pgx.Tx, planID string) (*PurchasePlan, error) {
	plan, err := scanPurchasePlanRow(
		tx.QueryRow(ctx, purchasePlanSelectCols+` WHERE id = $1 FOR UPDATE`, planID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to lock purchase plan %s: %w", planID, err)
	}
	return plan, nil
}

// advanceRampStep moves plan's in-memory ramp position to stepNumber, or
// returns the sentinel naming why it may not. The plan row is already locked by
// the caller's transaction, so the read-modify-write cannot lose an update.
func advanceRampStep(ctx context.Context, tx pgx.Tx, plan *PurchasePlan, stepNumber int) error {
	switch {
	case plan.RampSchedule.CurrentStep == stepNumber:
		// A sibling of this very step advanced the ramp first. The step is
		// counted and nothing is wrong, so this stays distinguishable from the
		// case below: only that one is worth recording against the execution.
		return fmt.Errorf("%w: plan %s is already on ramp step %d",
			ErrRampStepCountedBySibling, plan.ID, stepNumber)
	case plan.RampSchedule.CurrentStep > stepNumber:
		return fmt.Errorf("%w: plan %s is on ramp step %d, past the completing step %d",
			ErrRampStepAlreadyCounted, plan.ID, plan.RampSchedule.CurrentStep, stepNumber)
	case plan.RampSchedule.CurrentStep != stepNumber-1:
		return fmt.Errorf("refusing to advance plan %s from ramp step %d to %d: step(s) %d-%d never completed",
			plan.ID, plan.RampSchedule.CurrentStep, stepNumber,
			plan.RampSchedule.CurrentStep+1, stepNumber-1)
	}
	if err := requireRampStepBought(ctx, tx, plan.ID, stepNumber); err != nil {
		return err
	}
	plan.RampSchedule.CurrentStep = stepNumber
	return nil
}

// persistRampPosition writes plan's ramp position back, repointing
// next_execution_date at the step after it (or clearing it on a finished ramp)
// and stamping the execution timestamps.
func (s *PostgresStore) persistRampPosition(ctx context.Context, tx pgx.Tx, plan *PurchasePlan) error {
	if plan.RampSchedule.DatesManaged {
		return s.persistManagedRampPosition(ctx, tx, plan)
	}
	if plan.RampSchedule.IsComplete() {
		plan.NextExecutionDate = nil
	} else {
		nextDate := plan.RampSchedule.GetNextPurchaseDate()
		plan.NextExecutionDate = &nextDate
	}

	now := time.Now()
	plan.LastExecutionDate = &now

	return s.UpdatePurchasePlanTx(ctx, tx, plan)
}

// UpdatePurchasePlan publishes the new version only after the transaction commits.
func (s *PostgresStore) UpdatePurchasePlan(ctx context.Context, plan *PurchasePlan) error {
	pending := *plan
	if err := s.WithTx(ctx, func(tx pgx.Tx) error {
		old, err := s.LockPurchasePlanTx(ctx, tx, pending.ID)
		if err != nil {
			return err
		}
		if updErr := s.updatePurchasePlanGuardedTx(ctx, tx, old, &pending); updErr != nil {
			return updErr
		}
		if old == nil || !rampBaselineStale(old, &pending) {
			return nil
		}
		// After the update, never before: clearing bumps updated_at, which
		// would fail the update's own optimistic-lock check.
		err = tx.QueryRow(ctx,
			`UPDATE purchase_plans SET ramp_baseline = NULL WHERE id = $1 AND ramp_baseline IS NOT NULL RETURNING updated_at`,
			pending.ID).Scan(&pending.UpdatedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("failed to clear ramp baseline of plan %s: %w", pending.ID, err)
		}
		return nil
	}); err != nil {
		return err
	}
	plan.UpdatedAt = pending.UpdatedAt
	return nil
}

// UpdatePurchasePlanTx rejects snapshots that lost a race with another plan
// writer and refuses changes to what the editor owns on a managed plan. It
// re-locks the row, which every caller already holds, to judge the guard on
// the locked version.
func (s *PostgresStore) UpdatePurchasePlanTx(ctx context.Context, tx pgx.Tx, plan *PurchasePlan) error {
	old, err := s.LockPurchasePlanTx(ctx, tx, plan.ID)
	if err != nil {
		return err
	}
	return s.updatePurchasePlanGuardedTx(ctx, tx, old, plan)
}

// updatePurchasePlanGuardedTx runs the managed-plan guard against old, the row
// locked by the caller's transaction, and then the optimistic update.
func (s *PostgresStore) updatePurchasePlanGuardedTx(ctx context.Context, tx pgx.Tx, old, plan *PurchasePlan) error {
	if err := requireManagedPlanUpdateTx(old, plan); err != nil {
		return err
	}
	// Marshal services and ramp_schedule to JSONB
	servicesJSON, err := json.Marshal(plan.Services)
	if err != nil {
		return fmt.Errorf("failed to marshal services: %w", err)
	}

	rampScheduleJSON, err := json.Marshal(plan.RampSchedule)
	if err != nil {
		return fmt.Errorf("failed to marshal ramp_schedule: %w", err)
	}

	query := `
		UPDATE purchase_plans SET
			name = $2,
			enabled = $3,
			auto_purchase = $4,
			notification_days_before = $5,
			services = $6,
			ramp_schedule = $7,
			next_execution_date = $9,
			last_execution_date = $10,
			last_notification_sent = $11,
			max_step_commitment = $12
		WHERE id = $1 AND updated_at = $8
		RETURNING updated_at
	`

	var updatedAt time.Time
	err = tx.QueryRow(ctx, query,
		plan.ID,
		plan.Name,
		plan.Enabled,
		plan.AutoPurchase,
		plan.NotificationDaysBefore,
		servicesJSON,
		rampScheduleJSON,
		plan.UpdatedAt,
		plan.NextExecutionDate,
		plan.LastExecutionDate,
		plan.LastNotificationSent,
		plan.MaxStepCommitment,
	).Scan(&updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if probeErr := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM purchase_plans WHERE id = $1)`, plan.ID).Scan(&exists); probeErr != nil {
			return fmt.Errorf("failed to check purchase plan after conflict: %w", probeErr)
		}
		if !exists {
			return fmt.Errorf("%w: purchase plan %s", ErrNotFound, plan.ID)
		}
		return ErrPurchasePlanConflict
	}
	if err != nil {
		return fmt.Errorf("failed to update purchase plan: %w", err)
	}
	plan.UpdatedAt = updatedAt
	return nil
}

// StampPlanNotificationSent never writes a snapshot of the plan's ramp state.
func (s *PostgresStore) StampPlanNotificationSent(ctx context.Context, planID string, at time.Time) error {
	result, err := s.db.Exec(ctx, `UPDATE purchase_plans SET last_notification_sent = $2 WHERE id = $1`, planID, at)
	if err != nil {
		return fmt.Errorf("failed to stamp plan notification: %w", err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("%w: purchase plan %s", ErrNotFound, planID)
	}
	return nil
}

// DeletePurchasePlan deletes a purchase plan.
func (s *PostgresStore) DeletePurchasePlan(ctx context.Context, planID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		plan, err := s.LockPurchasePlanTx(ctx, tx, planID)
		if err != nil {
			return err
		}
		if plan == nil {
			return fmt.Errorf("purchase plan not found: %s", planID)
		}
		if plan.RampSchedule.DatesManaged {
			if err := requireManagedDeleteTx(ctx, tx, planID); err != nil {
				return err
			}
		}
		if err := cancelOpenPlanExecutionsTx(ctx, tx, planID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM purchase_plans WHERE id = $1`, planID); err != nil {
			return fmt.Errorf("failed to delete purchase plan: %w", err)
		}
		return nil
	})
}

// cancelOpenPlanExecutionsTx cancels the plan's step rows that can still be
// approved or run (pending, notified, paused) in the delete's transaction.
// purchase_executions.plan_id is ON DELETE SET NULL (migration 000033), and
// token approval applies the plan's max_step_commitment and the approve-own
// plan-step rule only while plan_id is set (approveConstraintsForSession), so
// a row left open would become approvable past its plan's cap (issue #862).
// The rows' suppressions go with them, as on the other cancel paths.
func cancelOpenPlanExecutionsTx(ctx context.Context, tx pgx.Tx, planID string) error {
	const q = `
		WITH canceled AS (
			UPDATE purchase_executions
			   SET status = $2, updated_at = NOW()
			 WHERE plan_id = $1
			   AND status IN ('pending', 'notified', 'paused')
			RETURNING execution_id
		)
		DELETE FROM purchase_suppressions
		 WHERE execution_id IN (SELECT execution_id FROM canceled)`
	if _, err := tx.Exec(ctx, q, planID, StatusCanceled); err != nil {
		return fmt.Errorf("failed to cancel open executions of purchase plan: %w", err)
	}
	return nil
}

// buildListPlansQuery returns the SQL query and args for ListPurchasePlans.
//
// When accountIDs is empty the query returns all plans without an unassigned
// column (false by default for every plan).
//
// When accountIDs is non-empty the query uses a LEFT JOIN so that plans with
// zero rows in plan_accounts ("unassigned" legacy plans) are included
// alongside the matched-account plans. The boolean expression
// (NOT EXISTS (SELECT 1 FROM plan_accounts WHERE plan_id = pp.id)) is
// selected as the "unassigned" column: true for zero-account plans, false
// for every plan that has at least one plan_accounts row. This lets the
// caller bucket the two groups without a separate query.
//
// The WHERE clause retains the original account-filter semantics for
// assigned plans and adds OR NOT EXISTS ... to include unassigned ones only
// when includeUnassigned is set, so an account-scoped caller never receives
// plans no account owns (issue #29). DISTINCT prevents duplicates when a plan
// matches multiple account IDs.
func buildListPlansQuery(accountIDs []string, includeUnassigned bool) (query string, args []any) {
	if len(accountIDs) == 0 {
		return `
			SELECT id, name, enabled, auto_purchase, notification_days_before,
			       services, ramp_schedule, created_at, updated_at,
			       next_execution_date, last_execution_date, last_notification_sent,
			       max_step_commitment,
			       false AS unassigned
			FROM purchase_plans
			ORDER BY created_at DESC
		`, nil
	}
	placeholders := make([]string, len(accountIDs))
	args = make([]any, len(accountIDs))
	for i, id := range accountIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	unassignedClause := ""
	if includeUnassigned {
		unassignedClause = " OR NOT EXISTS (SELECT 1 FROM plan_accounts WHERE plan_id = pp.id)"
	}
	query = fmt.Sprintf(`
		SELECT DISTINCT pp.id, pp.name, pp.enabled, pp.auto_purchase, pp.notification_days_before,
		       pp.services, pp.ramp_schedule, pp.created_at, pp.updated_at,
		       pp.next_execution_date, pp.last_execution_date, pp.last_notification_sent,
		       pp.max_step_commitment,
		       (NOT EXISTS (SELECT 1 FROM plan_accounts WHERE plan_id = pp.id)) AS unassigned
		FROM purchase_plans pp
		LEFT JOIN plan_accounts pa ON pa.plan_id = pp.id
		WHERE pa.account_id IN (%s)%s
		ORDER BY pp.created_at DESC
	`, strings.Join(placeholders, ", "), unassignedClause)
	return query, args
}

// ListPurchasePlans lists purchase plans, optionally filtered by account IDs.
// When filter.AccountIDs is non-empty the result includes plans that
// reference at least one of the given accounts and, with
// filter.IncludeUnassigned, plans with zero plan_accounts rows (flagged with
// Unassigned=true). The no-filter case returns all plans with Unassigned=false.
func (s *PostgresStore) ListPurchasePlans(ctx context.Context, filter PurchasePlanFilter) ([]PurchasePlan, error) {
	query, args := buildListPlansQuery(filter.AccountIDs, filter.IncludeUnassigned)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list purchase plans: %w", err)
	}
	defer rows.Close()

	plans := make([]PurchasePlan, 0)
	for rows.Next() {
		var plan PurchasePlan
		var servicesJSON, rampScheduleJSON []byte
		var nextExecDate, lastExecDate, lastNotifSent sql.NullTime

		err := rows.Scan(
			&plan.ID,
			&plan.Name,
			&plan.Enabled,
			&plan.AutoPurchase,
			&plan.NotificationDaysBefore,
			&servicesJSON,
			&rampScheduleJSON,
			&plan.CreatedAt,
			&plan.UpdatedAt,
			&nextExecDate,
			&lastExecDate,
			&lastNotifSent,
			&plan.MaxStepCommitment,
			&plan.Unassigned,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan purchase plan: %w", err)
		}

		// Unmarshal JSONB fields
		if err := json.Unmarshal(servicesJSON, &plan.Services); err != nil {
			return nil, fmt.Errorf("failed to unmarshal services: %w", err)
		}

		if err := json.Unmarshal(rampScheduleJSON, &plan.RampSchedule); err != nil {
			return nil, fmt.Errorf("failed to unmarshal ramp_schedule: %w", err)
		}

		// Handle nullable timestamps
		if nextExecDate.Valid {
			plan.NextExecutionDate = &nextExecDate.Time
		}
		if lastExecDate.Valid {
			plan.LastExecutionDate = &lastExecDate.Time
		}
		if lastNotifSent.Valid {
			plan.LastNotificationSent = &lastNotifSent.Time
		}

		plans = append(plans, plan)
	}

	return plans, rows.Err()
}

// ==========================================
// PURCHASE EXECUTIONS
// ==========================================

// SavePurchaseExecution saves a purchase execution record.
func (s *PostgresStore) SavePurchaseExecution(ctx context.Context, execution *PurchaseExecution) error {
	// Generate the execution ID before we attempt to open a tx so
	// pre-existing tests (which passed a nil DB and relied on ID
	// generation as a side effect) still work as-is.
	if execution.ExecutionID == "" {
		execution.ExecutionID = uuid.New().String()
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		return s.SavePurchaseExecutionTx(ctx, tx, execution)
	})
}

// SavePurchaseExecutionTx is the tx-accepting variant of
// SavePurchaseExecution. Used from handlers that need to bundle the
// execution insert with other writes (e.g. purchase_suppressions rows)
// in a single atomic transaction.
func (s *PostgresStore) SavePurchaseExecutionTx(ctx context.Context, tx pgx.Tx, execution *PurchaseExecution) error {
	// Generate execution ID if not provided
	if execution.ExecutionID == "" {
		execution.ExecutionID = uuid.New().String()
	}

	// Stamp a stable idempotency lineage key on first creation (issue #1012).
	// Unlike ExecutionID this is generated once and then copied verbatim onto
	// Retry successors / multi-account fan-out rows by the callers, so the
	// derived provider token survives a re-drive. INSERT-only below (omitted
	// from the ON CONFLICT DO UPDATE SET), so an upsert of an existing row
	// never overwrites the key already persisted at creation.
	if execution.IdempotencyKey == "" {
		execution.IdempotencyKey = uuid.New().String()
	}

	// Marshal recommendations to JSONB
	recommendationsJSON, err := json.Marshal(execution.Recommendations)
	if err != nil {
		return fmt.Errorf("failed to marshal recommendations: %w", err)
	}

	// Capacity % defaults to 100 (= "bought the full recommendation")
	// for legacy / scheduler paths that don't set the field.
	capacityPercent := execution.CapacityPercent
	if capacityPercent <= 0 {
		capacityPercent = 100
	}

	query := `
		INSERT INTO purchase_executions (
			plan_id, execution_id, status, step_number, scheduled_date,
			notification_sent, approval_token_hash, recommendations,
			total_upfront_cost, estimated_savings, completed_at, error, expires_at,
			cloud_account_id, source, approved_by, cancelled_by, capacity_percent,
			created_by_user_id, retry_execution_id, retry_attempt_n,
			approval_token_expires_at,
			executed_by_user_id, executed_at, pre_approval_skip_reason,
			idempotency_key, scheduled_execution_at, no_submission, created_by_system
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29)
		ON CONFLICT (execution_id) DO UPDATE SET
			status = $3,
			notification_sent = $6,
			approval_token_hash = $7,
			recommendations = $8,
			total_upfront_cost = $9,
			estimated_savings = $10,
			completed_at = $11,
			error = $12,
			expires_at = $13,
			cloud_account_id = $14,
			source = $15,
			approved_by = $16,
			cancelled_by = $17,
			capacity_percent = $18,
			approval_token_expires_at = $22,
			executed_by_user_id = $23,
			executed_at = $24,
			pre_approval_skip_reason = $25,
			scheduled_execution_at = $27,
			no_submission = CASE WHEN $3 = 'failed'
				THEN purchase_executions.no_submission OR $28
				ELSE false END,
			updated_at = NOW()
	`

	// Direct-execute purchases (from the Recommendations page, no plan)
	// arrive with an empty PlanID. The column is UUID — pass nil rather
	// than the empty string so PostgreSQL stores NULL instead of trying
	// to parse "" as a UUID (which crashed the handler with a generic
	// 500). Migration 000033 relaxed the NOT NULL so this is safe.
	var planIDArg any
	if execution.PlanID != "" {
		planIDArg = execution.PlanID
	}

	// created_by_user_id, created_by_system and retry_attempt_n are INSERT-only: the original
	// creator and the retry-chain position must not be rewritten by an
	// ON CONFLICT update (e.g. the scheduler upserting status transitions).
	// They are omitted from the DO UPDATE SET clause above.
	//
	// retry_execution_id is also omitted from the DO UPDATE SET clause
	// (issue #220 CR follow-up): any other caller that upserts a stale
	// in-memory copy of a failed row (e.g. the reaper) through this same
	// path must not be able to clear a link LinkRetryExecutionAtomic
	// already wrote, which would defeat the already-retried guard and
	// let the row be retried a second time. The retry handler links the
	// original failed row to its successor exclusively through
	// LinkRetryExecutionAtomic's targeted, conditional UPDATE.
	_, err = tx.Exec(ctx, query,
		planIDArg,
		execution.ExecutionID,
		execution.Status,
		execution.StepNumber,
		execution.ScheduledDate,
		execution.NotificationSent,
		execution.ApprovalToken,
		recommendationsJSON,
		execution.TotalUpfrontCost,
		execution.EstimatedSavings,
		execution.CompletedAt,
		execution.Error,
		timeFromTTL(execution.TTL),
		execution.CloudAccountID,
		execution.Source,
		execution.ApprovedBy,
		execution.CancelledBy,
		capacityPercent,
		execution.CreatedByUserID,
		execution.RetryExecutionID,
		execution.RetryAttemptN,
		execution.ApprovalTokenExpiresAt,
		execution.ExecutedByUserID,
		execution.ExecutedAt,
		execution.PreApprovalSkipReason,
		execution.IdempotencyKey,
		execution.ScheduledExecutionAt,
		execution.NoSubmission,
		execution.CreatedBySystem,
	)

	if err != nil {
		return fmt.Errorf("failed to save purchase execution: %w", err)
	}

	return nil
}

// LinkRetryExecutionAtomic stamps retry_execution_id on a failed execution
// row, conditional on status = 'failed' AND retry_execution_id IS NULL,
// without touching any other column. This replaces a full-row
// SavePurchaseExecutionTx upsert of a stale in-memory copy of the failed
// row (issue #220): that upsert both raced two concurrent retries of the
// same row past the in-memory already-retried guard (both would insert an
// approvable successor and send an approval email) and reverted any column
// another writer changed on the failed row in between.
//
// Returns (true, nil) when this call won the race and wrote the linkage,
// (false, nil) when zero rows were affected because a concurrent retry
// already claimed the row (or it is no longer 'failed'), and (false, err)
// on a real DB error. Must be called inside the same tx that inserts the
// successor row, after that insert, so the FK on retry_execution_id is
// satisfied.
func (s *PostgresStore) LinkRetryExecutionAtomic(ctx context.Context, tx pgx.Tx, executionID, retryExecutionID string) (linked bool, err error) {
	q := `
		UPDATE purchase_executions
		   SET retry_execution_id = $2,
		       updated_at = NOW()
		 WHERE execution_id = $1
		   AND status = 'failed'
		   AND retry_execution_id IS NULL
	`
	tag, err := tx.Exec(ctx, q, executionID, retryExecutionID)
	if err != nil {
		return false, fmt.Errorf("failed to link retry execution: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RotatePendingApprovalToken implements StoreInterface. The same statement
// that stores the emailed token promotes the row to 'notified' (a no-op for a
// row already there), so the status never claims a notice that was not sent.
func (s *PostgresStore) RotatePendingApprovalToken(ctx context.Context, executionID, tokenHash string, expiresAt time.Time) (bool, error) {
	q := `
		UPDATE purchase_executions
		   SET approval_token_hash = $2,
		       approval_token_expires_at = $3,
		       status = 'notified',
		       updated_at = NOW()
		 WHERE execution_id = $1
		   AND status IN ('pending', 'notified')
	`
	tag, err := s.db.Exec(ctx, q, executionID, tokenHash, expiresAt)
	if err != nil {
		return false, fmt.Errorf("failed to rotate approval token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// TransitionExecutionStatus atomically transitions an execution from one of the
// allowed statuses to a new status. Returns the updated record, or an error if
// the execution was not found or not in an allowed status.
// actor is the UUID of the user performing the transition (nil for system-initiated paths);
// it is stamped onto transitioned_by and transitioned_at is always set to NOW().
// A transition to StatusCanceled also stamps the actor's email onto canceled_by
// and the legacy cancelled_by (issue #119), matching what the other cancel paths
// record and what History reads.
func (s *PostgresStore) TransitionExecutionStatus(ctx context.Context, executionID string, fromStatuses []string, toStatus string, actor *string) (*PurchaseExecution, error) {
	return s.transitionExecutionStatus(ctx, executionID, fromStatuses, toStatus, actor, nil, nil)
}

// TransitionExecutionStatusPinned is TransitionExecutionStatus that also
// requires the row's recommendations to still be empty (or not empty), as the
// caller saw them. A recommendation attach landing between a gate check and the
// claim makes the pin fail with ErrExecutionNotInExpectedStatus, so a row is
// never run on a snapshot different from the one that was gated.
func (s *PostgresStore) TransitionExecutionStatusPinned(ctx context.Context, executionID string, fromStatuses []string, toStatus string, actor *string, sawEmptyRecommendations bool) (*PurchaseExecution, error) {
	return s.transitionExecutionStatus(ctx, executionID, fromStatuses, toStatus, actor, &sawEmptyRecommendations, nil)
}

// TransitionExecutionStatusTokenPinned is TransitionExecutionStatus that also
// requires approval_token_hash to equal the hash of rawToken and the token to
// be unexpired, checked in the same UPDATE as the status change. The caller's
// earlier token check ran against a snapshot; a re-notification that rotates
// the token between that read and this write would otherwise still let the
// old token win the transition (issue #590). An empty rawToken never matches.
func (s *PostgresStore) TransitionExecutionStatusTokenPinned(ctx context.Context, executionID string, fromStatuses []string, toStatus string, actor *string, rawToken string) (*PurchaseExecution, error) {
	if rawToken == "" {
		return nil, fmt.Errorf("%w: execution %s: no approval token supplied", ErrApprovalTokenChanged, executionID)
	}
	tokenHash := HashApprovalToken(rawToken)
	return s.transitionExecutionStatus(ctx, executionID, fromStatuses, toStatus, actor, nil, &tokenHash)
}

// RescheduleAwaitingExecution is a conditional single-column update: the
// status predicate makes a lost race a benign no-op instead of an overwrite.
func (s *PostgresStore) RescheduleAwaitingExecution(ctx context.Context, executionID string, scheduledDate time.Time) (bool, error) {
	var planID *string
	err := s.db.QueryRow(ctx, `SELECT plan_id::text FROM purchase_executions WHERE execution_id = $1`, executionID).Scan(&planID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reschedule execution %s: %w", executionID, err)
	}
	updated := false
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if planID != nil {
			plan, lockErr := s.LockPurchasePlanTx(ctx, tx, *planID)
			if lockErr != nil {
				return lockErr
			}
			if plan != nil && plan.RampSchedule.DatesManaged {
				return fmt.Errorf("%w: reschedule of execution %s", ErrEditorReconciliationRequired, executionID)
			}
		}
		tag, execErr := tx.Exec(ctx, `
			UPDATE purchase_executions SET scheduled_date = $2, updated_at = NOW()
			WHERE execution_id = $1 AND status IN ('pending', 'notified')`, executionID, scheduledDate)
		updated = execErr == nil && tag.RowsAffected() == 1
		return execErr
	})
	if err != nil {
		return false, fmt.Errorf("reschedule execution %s: %w", executionID, err)
	}
	return updated, nil
}

func (s *PostgresStore) transitionExecutionStatus(ctx context.Context, executionID string, fromStatuses []string, toStatus string, actor *string, sawEmpty *bool, tokenHash *string) (*PurchaseExecution, error) {
	var cancelActor *string
	if toStatus == StatusCanceled {
		cancelActor = actor
	}
	query := `
		UPDATE purchase_executions
		SET status = $2, updated_at = NOW(),
		    transitioned_by = $4, transitioned_at = NOW(),
		    canceled_by = COALESCE((SELECT email FROM users WHERE id = $5), canceled_by),
		    cancelled_by = COALESCE((SELECT email FROM users WHERE id = $5), cancelled_by)
		WHERE execution_id = $1 AND status = ANY($3)
		  AND ($6::boolean IS NULL OR (recommendations IN ('[]'::jsonb, 'null'::jsonb)) = $6)
		  AND ($7::text IS NULL OR (approval_token_hash = $7
		       AND (approval_token_expires_at IS NULL OR approval_token_expires_at > NOW())))
		RETURNING plan_id, execution_id, status, step_number, scheduled_date,
		          notification_sent, approval_token_hash, recommendations,
		          total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		          cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		          created_by_user_id, retry_execution_id, retry_attempt_n,
		          approval_token_expires_at,
		          executed_by_user_id, executed_at, pre_approval_skip_reason,
		          idempotency_key, scheduled_execution_at, no_submission, created_by_system
	`

	records, err := s.queryExecutions(ctx, query, executionID, toStatus, fromStatuses, actor, cancelActor, sawEmpty, tokenHash)
	if err != nil {
		return nil, err
	}

	if len(records) == 0 {
		return nil, s.transitionRejection(ctx, executionID, fromStatuses, toStatus, tokenHash != nil)
	}

	return &records[0], nil
}

// transitionRejection explains a zero-row transitionExecutionStatus by probing
// the row: gone (ErrNotFound), a hard DB error, a token pin lost on a row still
// in an allowed status (ErrApprovalTokenChanged), or a status CAS lost
// (ErrExecutionNotInExpectedStatus). Extracted to keep the caller under the
// gocyclo threshold.
func (s *PostgresStore) transitionRejection(ctx context.Context, executionID string, fromStatuses []string, toStatus string, tokenPinned bool) error {
	existing, existErr := s.GetExecutionByID(ctx, executionID)
	if errors.Is(existErr, ErrNotFound) {
		// Wrap ErrNotFound so callers (e.g. the purchase reaper) can
		// use errors.Is to distinguish "row vanished mid-flight" — a
		// legitimate CAS race-loss — from a hard DB error.
		return fmt.Errorf("%w: execution %s", ErrNotFound, executionID)
	}
	if existErr != nil {
		// A hard DB error during the probe must NOT read as a benign
		// race-loss: propagate it so callers see the outage.
		return fmt.Errorf("transition %s: probe after zero-row CAS failed: %w", executionID, existErr)
	}
	// The status still allowed the transition, so the token pin is what
	// rejected it: the stored token was rotated or expired (issue #590).
	if tokenPinned && slices.Contains(fromStatuses, existing.Status) {
		return fmt.Errorf("%w: execution %s", ErrApprovalTokenChanged, executionID)
	}
	// Wrap ErrExecutionNotInExpectedStatus so callers can use
	// errors.Is to recognize CAS rejection (status changed between
	// SELECT and UPDATE) as race-lost rather than a real error.
	return fmt.Errorf("%w: execution %s cannot transition from %q to %q", ErrExecutionNotInExpectedStatus, executionID, existing.Status, toStatus)
}

// SetCancelledBy stamps both the canceled_by and legacy cancelled_by columns
// for a single execution without touching any other column. This avoids the
// full-row overwrite that a SavePurchaseExecution follow-up would perform,
// eliminating the lost-update window between TransitionExecutionStatus and
// the attribution write (Finding #5). Writing both columns keeps this path
// symmetric with CancelExecutionAtomic/CancelScheduledExecutionAtomic (which
// write canceled_by only) so revoke-attribution keeps working unchanged if a
// future contract migration (#1278) drops the legacy column.
func (s *PostgresStore) SetCancelledBy(ctx context.Context, executionID, cancelledBy string) error {
	q := `UPDATE purchase_executions SET canceled_by = $2, cancelled_by = $2, updated_at = NOW()
	       WHERE execution_id = $1`
	_, err := s.db.Exec(ctx, q, executionID, cancelledBy)
	return err
}

// CancelExecutionAtomic atomically transitions an execution from
// pending or notified to 'canceled' (canonical US spelling), setting
// canceled_by to the supplied actor (NULL when actor is nil). The UPDATE
// is conditional on
// status IN ('pending','notified') so a concurrent approve that has
// already transitioned the row to 'approved' causes zero rows to be
// affected and the method returns (false, currentStatus, nil) with the
// live status fetched via a follow-up SELECT. Returns (true, "canceled",
// nil) on success and (false, "", err) on a real DB error.
//
// The 'scheduled' status is intentionally NOT accepted here -- the
// Gmail-style pre-fire delay revoke flow has its own
// CancelScheduledExecutionAtomic so the two paths surface distinct CAS
// race outcomes (cancel returns 409 on already-approved; scheduled-revoke
// returns 410 "window closed" on already-fired).
//
// Callers must run the suppression cleanup in the same transaction; use
// the WithTx + DeleteSuppressionsByExecutionTx pairing at the call site
// exactly as the old SavePurchaseExecutionTx path did, except now the
// status guard is inside the UPDATE rather than checked optimistically
// before entering the tx.
func (s *PostgresStore) CancelExecutionAtomic(ctx context.Context, tx pgx.Tx, executionID string, cancelledBy *string) (canceled bool, currentStatus string, err error) {
	// StatusCanceled is the canonical US-spelling value; see types.go.
	q := `
		UPDATE purchase_executions
		   SET status       = $3,
		       canceled_by  = $2,
		       updated_at   = NOW()
		 WHERE execution_id = $1
		   AND status IN ('pending', 'notified')
		RETURNING status
	`
	rows, err := tx.Query(ctx, q, executionID, cancelledBy, StatusCanceled)
	if err != nil {
		return false, "", fmt.Errorf("failed to cancel execution: %w", err)
	}
	defer rows.Close()

	if rows.Next() {
		var st string
		if scanErr := rows.Scan(&st); scanErr != nil {
			return false, "", fmt.Errorf("failed to scan cancel result: %w", scanErr)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return false, "", fmt.Errorf("failed to iterate cancel result: %w", rowsErr)
		}
		return true, st, nil
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return false, "", fmt.Errorf("failed to iterate cancel result: %w", rowsErr)
	}

	// Zero rows affected: execution either does not exist or has already
	// transitioned out of pending/notified. Surface the current status so
	// callers can return a meaningful 409 body.
	existing, existErr := s.GetExecutionByID(ctx, executionID)
	if existErr != nil {
		return false, "", fmt.Errorf("execution not found or db error: %w", existErr)
	}
	return false, existing.Status, nil
}

// CancelScheduledExecutionAtomic atomically transitions an execution from
// 'scheduled' to 'canceled' (canonical US spelling), setting canceled_by to
// the supplied actor (NULL when actor is nil). Used by the Gmail-style
// pre-fire delay revoke path (issue #290 / #291 wave-2): an
// approved-but-not-yet-fired execution can be revoked at $0 by flipping it
// to canceled before the scheduler fires the cloud SDK call.
//
// The 'scheduled' status is the only accepted source. A concurrent
// scheduler tick that already transitioned the row to 'approved' or
// 'running' causes zero rows to be affected and the method returns
// (false, currentStatus, nil) -- the caller maps that to a 410
// ("revocation window has closed") so the frontend can fall through to
// the post-execution Azure direct-cancel API path.
//
// Returns (true, StatusCanceled, nil) on success and (false, "", err) on a
// real DB error. Must be called inside a WithTx block so the suppression
// cleanup commits atomically with the status flip.
func (s *PostgresStore) CancelScheduledExecutionAtomic(ctx context.Context, tx pgx.Tx, executionID string, cancelledBy *string) (canceled bool, currentStatus string, err error) {
	// StatusCanceled is the canonical US-spelling value; see types.go.
	q := `
		UPDATE purchase_executions
		   SET status       = $3,
		       canceled_by  = $2,
		       updated_at   = NOW()
		 WHERE execution_id = $1
		   AND status = 'scheduled'
		RETURNING status
	`
	rows, err := tx.Query(ctx, q, executionID, cancelledBy, StatusCanceled)
	if err != nil {
		return false, "", fmt.Errorf("failed to cancel scheduled execution: %w", err)
	}
	defer rows.Close()

	if rows.Next() {
		var st string
		if scanErr := rows.Scan(&st); scanErr != nil {
			return false, "", fmt.Errorf("failed to scan cancel-scheduled result: %w", scanErr)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return false, "", fmt.Errorf("failed to iterate cancel-scheduled result: %w", rowsErr)
		}
		return true, st, nil
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return false, "", fmt.Errorf("failed to iterate cancel-scheduled result: %w", rowsErr)
	}

	// Zero rows affected: execution either does not exist or the scheduler
	// has already transitioned it out of 'scheduled'. Surface the current
	// status so the caller can return a meaningful 410 body.
	existing, existErr := s.GetExecutionByID(ctx, executionID)
	if existErr != nil {
		return false, "", fmt.Errorf("execution not found or db error: %w", existErr)
	}
	return false, existing.Status, nil
}

// GetExecutionsByStatuses returns executions whose Status is any of the
// supplied values, newest-first, capped at `limit`. Used by the History
// handler to merge pending/failed/expired rows alongside completed purchases
// without changing the narrower GetPendingExecutions contract the scheduler
// depends on.
//
// Pending rows dated in the future are the one exception to newest-first
// (issue #680): plan creation writes every ramp step up front as a pending
// row, up to 52 per plan, so about 20 plans fill MaxListLimit with steps
// months ahead and a plain scheduled_date DESC would cut off a pending
// approval due today. Those future pending steps sort after every other row,
// and among themselves soonest first, so the cap drops the farthest steps.
// No other status is affected: a notified, scheduled or in-flight row is
// actionable whatever its date, and terminal statuses never match the
// pending predicate, so their order is unchanged.
//
// Executions with status 'completed' and an empty error are excluded: they are
// already represented by their purchase_history rows, so returning them would
// only consume the cap (the History handler skips them anyway).
func (s *PostgresStore) GetExecutionsByStatuses(ctx context.Context, statuses []string, limit int) ([]PurchaseExecution, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	query := `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE status = ANY($1)
		  AND NOT (status = 'completed' AND COALESCE(error, '') = '')
		ORDER BY (status = 'pending' AND scheduled_date > now()) ASC,
		         CASE WHEN status = 'pending' AND scheduled_date > now() THEN scheduled_date END ASC,
		         scheduled_date DESC
		LIMIT $2
	`
	return s.queryExecutions(ctx, query, statuses, limit)
}

// CountExecutionsByPlanAndStatus returns, keyed by plan ID, the exact number
// of executions in each of the supplied statuses that last changed state at
// or after `since`.
//
// Aggregated with GROUP BY rather than counted from a GetExecutionsByStatuses
// page: that method's DESC + LIMIT truncation would silently understate any
// plan whose executions fall outside the newest `limit` rows, which for the
// plan health score means an unhealthy plan quietly renders as healthy once
// newer rows from other plans push its failures out of the window. The result
// set here is bounded by plans x statuses, not by execution volume, so it
// needs no limit of its own.
//
// The window is on updated_at, NOT scheduled_date, because for the terminal
// statuses this method exists to count those are different dates pointing in
// opposite directions. A plan's executions are created up front for the whole
// ramp, so a pending row carries a scheduled_date months in the FUTURE;
// canceling it (CancelExecutionAtomic) leaves that future date untouched.
// Windowing on scheduled_date would therefore count a purchase cancelled
// today under a date next year, and answer "how many rows are scheduled
// recently-or-later" instead of "how many changed state recently".
// updated_at is stamped by CancelExecutionAtomic and
// TransitionExecutionStatus, and backstopped by the
// update_purchase_executions_updated_at trigger, so it is when the row
// actually entered the status being counted.
//
// plan_id has been nullable since migration 000033 (direct-execute purchases
// from the Recommendations page have no originating plan, and deleting a plan
// SET NULLs its executions). Those rows belong to no plan and are excluded in
// SQL; the scan still goes through sql.NullString so a NULL can never turn a
// per-plan count into a scan error that blanks the score for every plan.
func (s *PostgresStore) CountExecutionsByPlanAndStatus(ctx context.Context, statuses []string, since time.Time) (map[string]ExecutionStatusCounts, error) {
	counts := make(map[string]ExecutionStatusCounts)
	if len(statuses) == 0 {
		return counts, nil
	}

	rows, err := s.db.Query(ctx, `
		SELECT plan_id, status, COUNT(*)
		FROM purchase_executions
		WHERE status = ANY($1)
		  AND plan_id IS NOT NULL
		  AND updated_at >= $2
		GROUP BY plan_id, status
	`, statuses, since)
	if err != nil {
		return nil, fmt.Errorf("failed to count executions by plan and status: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var planID sql.NullString
		var status string
		var n int
		if scanErr := rows.Scan(&planID, &status, &n); scanErr != nil {
			return nil, fmt.Errorf("failed to scan execution status count: %w", scanErr)
		}
		if !planID.Valid {
			continue
		}
		if counts[planID.String] == nil {
			counts[planID.String] = ExecutionStatusCounts{}
		}
		counts[planID.String][status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate execution status counts: %w", err)
	}
	return counts, nil
}

// GetPlannedExecutions returns executions whose Status is any of the supplied
// values, ordered by scheduled_date ASC (soonest first), capped at `limit`.
// Used by the Planned Purchases handler where the user expects to act on
// imminent rows first.
//
// Distinct from GetExecutionsByStatuses (DESC + LIMIT for History's
// newest-first semantics): when total rows exceed `limit`, a DESC truncation
// drops the soonest rows, exactly the ones this list must surface. Sorting
// the already-truncated subset in-memory cannot recover them.
//
// Secondary sort by id ASC keeps ordering stable when multiple rows share a
// scheduled_date. NULLS LAST is defensive: the schema makes scheduled_date
// NOT NULL today, but the clause guards against a future relaxation silently
// hiding rows at the top of the list.
func (s *PostgresStore) GetPlannedExecutions(ctx context.Context, statuses []string, limit int) ([]PurchaseExecution, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	query := `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE status = ANY($1)
		ORDER BY scheduled_date ASC NULLS LAST, id ASC
		LIMIT $2
	`
	return s.queryExecutions(ctx, query, statuses, limit)
}

// GetStaleApprovedExecutions returns executions stuck in the "approved" status
// whose last update is older than olderThan. These are executions that were
// flipped to "approved" by ApproveAndExecute but whose synchronous purchase run
// never finalized (Lambda timeout, cold-start eviction, panic) — issue #632.
// updated_at is stamped to NOW() at the moment of the approved transition (see
// TransitionExecutionStatus) and is not touched again unless the run finalizes,
// so it is the age of the strand. Mirrors GetStaleProcessingExchanges.
func (s *PostgresStore) GetStaleApprovedExecutions(ctx context.Context, olderThan time.Duration) ([]PurchaseExecution, error) {
	query := `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE status = 'approved' AND updated_at < NOW() - $1::interval
	`
	return s.queryExecutions(ctx, query, fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
}

// ListStuckExecutions returns purchase executions whose Status is any of the
// supplied values and whose updated_at is older than the given duration. Used
// by the reaper sweep (issue #678) to find executions stuck in
// approved/running long enough that the synchronous executor has clearly
// failed without flipping the row to a terminal state.
//
// Returns rows oldest-first (ORDER BY updated_at ASC) so the longest-stuck
// rows are processed first within a single sweep, capped at MaxListLimit so
// an unbounded backlog doesn't blow up the Lambda's memory budget. The reaper
// invokes the sweep periodically; a backlog larger than MaxListLimit just
// gets drained across successive invocations.
//
// olderThan must be > 0; a zero/negative value would invert the WHERE clause
// into "updated_at < NOW() + |olderThan|" and reap fresh rows. Defense-in-
// depth: the caller (ParseReapAfterFromEnv) also rejects non-positive env
// values.
//
// olderThan is passed as a Postgres interval (seconds) so the comparison
// happens server-side against NOW() — keeping the cutoff in the DB clock
// avoids any drift between the API process and the database.
func (s *PostgresStore) ListStuckExecutions(ctx context.Context, statuses []string, olderThan time.Duration) ([]PurchaseExecution, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	if olderThan <= 0 {
		return nil, fmt.Errorf("ListStuckExecutions: olderThan must be > 0, got %s", olderThan)
	}
	query := `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE status = ANY($1)
		  AND updated_at < NOW() - $2::interval
		ORDER BY updated_at ASC
		LIMIT $3
	`
	intervalArg := fmt.Sprintf("%d seconds", int(olderThan.Seconds()))
	return s.queryExecutions(ctx, query, statuses, intervalArg, MaxListLimit)
}

// GetPendingExecutions retrieves all pending purchase executions.
func (s *PostgresStore) GetPendingExecutions(ctx context.Context) ([]PurchaseExecution, error) {
	query := `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE status IN ('pending', 'notified')
		  AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY scheduled_date ASC
		LIMIT 1000
	`

	return s.queryExecutions(ctx, query)
}

// GetPendingExecutionsTx is the tx-accepting variant of GetPendingExecutions.
// Running the read inside the same transaction as the subsequent insert makes
// duplicate-detection and execution creation atomic, closing the TOCTOU race
// in executePurchase (issue #643).
func (s *PostgresStore) GetPendingExecutionsTx(ctx context.Context, tx pgx.Tx) ([]PurchaseExecution, error) {
	const query = `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE status IN ('pending', 'notified')
		  AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY scheduled_date ASC
		LIMIT 1000
		FOR UPDATE
	`
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query pending executions in tx: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// planStepProviderLockPrefix namespaces the per-provider plan-step advisory lock.
const planStepProviderLockPrefix = "cudly:planstep:"

// planStepLockKeys returns the advisory-lock keys for providers: validated,
// de-duplicated and sorted. Sorted acquisition keeps two callers with
// overlapping provider sets from deadlocking on each other.
func planStepLockKeys(providers []string) ([]string, error) {
	if len(providers) == 0 {
		return nil, errors.New("lock plan providers: no providers given")
	}
	keys := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		if p == "" {
			return nil, errors.New("lock plan providers: empty provider name")
		}
		if _, dup := seen[p]; !dup {
			seen[p] = struct{}{}
			keys = append(keys, planStepProviderLockPrefix+p)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// LockPlanProvidersTx implements StoreInterface.LockPlanProvidersTx.
func (s *PostgresStore) LockPlanProvidersTx(ctx context.Context, tx pgx.Tx, providers []string) error {
	keys, err := planStepLockKeys(providers)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key); err != nil {
			return fmt.Errorf("lock plan step key %s: %w", key, err)
		}
	}
	return nil
}

// submitDedupeStatuses are the execution states that still stand for a
// commitment the submitter asked for: awaiting approval, being bought, or
// bought. A failed, expired or canceled row bought nothing, so a resubmit
// after one of those must create a fresh execution.
const submitDedupeStatuses = "'pending', 'notified', 'approved', 'running', 'completed', 'partially_completed'"

// ListRecentSubmitsTx serializes concurrent submits by creatorID and returns
// that creator's web-submitted executions scheduled at or after since, in
// every state in submitDedupeStatuses. The transaction-scoped advisory lock is
// held until tx ends, so a second submit by the same creator waits here, then
// reads the first one's committed row and can collapse onto it. SELECT ...
// FOR UPDATE could not do this: it locks only rows that already exist, so two
// concurrent submits both saw nothing and both inserted. The status set is
// wider than pending/notified because a direct execute moves its row to
// approved/running/completed within seconds, and a retry arriving then must
// still be recognized. An empty creatorID matches rows with no creator.
func (s *PostgresStore) ListRecentSubmitsTx(ctx context.Context, tx pgx.Tx, creatorID string, since time.Time) ([]PurchaseExecution, error) {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "purchase-submit:"+creatorID); err != nil {
		return nil, fmt.Errorf("failed to lock purchase submits: %w", err)
	}
	var creator *string
	if creatorID != "" {
		creator = &creatorID
	}
	query := `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE source = $3
		  AND status IN (` + submitDedupeStatuses + `)
		  AND scheduled_date >= $1
		  AND created_by_user_id IS NOT DISTINCT FROM $2::uuid
		ORDER BY scheduled_date ASC
	`
	rows, err := tx.Query(ctx, query, since, creator, common.PurchaseSourceWeb)
	if err != nil {
		return nil, fmt.Errorf("failed to query recent submits in tx: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// GetExecutionByID retrieves a purchase execution by execution ID.
// Returns an error wrapping ErrNotFound when no row matches executionID so
// callers can cleanly distinguish "not found" (errors.Is(err, ErrNotFound))
// from a real DB failure (any other non-nil error). A nil error guarantees
// a non-nil execution (fail-loud contract; issues #976, #1339).
func (s *PostgresStore) GetExecutionByID(ctx context.Context, executionID string) (*PurchaseExecution, error) {
	query := `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE execution_id = $1
	`

	executions, err := s.queryExecutions(ctx, query, executionID)
	if err != nil {
		return nil, err
	}

	if len(executions) == 0 {
		return nil, fmt.Errorf("%w: execution %s", ErrNotFound, executionID)
	}

	return &executions[0], nil
}

// GetExecutionByPlanAndDate retrieves execution for a specific plan and date.
func (s *PostgresStore) GetExecutionByPlanAndDate(ctx context.Context, planID string, scheduledDate time.Time) (*PurchaseExecution, error) {
	query := `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE plan_id = $1 AND scheduled_date = $2
	`

	executions, err := s.queryExecutions(ctx, query, planID, scheduledDate)
	if err != nil {
		return nil, err
	}

	if len(executions) == 0 {
		return nil, fmt.Errorf("%w: plan %s at %v", ErrNotFound, planID, scheduledDate)
	}

	return &executions[0], nil
}

// GetUserEmailByID resolves the email address of the auth user identified by
// userID. The `users` table belongs to internal/auth's schema, but internal/
// auth already imports internal/config (service_mfa.go), so internal/config
// cannot import internal/auth back without a cycle. This method queries the
// shared database directly by table/column name instead, returning a plain
// string so no auth.User type crosses the package boundary. Returns ("", nil)
// when userID does not resolve to a row -- callers must treat that as
// "identity unresolved," not as a legitimately blank email.
func (s *PostgresStore) GetUserEmailByID(ctx context.Context, userID string) (string, error) {
	var email string
	err := s.db.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("failed to get user email: %w", err)
	}
	return email, nil
}

// CountPendingExecutionsForAccount returns the number of pending/notified
// purchase executions still referencing this cloud account. The deleteAccount
// handler calls this before issuing DELETE FROM cloud_accounts so it can
// short-circuit with a 409 instead of letting migration 000053's ON DELETE
// RESTRICT bubble up as an opaque FK-violation error. See issue #606.
func (s *PostgresStore) CountPendingExecutionsForAccount(ctx context.Context, accountID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM purchase_executions
		WHERE cloud_account_id = $1
		  AND status IN ('pending', 'notified')
	`, accountID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("failed to count pending executions for account: %w", err)
	}
	return n, nil
}

// CountLiveExecutionsForAccount returns the number of purchase executions
// referencing this cloud account whose status is not in
// TerminalExecutionStatuses: rows an executor, scheduler or reaper may still
// act on, which need the account to exist. DeleteCloudAccount detaches every
// other row, so these are exactly the rows that block the delete (issue #265).
// It includes the pending/notified rows CountPendingExecutionsForAccount
// counts.
func (s *PostgresStore) CountLiveExecutionsForAccount(ctx context.Context, accountID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM purchase_executions
		WHERE cloud_account_id = $1
		  AND status <> ALL($2)
	`, accountID, TerminalExecutionStatuses).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("failed to count live executions for account: %w", err)
	}
	return n, nil
}

// ListPendingExecutionIDsForAccount returns the execution IDs that the
// frontend's Cancel-All-Then-Delete flow needs to POST cancel for. Capped
// at 1000 rows — a single account with more pending executions than that
// is an unusual operator-cleanup task rather than a button-click flow.
func (s *PostgresStore) ListPendingExecutionIDsForAccount(ctx context.Context, accountID string) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT execution_id FROM purchase_executions
		WHERE cloud_account_id = $1
		  AND status IN ('pending', 'notified')
		ORDER BY scheduled_date ASC
		LIMIT 1000
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list pending execution ids for account: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan pending execution id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate pending execution ids: %w", err)
	}
	return ids, nil
}

// queryExecutions is a helper to query and scan purchase executions.
func (s *PostgresStore) queryExecutions(ctx context.Context, query string, args ...any) ([]PurchaseExecution, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query executions: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// applyNullTimesToExecution sets the nullable timestamp fields on exec from the
// sql.NullTime wrappers, pulled out of scanExecutionRows to keep that function
// under the cyclomatic limit.
func applyNullTimesToExecution(exec *PurchaseExecution, notifSent, completedAt, expiresAt, tokenExpiresAt, executedAt, scheduledExecutionAt sql.NullTime) {
	if notifSent.Valid {
		exec.NotificationSent = &notifSent.Time
	}
	if completedAt.Valid {
		exec.CompletedAt = &completedAt.Time
	}
	if expiresAt.Valid {
		exec.TTL = ttlFromTime(expiresAt.Time)
	}
	if tokenExpiresAt.Valid {
		exec.ApprovalTokenExpiresAt = &tokenExpiresAt.Time
	}
	if executedAt.Valid {
		exec.ExecutedAt = &executedAt.Time
	}
	if scheduledExecutionAt.Valid {
		exec.ScheduledExecutionAt = &scheduledExecutionAt.Time
	}
}

// scanExecutionRows scans a pgx.Rows cursor into a slice of PurchaseExecution.
// It is used by queryExecutions (pool query) and GetPendingExecutionsTx (tx
// query) so the scan logic lives in one place.
func scanExecutionRows(rows pgx.Rows) ([]PurchaseExecution, error) {
	executions := make([]PurchaseExecution, 0)
	for rows.Next() {
		var exec PurchaseExecution
		var recommendationsJSON []byte
		var notifSent, completedAt, expiresAt, tokenExpiresAt, executedAt, scheduledExecutionAt sql.NullTime
		// plan_id is nullable since migration 000033 (direct-execute
		// rows from the Recommendations page have no originating plan).
		var planID sql.NullString
		// idempotency_key is NULL on rows created before migration 000066;
		// leave exec.IdempotencyKey "" for those so the derivation falls back
		// to ExecutionID (issue #1012).
		var idempotencyKey sql.NullString
		// approval_token_hash is NULL on rows migration 000100 found with no
		// raw token (every canceled row among them).
		var approvalTokenHash sql.NullString

		err := rows.Scan(
			&planID,
			&exec.ExecutionID,
			&exec.Status,
			&exec.StepNumber,
			&exec.ScheduledDate,
			&notifSent,
			&approvalTokenHash,
			&recommendationsJSON,
			&exec.TotalUpfrontCost,
			&exec.EstimatedSavings,
			&completedAt,
			&exec.Error,
			&expiresAt,
			&exec.CloudAccountID,
			&exec.Source,
			&exec.ApprovedBy,
			&exec.CancelledBy,
			&exec.CapacityPercent,
			&exec.CreatedByUserID,
			&exec.RetryExecutionID,
			&exec.RetryAttemptN,
			&tokenExpiresAt,
			&exec.ExecutedByUserID,
			&executedAt,
			&exec.PreApprovalSkipReason,
			&idempotencyKey,
			&scheduledExecutionAt,
			&exec.NoSubmission,
			&exec.CreatedBySystem,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan execution: %w", err)
		}

		if planID.Valid {
			exec.PlanID = planID.String
		}
		if idempotencyKey.Valid {
			exec.IdempotencyKey = idempotencyKey.String
		}
		exec.ApprovalToken = approvalTokenHash.String

		// Unmarshal recommendations
		if err := json.Unmarshal(recommendationsJSON, &exec.Recommendations); err != nil {
			return nil, fmt.Errorf("failed to unmarshal recommendations: %w", err)
		}

		applyNullTimesToExecution(&exec, notifSent, completedAt, expiresAt, tokenExpiresAt, executedAt, scheduledExecutionAt)

		executions = append(executions, exec)
	}

	return executions, rows.Err()
}

// GetScheduledExecutionsDue returns purchase_executions with status='scheduled'
// whose scheduled_execution_at has elapsed (scheduled_execution_at <= NOW()).
// Used by the Gmail-style pre-fire delay scheduler tick (issue #291 wave-2).
// Results are ordered oldest-due-first so the scheduler fires them in FIFO order.
// Capped at MaxListLimit per sweep to bound the per-tick blast radius.
func (s *PostgresStore) GetScheduledExecutionsDue(ctx context.Context) ([]PurchaseExecution, error) {
	query := `
		SELECT plan_id, execution_id, status, step_number, scheduled_date,
		       notification_sent, approval_token_hash, recommendations,
		       total_upfront_cost, estimated_savings, completed_at, error, expires_at,
		       cloud_account_id, source, approved_by, COALESCE(canceled_by, cancelled_by) AS cancelled_by, capacity_percent,
		       created_by_user_id, retry_execution_id, retry_attempt_n,
		       approval_token_expires_at,
		       executed_by_user_id, executed_at, pre_approval_skip_reason,
		       idempotency_key, scheduled_execution_at, no_submission, created_by_system
		FROM purchase_executions
		WHERE status = 'scheduled'
		  AND scheduled_execution_at IS NOT NULL
		  AND scheduled_execution_at <= NOW()
		ORDER BY scheduled_execution_at ASC
		LIMIT $1
	`
	rows, err := s.db.Query(ctx, query, MaxListLimit)
	if err != nil {
		return nil, fmt.Errorf("GetScheduledExecutionsDue: query failed: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// CleanupOldExecutions deletes purchase executions older than retentionDays.
//
// Three independent cleanup branches, each with its own retention window so
// that a row far in one dimension doesn't block cleanup in the other:
//
//  1. Completed-state cleanup: `status = 'completed' AND scheduled_date <
//     NOW() - retention`. Keeps recent completions visible in the UI for at
//     least `retention` days before purging. `scheduled_date` is the right
//     clock here because a completed row executed on (or very near) the day
//     it was scheduled for.
//
//  2. Canceled-state cleanup: `status IN ('cancelled', 'canceled') AND
//     updated_at < NOW() - retention`. Both spellings are included to cover
//     legacy rows ('cancelled') and new code rows ('canceled', canonical US
//     spelling per StatusCanceled / migration 000089).
//
//     This branch retains on `updated_at`, NOT `scheduled_date`, and must
//     stay that way: CountExecutionsByPlanAndStatus windows the plan health
//     score's canceled count on `updated_at` over exactly this retention
//     period (see internal/api/plan_health.go planHealthLookbackDays), and
//     CancelExecutionAtomic stamps `updated_at` while leaving
//     `scheduled_date` untouched. Retaining on `scheduled_date` therefore
//     purged rows the score was still counting: cancel a purchase whose
//     scheduled_date is already past the horizon (a plan's rows are created
//     up front, and parseCreatePurchasesRequest accepts a past start date)
//     and the next sweep deleted it the same day, silently raising that
//     plan's health score by up to 20 points overnight with no operator
//     action. Nothing accumulates indefinitely: a genuinely old canceled
//     row still has an old `updated_at`.
//
//     This cuts the other way too, and the change is user-visible: a
//     canceled row with a FUTURE `scheduled_date` used to be retained
//     forever, because `scheduled_date < NOW() - retention` was never true
//     for it. It now leaves History `retention` days after the
//     cancellation rather than `retention` days after the date it was
//     scheduled for, so a purchase canceled today for a 2-year-out date
//     disappears in a month instead of in two years. That closes an
//     unbounded-retention leak and is the behavior the health window
//     already assumed.
//
//  3. Expired-row cleanup: `expires_at IS NOT NULL AND expires_at < NOW() -
//     retention`, for any status the health score does not count. This is
//     the row's own TTL column (SavePurchaseExecution writes it from
//     PurchaseExecution.TTL), NOT the approval-token deadline -- that is a
//     separate column, `approval_token_expires_at`, added by migration
//
//  000051. A row whose TTL lapsed longer than `retention` ago is dead, so
//     without this branch it would accumulate indefinitely.
//
//     Every status in HealthScoredExecutionStatuses is excluded here, which
//     is the whole reason that slice is exported rather than spelled out
//     twice. `expires_at` is written once at insert and never rewritten
//     when a row later reaches a terminal status, so it can be arbitrarily
//     older than the terminal transition. Without the exclusion, a row that
//     failed or was canceled TODAY but carries a lapsed `expires_at` is
//     purged by this branch the same day, while the score is still counting
//     it -- reintroducing through this branch the exact overnight score
//     jump branch 2 exists to prevent, and at up to -40 for `failed`, twice
//     canceled's worst case. Their own terminal-status branches govern them.
//
//     `failed` deliberately has no cleanup branch of its own: the health
//     score's lookback comment (internal/api/plan_health.go
//     planHealthLookbackDays) documents that this sweep "never deletes
//     failed ones", and the score's window depends on that being true.
//
//     Note on reachability: `expires_at` currently has exactly one writer,
//     SavePurchaseExecution via timeFromTTL(execution.TTL), and no
//     production path assigns PurchaseExecution.TTL, so rows created by
//     current code leave it NULL and this branch is inert for them. It can
//     still hold non-NULL values on legacy imported rows, which is why the
//     exclusion is written defensively rather than omitted.
//
// The branches are OR'd: a row that qualifies under ANY is deleted,
// regardless of the other columns. An earlier revision of this function
// incorrectly AND'd the `scheduled_date` gate with both branches, which
// meant pending rows with a far-future `scheduled_date` but a long-past
// `expires_at` never got cleaned up (a 2-year-out purchase whose row TTL
// lapsed would leave a dead row accumulating for 1.9 years). Keep each
// branch fully parenthesized so a future edit can't change the predicate's
// meaning through AND/OR precedence.
//
// NULL `expires_at` is excluded from branch 3 so rows that never had a TTL
// are safe from expiry-based cleanup -- which, per the reachability note
// above, is every row current code writes.
func (s *PostgresStore) CleanupOldExecutions(ctx context.Context, retentionDays int) (int64, error) {
	// Branch 2 lists both spellings: 'cancelled' (legacy, pre-migration
	// 000089 rows) and 'canceled' (canonical, written by new code after the
	// follow-up to #1277). #1278 will drop 'cancelled' once all rows are
	// normalized.
	//
	// Branch 3's exclusion is parameterized from
	// HealthScoredExecutionStatuses instead of repeating a literal list, so
	// a status added to the plan health score extends this exclusion
	// automatically and the two cannot drift. `status` is NOT NULL
	// (migration 000001), so `<> ALL` is not exposed to three-valued logic.
	query := `
		DELETE FROM purchase_executions
		WHERE (
		        status = 'completed'
		    AND scheduled_date < NOW() - INTERVAL '1 day' * $1
		      )
		   OR (
		        status IN ('cancelled', 'canceled')
		    AND updated_at     < NOW() - INTERVAL '1 day' * $1
		      )
		   OR (
		        status <> ALL($2)
		    AND expires_at IS NOT NULL
		    AND expires_at     < NOW() - INTERVAL '1 day' * $1
		      )
	`

	result, err := s.db.Exec(ctx, query, retentionDays, HealthScoredExecutionStatuses)
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup old executions: %w", err)
	}

	return result.RowsAffected(), nil
}

// ==========================================
// PURCHASE HISTORY
// ==========================================

// SavePurchaseHistory saves a purchase history record. It is idempotent per
// (provider, account_id, purchase_id): a re-drive or root retry that adopts a
// commitment the first attempt already bought saves the same key again, and a
// second row would double-count spend and savings (#704). The NOT EXISTS
// covers rows that predate the unique index from migration 000108; the index
// and ON CONFLICT cover two concurrent saves. An existing row is left as is.
func (s *PostgresStore) SavePurchaseHistory(ctx context.Context, record *PurchaseHistoryRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	query := `
		INSERT INTO purchase_history (
			account_id, purchase_id, timestamp, provider, service, region,
			resource_type, count, term, payment, upfront_cost, monthly_cost,
			estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
			source, revocation_window_closes_at, offering_class
		)
		SELECT $1::varchar, $2::varchar, $3, $4::varchar, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20
		WHERE NOT EXISTS (
			SELECT 1 FROM purchase_history
			WHERE provider = $4 AND account_id = $1 AND purchase_id = $2
		)
		ON CONFLICT DO NOTHING
	`

	tag, err := s.db.Exec(ctx, query,
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
	)

	if err != nil {
		return fmt.Errorf("failed to save purchase history: %w", err)
	}
	if tag.RowsAffected() == 0 {
		logging.Infof("purchase history for %s/%s/%s already recorded; keeping the existing row", record.Provider, record.AccountID, record.PurchaseID)
	}

	return nil
}

// UpdatePurchaseHistoryListing stamps the marketplace listing fields onto a
// purchase_history row identified by its purchase_id (RI reservation ID).
// Called by the marketplace-list handler after CreateReservedInstancesListing
// succeeds (listing_id + listing_state="active") and by the status-poll path on
// the later closed/canceled transitions.
func (s *PostgresStore) UpdatePurchaseHistoryListing(ctx context.Context, purchaseID, listingID, listingState string) error {
	query := `
		UPDATE purchase_history
		SET listing_id = $1, listing_state = $2,
		    listing_client_token = NULL, listing_price_schedule = NULL
		WHERE purchase_id = $3
	`
	tag, err := s.db.Exec(ctx, query, listingID, listingState, purchaseID)
	if err != nil {
		return fmt.Errorf("failed to update listing state for purchase %s: %w", purchaseID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("purchase %s not found in history", purchaseID)
	}
	return nil
}

// StampOfferingClass writes the offering_class value onto a purchase_history
// row identified by purchase_id. Used by the marketplace-list handler to
// lazily persist offering_class fetched from AWS DescribeReservedInstances for
// rows created before migration 000087 or for externally-created Standard RIs.
// A no-match (row not found) is treated as a non-fatal warning by callers.
func (s *PostgresStore) StampOfferingClass(ctx context.Context, purchaseID, offeringClass string) error {
	query := `UPDATE purchase_history SET offering_class = $1 WHERE purchase_id = $2`
	_, err := s.db.Exec(ctx, query, offeringClass, purchaseID)
	if err != nil {
		return fmt.Errorf("failed to stamp offering_class for purchase %s: %w", purchaseID, err)
	}
	return nil
}

// ClaimMarketplaceListingSlot atomically reserves the marketplace-listing slot
// for a purchase_history row so two concurrent marketplace-list requests cannot
// both proceed to create a duplicate AWS listing (issue #292). The single
// conditional UPDATE transitions listing_state to ListingStatePending unless the
// row is active, or pending without a persisted token. It stores clientToken and
// priceSchedule in the same statement unless an earlier attempt already stored a
// token, and returns the row's values as of the claim (issue #525): the handler
// must not trust a row read before the claim. It returns (nil, nil) when the row
// could not be claimed (active, pending without a token, or absent).
func (s *PostgresStore) ClaimMarketplaceListingSlot(ctx context.Context, purchaseID, clientToken string, priceSchedule []byte) (*MarketplaceListingClaim, error) {
	// prev locks the row first, so a claim that races another one waits for it
	// and then sees its committed token and state instead of a stale snapshot.
	query := `
		WITH prev AS (
			SELECT purchase_id, listing_state AS prior_state
			FROM purchase_history WHERE purchase_id = $2 FOR UPDATE
		)
		UPDATE purchase_history AS p
		SET listing_state = $1,
		    listing_client_token = COALESCE(p.listing_client_token, $3::text),
		    listing_price_schedule = CASE WHEN p.listing_client_token IS NULL
		                                  THEN $4::jsonb ELSE p.listing_price_schedule END
		FROM prev
		WHERE p.purchase_id = prev.purchase_id
		  AND lower(COALESCE(p.listing_state, '')) <> $5
		  AND (lower(COALESCE(p.listing_state, '')) <> $1 OR p.listing_client_token IS NOT NULL)
		RETURNING COALESCE(p.listing_id, ''), COALESCE(prev.prior_state, ''),
		          p.listing_client_token, p.listing_price_schedule::text
	`
	var claim MarketplaceListingClaim
	var schedule string
	err := s.db.QueryRow(ctx, query, ListingStatePending, purchaseID, clientToken, priceSchedule, ListingStateActive).
		Scan(&claim.ListingID, &claim.PriorState, &claim.ClientToken, &schedule)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to claim marketplace listing slot for purchase %s: %w", purchaseID, err)
	}
	claim.PriceSchedule = []byte(schedule)
	claim.Resumed = claim.ClientToken != clientToken
	return &claim, nil
}

// ReleaseMarketplaceListingClaim moves a pending row back to priorState and,
// unless keepAttempt, clears the persisted token and schedule. It only touches
// a pending row whose token is clientToken, so it never overwrites a recorded
// listing or the attempt of another request.
func (s *PostgresStore) ReleaseMarketplaceListingClaim(ctx context.Context, purchaseID, clientToken, priorState string, keepAttempt bool) error {
	query := `
		UPDATE purchase_history
		SET listing_state = NULLIF($2, ''),
		    listing_client_token = CASE WHEN $3 THEN listing_client_token END,
		    listing_price_schedule = CASE WHEN $3 THEN listing_price_schedule END
		WHERE purchase_id = $1 AND lower(COALESCE(listing_state, '')) = $4
		  AND listing_client_token = $5
	`
	if _, err := s.db.Exec(ctx, query, purchaseID, priorState, keepAttempt, ListingStatePending, clientToken); err != nil {
		return fmt.Errorf("failed to release marketplace listing claim for purchase %s: %w", purchaseID, err)
	}
	return nil
}

// GetPurchaseHistory retrieves purchase history for an account.
func (s *PostgresStore) GetPurchaseHistory(ctx context.Context, accountID string, limit int) ([]PurchaseHistoryRecord, error) {
	query := `
		SELECT account_id, purchase_id, timestamp, provider, service, region,
		       resource_type, count, term, payment, upfront_cost, monthly_cost,
		       estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
		       revocation_window_closes_at, revoked_at, revoked_via, support_case_id,
		       offering_class, listing_id, listing_state
		FROM purchase_history
		WHERE account_id = $1
		ORDER BY timestamp DESC
		LIMIT $2
	`

	return s.queryPurchaseHistory(ctx, query, accountID, limit)
}

// GetAllPurchaseHistory retrieves all purchase history.
func (s *PostgresStore) GetAllPurchaseHistory(ctx context.Context, limit int) ([]PurchaseHistoryRecord, error) {
	query := `
		SELECT account_id, purchase_id, timestamp, provider, service, region,
		       resource_type, count, term, payment, upfront_cost, monthly_cost,
		       estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
		       revocation_window_closes_at, revoked_at, revoked_via, support_case_id,
		       offering_class, listing_id, listing_state
		FROM purchase_history
		ORDER BY timestamp DESC
		LIMIT $1
	`

	return s.queryPurchaseHistory(ctx, query, limit)
}

// activeExpiryEpochExpr is a purchase_history row's commitment expiry
// (timestamp + term * 8760 hours) as Unix epoch seconds. It must stay the
// same expression as the one indexed by
// idx_purchase_history_active_expiry (migration 000126), or the planner cannot
// use that index and GetActivePurchaseHistory falls back to evaluating every
// row (issue #33).
//
// The obvious form, timestamp + make_interval(hours => term * 8760), cannot be
// indexed: timestamptz + interval is only STABLE because day and month
// intervals depend on the session time zone. Converting to UTC wall time and
// taking the epoch is IMMUTABLE, and for an hours-only interval it is exact
// arithmetic on the same instant, so the rows matched are unchanged. numeric
// arithmetic cannot overflow, so no term value can make an INSERT fail on the
// index expression. 31536000 = 8760 * 3600, the same 365-day year as the
// analytics collector's HoursPerYear.
const activeExpiryEpochExpr = "(extract(epoch FROM (timestamp AT TIME ZONE 'UTC')) + term::numeric * 31536000)"

// GetActivePurchaseHistory retrieves every purchase_history row still within its
// commitment term at asOf, optionally scoped to a set of accounts via the same
// dual-column predicate as GetPurchaseHistoryFiltered (appendAccountPredicate):
// both accountIDs (cloud_accounts UUIDs matched on cloud_account_id) and
// externalIDsByProvider (provider-scoped external numbers matched on
// account_id) are applied with OR semantics so rows carrying only one
// identifier are still returned (issues #701/#498/#866). Both empty means all
// accounts. The active filter is pushed into SQL so the result is bounded by
// the number of live commitments (not by all history ever recorded), which is
// what the analytics collector, dashboard KPIs, and inventory endpoints need:
// it cannot silently truncate older-but-still-active 1y/3y commitments the way
// a newest-first LIMIT page does (issue #1140). term*8760 hours matches the
// collector's HoursPerYear and the API layer's commitmentExpiry (both 365*24)
// so the SQL and Go term windows agree. The expiry comparison is inclusive
// (expiry >= asOf): a commitment expiring exactly at asOf is still active,
// matching the API layer's isActiveCommitment (!now.After(expiry)) so the SQL
// result set and the Go-side active checks share one boundary definition.
//
// The expiry is compared as Unix epoch seconds (activeExpiryEpochExpr) so the
// predicate can use idx_purchase_history_active_expiry (migration 000126)
// instead of evaluating every row ever recorded (issue #33).
func (s *PostgresStore) GetActivePurchaseHistory(ctx context.Context, asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) ([]PurchaseHistoryRecord, error) {
	query, args := activePurchaseHistoryQuery(asOf, accountIDs, externalIDsByProvider)
	return s.queryPurchaseHistory(ctx, query, args...)
}

// activePurchaseHistoryQuery builds GetActivePurchaseHistory's SQL and bind
// args. It is separate so the integration tests can EXPLAIN the exact query.
func activePurchaseHistoryQuery(asOf time.Time, accountIDs []string, externalIDsByProvider map[string][]string) (query string, args []any) {
	conds := []string{
		"term > 0",
		activeExpiryEpochExpr + " >= extract(epoch FROM ($1::timestamptz AT TIME ZONE 'UTC'))",
		"revoked_at IS NULL",
	}
	args = []any{asOf}
	conds, args = appendAccountPredicate(conds, args, accountIDs, externalIDsByProvider)

	// The column list MUST match scanPurchaseHistoryRow's scan destinations
	// exactly (pgx errors on a field/destination count mismatch). #808 added
	// offering_class, listing_id, listing_state to the scanner and every other
	// purchase-history SELECT but missed this one, which broke every real-Postgres
	// call here (dashboard KPIs, /api/inventory/*, analytics collector).
	query = fmt.Sprintf(`
		SELECT account_id, purchase_id, timestamp, provider, service, region,
		       resource_type, count, term, payment, upfront_cost, monthly_cost,
		       estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
		       revocation_window_closes_at, revoked_at, revoked_via, support_case_id,
		       offering_class, listing_id, listing_state
		FROM purchase_history
		WHERE %s
		ORDER BY timestamp DESC
	`, strings.Join(conds, " AND "))
	return query, args
}

// appendAccountPredicate pulls the dual-column account-predicate arg-building
// branch out of GetPurchaseHistoryFiltered to keep it under the cyclomatic limit.
//
// Either input alone is sufficient: a UUID-only filter still matches
// NULL-cloud_account_id rows via the resolved external IDs, and an
// external-only filter matches UUID-only rows once the caller resolves them.
// Both empty returns conds/args unchanged (no account clause -> all accounts).
//
// The external-id half is grouped per provider so each external number is only
// compared against rows of its own provider:
//
//	(cloud_account_id = ANY($u)
//	   OR ((provider = '' OR LOWER(provider) = LOWER($p1)) AND account_id = ANY($e1))
//	   OR ((provider = '' OR LOWER(provider) = LOWER($p2)) AND account_id = ANY($e2)))
//
// This preserves the (provider, external_id) pairing so a filter for aws/123
// never matches azure/123 rows. The "" provider key (legacy raw external
// number, unknown provider) matches account_id with no provider gate. Providers
// are sorted for deterministic SQL. The OR is wrapped in parentheses so it
// composes with the surrounding AND chain.
//
// The provider gate is case-insensitive, and a row whose stored provider is
// empty (unknown) matches on account_id alone, the same way the "" filter key
// does. Without this, a NULL-cloud_account_id row stored as "AWS" or "" was
// silently dropped even though its account_id matched (issue #24). A row
// stored under a different, non-empty provider is still excluded.
func appendAccountPredicate(conds []string, args []any, accountIDs []string, externalIDsByProvider map[string][]string) (outConds []string, outArgs []any) {
	if len(accountIDs) == 0 && len(externalIDsByProvider) == 0 {
		return conds, args
	}
	var ors []string
	if len(accountIDs) > 0 {
		args = append(args, accountIDs)
		ors = append(ors, fmt.Sprintf("cloud_account_id = ANY($%d)", len(args)))
	}
	for _, provider := range sortedProviderKeys(externalIDsByProvider) {
		exts := externalIDsByProvider[provider]
		if len(exts) == 0 {
			continue
		}
		if provider == "" {
			args = append(args, exts)
			ors = append(ors, fmt.Sprintf("account_id = ANY($%d)", len(args)))
			continue
		}
		args = append(args, provider)
		providerArg := len(args)
		args = append(args, exts)
		ors = append(ors, fmt.Sprintf(
			"((provider = '' OR LOWER(provider) = LOWER($%d)) AND account_id = ANY($%d))",
			providerArg, len(args)))
	}
	if len(ors) == 0 {
		return conds, args
	}
	conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	return conds, args
}

// sortedProviderKeys returns the map keys in ascending order so the generated
// SQL (and its bind-arg ordering) is deterministic across calls and testable.
func sortedProviderKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// GetPurchaseHistoryFiltered reads purchase_history rows matching the
// supplied filter set, newest-first, capped at filter.Limit. See the
// StoreInterface docstring and PurchaseHistoryFilter for the per-field
// semantics. Each WHERE clause is appended only when its field is populated,
// so an empty filter gets the same plan-shape as GetAllPurchaseHistory.
// Implementation mirrors buildRecommendationFilter
// (store_postgres_recommendations.go).
//
// The account predicate matches BOTH identifier columns:
//
//	(cloud_account_id = ANY($uuids)
//	   OR ((provider = '' OR LOWER(provider) = LOWER($p)) AND account_id = ANY($extsForP)) OR ...)
//
// purchase_history carries two account identifiers and either may be the only
// one populated on a given row: cloud_account_id (the cloud_accounts UUID FK,
// added in migration 000011 with no backfill, so NULL on every direct-execute /
// ambient / pre-000011 row) and account_id (the cloud-provider external number,
// e.g. an AWS account number, always populated). The top-bar Account chip emits
// the UUID, so a UUID-only predicate silently dropped every NULL-cloud_account_id
// row (issue #701/#498) while an external-only predicate dropped every row that
// only has the UUID (issue #866). Matching both columns includes rows written by
// either path. The caller resolves the requested UUIDs to their (provider,
// external_id) pairs scoped to the user's accessible accounts and groups the
// external ids by provider, so the external-id half stays provider-scoped and a
// reused external number (aws/123 vs azure/123) cannot leak the wrong rows.
func (s *PostgresStore) GetPurchaseHistoryFiltered(
	ctx context.Context,
	filter PurchaseHistoryFilter,
) ([]PurchaseHistoryRecord, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	var (
		conds []string
		args  []any
	)
	add := func(cond string, val any) {
		conds = append(conds, fmt.Sprintf(cond, len(args)+1))
		args = append(args, val)
	}
	if filter.Provider != "" {
		add("provider = $%d", filter.Provider)
	}
	conds, args = appendAccountPredicate(conds, args, filter.AccountIDs, filter.ExternalIDsByProvider)
	if filter.Start != nil {
		add("timestamp >= $%d", *filter.Start)
	}
	if filter.End != nil {
		add("timestamp <= $%d", *filter.End)
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)

	query := fmt.Sprintf(`
		SELECT account_id, purchase_id, timestamp, provider, service, region,
		       resource_type, count, term, payment, upfront_cost, monthly_cost,
		       estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
		       revocation_window_closes_at, revoked_at, revoked_via, support_case_id,
		       offering_class, listing_id, listing_state
		FROM purchase_history%s
		ORDER BY timestamp DESC
		LIMIT $%d
	`, where, len(args))

	return s.queryPurchaseHistory(ctx, query, args...)
}

// queryPurchaseHistory is a helper to query and scan purchase history.
// The query must SELECT the following columns in order:
//
//	account_id, purchase_id, timestamp, provider, service, region,
//	resource_type, count, term, payment, upfront_cost, monthly_cost,
//	estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
//	revocation_window_closes_at, revoked_at, revoked_via, support_case_id,
//	offering_class, listing_id, listing_state
//
// The revocation columns were added in migration 000057; the marketplace
// listing columns (offering_class, listing_id, listing_state) in the #292
// migration. Queries must include them explicitly so the Scan targets stay
// in sync.
func (s *PostgresStore) queryPurchaseHistory(ctx context.Context, query string, args ...any) ([]PurchaseHistoryRecord, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query purchase history: %w", err)
	}
	defer rows.Close()

	records := make([]PurchaseHistoryRecord, 0)
	for rows.Next() {
		record, err := scanPurchaseHistoryRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	return records, rows.Err()
}

// scanPurchaseHistoryRow scans a single purchase_history row and reconciles the
// nullable columns into a PurchaseHistoryRecord. It is pulled out of
// queryPurchaseHistory to keep that function under the cyclomatic limit. The
// column order must match the SELECT lists in GetPurchaseHistory /
// GetAllPurchaseHistory / GetPurchaseHistoryFiltered (base columns, then the
// revocation columns from issue #290, then the marketplace columns from #292).
// purchaseHistoryNullables holds the nullable scan targets shared by every
// purchase_history reader (scanPurchaseHistoryRow, GetPurchaseHistoryByPurchaseID).
// Centralizing the NULL reconciliation in applyTo keeps each reader's
// cyclomatic complexity under the gocyclo budget as columns accrue across
// issues #290 (revocation) and #292 (marketplace).
type purchaseHistoryNullables struct {
	upfrontCost              sql.NullFloat64
	monthlyCost              sql.NullFloat64
	planID                   sql.NullString
	planName                 sql.NullString
	cloudAccountID           sql.NullString
	revocationWindowClosesAt *time.Time
	revokedAt                *time.Time
	revokedVia               sql.NullString
	supportCaseID            sql.NullString
	offeringClass            sql.NullString
	listingID                sql.NullString
	listingState             sql.NullString
}

// applyTo reconciles the nullable columns into record. monthly_cost is left as
// a nil pointer when the provider returned no monthly breakdown (distinct from
// an explicit $0 recurring charge); the revocation-window timestamps map
// directly onto their pointer fields.
func (n *purchaseHistoryNullables) applyTo(record *PurchaseHistoryRecord) {
	record.UpfrontCost = nullPtrFromNullFloat64(n.upfrontCost)
	if n.monthlyCost.Valid {
		v := n.monthlyCost.Float64
		record.MonthlyCost = &v
	}
	if n.planID.Valid {
		record.PlanID = n.planID.String
	}
	if n.planName.Valid {
		record.PlanName = n.planName.String
	}
	if n.cloudAccountID.Valid {
		record.CloudAccountID = &n.cloudAccountID.String
	}
	record.RevocationWindowClosesAt = n.revocationWindowClosesAt
	record.RevokedAt = n.revokedAt
	if n.revokedVia.Valid {
		record.RevokedVia = n.revokedVia.String
	}
	if n.supportCaseID.Valid {
		record.SupportCaseID = n.supportCaseID.String
	}
	if n.offeringClass.Valid {
		record.OfferingClass = n.offeringClass.String
	}
	if n.listingID.Valid {
		record.ListingID = n.listingID.String
	}
	if n.listingState.Valid {
		record.ListingState = n.listingState.String
	}
}

func scanPurchaseHistoryRow(rows pgx.Rows) (PurchaseHistoryRecord, error) {
	var record PurchaseHistoryRecord
	var n purchaseHistoryNullables

	if err := rows.Scan(
		&record.AccountID,
		&record.PurchaseID,
		&record.Timestamp,
		&record.Provider,
		&record.Service,
		&record.Region,
		&record.ResourceType,
		&record.Count,
		&record.Term,
		&record.Payment,
		&n.upfrontCost,
		&n.monthlyCost,
		&record.EstimatedSavings,
		&n.planID,
		&n.planName,
		&record.RampStep,
		&n.cloudAccountID,
		&n.revocationWindowClosesAt,
		&n.revokedAt,
		&n.revokedVia,
		&n.supportCaseID,
		&n.offeringClass,
		&n.listingID,
		&n.listingState,
	); err != nil {
		return PurchaseHistoryRecord{}, fmt.Errorf("failed to scan purchase history: %w", err)
	}

	n.applyTo(&record)
	return record, nil
}

// GetPurchaseHistoryByPurchaseID returns the single purchase_history row
// whose purchase_id matches purchaseID. Returns (nil, nil) when the row
// does not exist. The revocation-window columns (revocation_window_closes_at,
// revoked_at, revoked_via, support_case_id) are read alongside the base
// columns so the revoke endpoint can check idempotency without a second round
// trip (issue #290). The marketplace columns (offering_class, listing_id,
// listing_state) are read so the marketplace-list handler can validate
// offering_class and look up the cloud account (issue #292).
func (s *PostgresStore) GetPurchaseHistoryByPurchaseID(ctx context.Context, purchaseID string) (*PurchaseHistoryRecord, error) {
	query := `
		SELECT account_id, purchase_id, timestamp, provider, service, region,
		       resource_type, count, term, payment, upfront_cost, monthly_cost,
		       estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
		       revocation_window_closes_at, revoked_at, revoked_via, support_case_id,
		       revocation_in_flight, offering_class, listing_id, listing_state
		FROM purchase_history
		WHERE purchase_id = $1
		LIMIT 1
	`
	rows, err := s.db.Query(ctx, query, purchaseID)
	if err != nil {
		return nil, fmt.Errorf("GetPurchaseHistoryByPurchaseID: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, rows.Err()
	}

	var r PurchaseHistoryRecord
	var n purchaseHistoryNullables

	// Note the extra revocation_in_flight bool between support_case_id and the
	// marketplace columns; it scans straight into r.RevocationInFlight rather
	// than through the shared nullables struct.
	if err := rows.Scan(
		&r.AccountID,
		&r.PurchaseID,
		&r.Timestamp,
		&r.Provider,
		&r.Service,
		&r.Region,
		&r.ResourceType,
		&r.Count,
		&r.Term,
		&r.Payment,
		&n.upfrontCost,
		&r.MonthlyCost,
		&r.EstimatedSavings,
		&n.planID,
		&n.planName,
		&r.RampStep,
		&n.cloudAccountID,
		&n.revocationWindowClosesAt,
		&n.revokedAt,
		&n.revokedVia,
		&n.supportCaseID,
		&r.RevocationInFlight,
		&n.offeringClass,
		&n.listingID,
		&n.listingState,
	); err != nil {
		return nil, fmt.Errorf("GetPurchaseHistoryByPurchaseID scan: %w", err)
	}

	// MonthlyCost is scanned directly above (not through n.monthlyCost), so
	// leave n.monthlyCost zero-valued; applyTo will not overwrite r.MonthlyCost.
	n.applyTo(&r)

	return &r, rows.Err()
}

// MarkPurchaseRevoked stamps revoked_at / revoked_via / support_case_id and
// the refund-quote audit columns (calc_refund_amount, calc_refund_currency) on
// the purchase_history row identified by purchaseID. The UPDATE is a no-op
// when revoked_at is already non-null (idempotency guard). Returns a not-found
// error when zero rows are affected and revoked_at was previously NULL.
func (s *PostgresStore) MarkPurchaseRevoked(ctx context.Context, purchaseID string, revokedAt time.Time, revokedVia, supportCaseID string, calcRefundAmount *float64, calcRefundCurrency string) error {
	var supportCaseIDPtr *string
	if supportCaseID != "" {
		supportCaseIDPtr = &supportCaseID
	}
	var calcCurrencyPtr *string
	if calcRefundCurrency != "" {
		calcCurrencyPtr = &calcRefundCurrency
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE purchase_history
		   SET revoked_at            = $2,
		       revoked_via           = $3,
		       support_case_id       = $4,
		       calc_refund_amount    = $5,
		       calc_refund_currency  = $6
		 WHERE purchase_id = $1
		   AND revoked_at IS NULL
	`, purchaseID, revokedAt, revokedVia, supportCaseIDPtr, calcRefundAmount, calcCurrencyPtr)
	if err != nil {
		return fmt.Errorf("MarkPurchaseRevoked: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either no row found or already revoked — check which.
		var exists bool
		err2 := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM purchase_history WHERE purchase_id=$1)`, purchaseID).Scan(&exists)
		if err2 != nil {
			return fmt.Errorf("MarkPurchaseRevoked existence check: %w", err2)
		}
		if !exists {
			return fmt.Errorf("MarkPurchaseRevoked: purchase_id %q not found", purchaseID)
		}
		// Already revoked — idempotent, treat as success.
	}
	return nil
}

// FlipPurchaseRevocationInFlight sets revocation_in_flight=true on the
// purchase_history row for purchaseID. Called immediately before the Azure
// Return API call to enable partial-success reconciliation (issue #290
// Finding #6). Idempotent: already-true rows are not modified. Returns a
// not-found error when no row matches.
func (s *PostgresStore) FlipPurchaseRevocationInFlight(ctx context.Context, purchaseID string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE purchase_history
		   SET revocation_in_flight = true
		 WHERE purchase_id = $1
	`, purchaseID)
	if err != nil {
		return fmt.Errorf("FlipPurchaseRevocationInFlight: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("FlipPurchaseRevocationInFlight: purchase_id %q not found", purchaseID)
	}
	return nil
}

// ClearRevocationInFlight sets revocation_in_flight=false on a purchase_history
// row. Called when the Azure Return call fails transiently (before Azure actually
// issued a refund) so the row is not left stuck in the in-flight state, which
// would mislead the finalize_revocations sweep into thinking Azure succeeded
// (issue #290, second-wave CR Finding D). No-op when already false.
func (s *PostgresStore) ClearRevocationInFlight(ctx context.Context, purchaseID string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE purchase_history
		   SET revocation_in_flight = false
		 WHERE purchase_id = $1
	`, purchaseID)
	if err != nil {
		return fmt.Errorf("ClearRevocationInFlight: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("ClearRevocationInFlight: purchase_id %q not found", purchaseID)
	}
	return nil
}

// GetPurchaseHistoryInFlight returns all purchase_history rows with
// revocation_in_flight=true and revoked_at IS NULL. Used by the
// finalize_revocations scheduled sweep to retry MarkPurchaseRevoked for rows
// where the Azure Return succeeded but the subsequent DB write failed
// (issue #290 Finding #6).
func (s *PostgresStore) GetPurchaseHistoryInFlight(ctx context.Context) ([]*PurchaseHistoryRecord, error) {
	rows, err := s.db.Query(ctx, `
		SELECT account_id, purchase_id, timestamp, provider, service, region,
		       resource_type, count, term, payment, upfront_cost, monthly_cost,
		       estimated_savings, plan_id, plan_name, ramp_step, cloud_account_id,
		       revocation_window_closes_at, revoked_at, revoked_via, support_case_id
		FROM purchase_history
		WHERE revocation_in_flight = true
		  AND revoked_at IS NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("GetPurchaseHistoryInFlight: %w", err)
	}
	defer rows.Close()

	var result []*PurchaseHistoryRecord
	for rows.Next() {
		var r PurchaseHistoryRecord
		var planID, planName, cloudAccountID sql.NullString
		var revocationWindowClosesAt, revokedAt *time.Time
		var revokedVia, supportCaseID sql.NullString
		var upfrontCost sql.NullFloat64

		if err := rows.Scan(
			&r.AccountID,
			&r.PurchaseID,
			&r.Timestamp,
			&r.Provider,
			&r.Service,
			&r.Region,
			&r.ResourceType,
			&r.Count,
			&r.Term,
			&r.Payment,
			&upfrontCost,
			&r.MonthlyCost,
			&r.EstimatedSavings,
			&planID,
			&planName,
			&r.RampStep,
			&cloudAccountID,
			&revocationWindowClosesAt,
			&revokedAt,
			&revokedVia,
			&supportCaseID,
		); err != nil {
			return nil, fmt.Errorf("GetPurchaseHistoryInFlight scan: %w", err)
		}
		r.UpfrontCost = nullPtrFromNullFloat64(upfrontCost)

		if planID.Valid {
			r.PlanID = planID.String
		}
		if planName.Valid {
			r.PlanName = planName.String
		}
		if cloudAccountID.Valid {
			r.CloudAccountID = &cloudAccountID.String
		}
		r.RevocationWindowClosesAt = revocationWindowClosesAt
		r.RevokedAt = revokedAt
		if revokedVia.Valid {
			r.RevokedVia = revokedVia.String
		}
		if supportCaseID.Valid {
			r.SupportCaseID = supportCaseID.String
		}
		r.RevocationInFlight = true
		result = append(result, &r)
	}
	return result, rows.Err()
}

// ==========================================
// RI EXCHANGE HISTORY
// ==========================================

// ClaimRIExchangeIdempotencyKey atomically claims an RI exchange submit
// fingerprint (issue #1642). See the interface doc for the contract.
//
// The whole decision is one statement, so two concurrent submits of the same
// fingerprint cannot both win: the INSERT and the conditional takeover of an
// expired row are the same atomic row operation, with no read-then-write
// window between them. RowsAffected() is 1 only when this call inserted the
// row or took over an expired one; the ON CONFLICT ... WHERE predicate
// evaluating false leaves it at 0.
//
// Both the write and the expiry comparison use now(), the DATABASE clock, so
// clock skew between concurrent application instances cannot distort the
// window.
func (s *PostgresStore) ClaimRIExchangeIdempotencyKey(ctx context.Context, key string, window time.Duration) (bool, error) {
	if key == "" {
		return false, fmt.Errorf("refusing to claim an empty RI exchange idempotency key")
	}
	if window <= 0 {
		return false, fmt.Errorf("RI exchange idempotency window must be positive, got %s", window)
	}
	query := `
		INSERT INTO ri_exchange_idempotency (idempotency_key, claimed_at)
		VALUES ($1, now())
		ON CONFLICT (idempotency_key) DO UPDATE
		   SET claimed_at = now()
		 WHERE ri_exchange_idempotency.claimed_at < now() - make_interval(secs => $2)
	`
	tag, err := s.db.Exec(ctx, query, key, window.Seconds())
	if err != nil {
		return false, fmt.Errorf("failed to claim RI exchange idempotency key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SaveRIExchangeRecord saves an RI exchange record. This is the single write
// path for ri_exchange_history (an INSERT, never an update-in-place of an
// existing row's token), so it is the correct place to hash the token
// into approval_token_hash (issue #103): record.ApprovalToken always carries a fresh
// raw value freshly generated by the caller (pkg/exchange in
// cloud-commitments-go), never a value previously read back from this
// column, so hashing it here can never double-hash an already-hashed value.
func (s *PostgresStore) SaveRIExchangeRecord(ctx context.Context, record *RIExchangeRecord) error {
	return saveRIExchangeRecord(ctx, s.db, record)
}

func saveRIExchangeRecord(ctx context.Context, q globalConfigExecutor, record *RIExchangeRecord) error {
	if record.ID == "" {
		record.ID = uuid.New().String()
	}

	now := time.Now()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now

	// PaymentDue is the Go-side mirror of a DECIMAL(20,6) NOT NULL DEFAULT 0
	// column with `CHECK (payment_due >= 0)`. We keep the Go field as a
	// string (rather than float64 — money should not round) but pgx can't
	// cast `""` to DECIMAL. Default the empty string to "0" at the boundary
	// so a freshly-zero-valued struct inserts cleanly. Anything non-empty
	// is passed through verbatim and the DECIMAL parser rejects malformed
	// values with a clear error.
	paymentDue := record.PaymentDue
	if paymentDue == "" {
		paymentDue = "0"
	}

	query := `
		INSERT INTO ri_exchange_history (
			id, account_id, exchange_id, region, source_ri_ids,
			source_instance_type, source_count, target_offering_id,
			target_instance_type, target_count, payment_due,
			status, approval_token_hash, error, mode, completed_at, expires_at,
			created_at, updated_at, created_by_user_id, ladder_run_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
	`

	_, err := q.Exec(ctx, query,
		record.ID,
		record.AccountID,
		record.ExchangeID,
		record.Region,
		record.SourceRIIDs,
		record.SourceInstanceType,
		record.SourceCount,
		record.TargetOfferingID,
		record.TargetInstanceType,
		record.TargetCount,
		paymentDue,
		record.Status,
		nullStringFromString(HashApprovalToken(record.ApprovalToken)),
		nullStringFromString(record.Error),
		record.Mode,
		record.CompletedAt,
		record.ExpiresAt,
		record.CreatedAt,
		record.UpdatedAt,
		record.CreatedByUserID,
		record.LadderRunID,
	)

	if err != nil {
		return fmt.Errorf("failed to save ri exchange record: %w", err)
	}

	return nil
}

// ReserveRIExchange atomically records the full execution ceiling before a provider call.
func (s *PostgresStore) ReserveRIExchange(ctx context.Context, record *RIExchangeRecord, dailyCapUSD, perExchangeCapUSD string) (string, error) {
	initial, dailyCap, perCap, err := riExchangeReservationMicros(record, dailyCapUSD, perExchangeCapUSD)
	if err != nil {
		return "", err
	}

	candidate := *record
	var ceiling string
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		var reserveErr error
		ceiling, reserveErr = reserveRIExchangeCeiling(ctx, tx, nil, record.PaymentDue, initial, dailyCap, perCap, dailyCapUSD)
		if reserveErr != nil {
			return reserveErr
		}
		candidate.PaymentDue = ceiling
		return saveRIExchangeRecord(ctx, tx, &candidate)
	})
	if err != nil {
		return "", err
	}
	*record = candidate
	return ceiling, nil
}

// ReserveApprovedRIExchange reserves the full execution ceiling for an approved
// exchange whose row is already processing. It runs under the same global
// advisory lock as ReserveRIExchange, so manual approvals and AUTO exchanges
// share one headroom protocol, and it leaves the row's own amount out of the
// spend it checks against. The row's payment_due becomes the reserved ceiling
// until CompleteRIExchangeWithPayment settles the accepted amount.
func (s *PostgresStore) ReserveApprovedRIExchange(ctx context.Context, id, dailyCapUSD, perExchangeCapUSD string) (string, error) {
	dailyCap, err := riExchangeMicros(dailyCapUSD)
	if err != nil {
		return "", fmt.Errorf("invalid daily RI exchange cap: %w", err)
	}
	perCap, err := riExchangeMicros(perExchangeCapUSD)
	if err != nil {
		return "", fmt.Errorf("invalid per-exchange RI cap: %w", err)
	}

	var ceiling string
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		var initialText string
		scanErr := tx.QueryRow(ctx,
			`SELECT payment_due::text FROM ri_exchange_history WHERE id = $1 AND status = 'processing' FOR UPDATE`, id,
		).Scan(&initialText)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return fmt.Errorf("ri exchange %s is not processing", id)
		}
		if scanErr != nil {
			return fmt.Errorf("failed to read RI exchange %s: %w", id, scanErr)
		}
		initial, parseErr := riExchangeMicros(initialText)
		if parseErr != nil {
			return fmt.Errorf("invalid initial RI exchange payment: %w", parseErr)
		}
		var reserveErr error
		ceiling, reserveErr = reserveRIExchangeCeiling(ctx, tx, &id, initialText, initial, dailyCap, perCap, dailyCapUSD)
		if reserveErr != nil {
			return reserveErr
		}
		_, updErr := tx.Exec(ctx,
			`UPDATE ri_exchange_history SET payment_due = $2::numeric, updated_at = NOW() WHERE id = $1`, id, ceiling)
		if updErr != nil {
			return fmt.Errorf("failed to record RI exchange reservation: %w", updErr)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return ceiling, nil
}

// ErrRIExchangeDailyCapExceeded marks a reservation refused because the
// initial quote does not fit the remaining daily or per-exchange headroom.
var ErrRIExchangeDailyCapExceeded = errors.New("RI exchange daily cap exceeded")

// reserveRIExchangeCeiling takes the global daily-spend lock (held until tx ends)
// and returns min(perExchangeCap, dailyCap - spend) as the execution ceiling,
// refusing when the initial quote does not fit. excludeID leaves one row's own
// amount out of spend so an approval does not count itself twice.
func reserveRIExchangeCeiling(ctx context.Context, tx pgx.Tx, excludeID *string, initialText string, initial, dailyCap, perCap *big.Int, dailyCapUSD string) (string, error) {
	if _, lockErr := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", riExchangeDailySpendLockKey); lockErr != nil {
		return "", fmt.Errorf("failed to lock RI exchange daily spend: %w", lockErr)
	}
	var spendText string
	if scanErr := tx.QueryRow(ctx, riExchangeDailySpendQuery, time.Now().UTC(), excludeID).Scan(&spendText); scanErr != nil {
		return "", fmt.Errorf("failed to read RI exchange daily spend: %w", scanErr)
	}
	spend, parseErr := riExchangeMicros(spendText)
	if parseErr != nil {
		return "", fmt.Errorf("invalid RI exchange daily spend: %w", parseErr)
	}
	remaining := new(big.Int).Sub(dailyCap, spend)
	if remaining.Cmp(perCap) > 0 {
		remaining.Set(perCap)
	}
	if remaining.Sign() < 0 {
		return "", fmt.Errorf("%w: spend %s exceeds cap %s", ErrRIExchangeDailyCapExceeded, spendText, dailyCapUSD)
	}
	if initial.Cmp(remaining) > 0 {
		return "", fmt.Errorf("%w: initial %s, remaining %s", ErrRIExchangeDailyCapExceeded, initialText, riExchangeUSD(remaining))
	}
	return riExchangeUSD(remaining), nil
}

func riExchangeReservationMicros(record *RIExchangeRecord, dailyCapUSD, perExchangeCapUSD string) (initial, dailyCap, perCap *big.Int, err error) {
	if record == nil || record.Status != "processing" || record.Mode != "auto" {
		return nil, nil, nil, errors.New("auto RI exchange reservation requires a processing auto record")
	}
	initial, err = riExchangeMicros(record.PaymentDue)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid initial RI exchange payment: %w", err)
	}
	dailyCap, err = riExchangeMicros(dailyCapUSD)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid daily RI exchange cap: %w", err)
	}
	perCap, err = riExchangeMicros(perExchangeCapUSD)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid per-exchange RI cap: %w", err)
	}
	return initial, dailyCap, perCap, nil
}

var riExchangeMicroScale = big.NewInt(1_000_000)
var riExchangeDecimalPattern = regexp.MustCompile(`^\d+(?:\.\d+)?$`)

func riExchangeMicros(value string) (*big.Int, error) {
	if !riExchangeDecimalPattern.MatchString(value) {
		return nil, fmt.Errorf("invalid nonnegative decimal %q", value)
	}
	amount, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, fmt.Errorf("invalid nonnegative decimal %q", value)
	}
	amount.Mul(amount, new(big.Rat).SetInt(riExchangeMicroScale))
	if !amount.IsInt() || len(new(big.Int).Quo(amount.Num(), riExchangeMicroScale).String()) > 14 {
		return nil, fmt.Errorf("decimal exceeds DECIMAL(20,6): %q", value)
	}
	return new(big.Int).Set(amount.Num()), nil
}

func riExchangeUSD(micros *big.Int) string {
	whole, fractional := new(big.Int), new(big.Int)
	whole.QuoRem(micros, riExchangeMicroScale, fractional)
	return fmt.Sprintf("%s.%06d", whole, fractional)
}

// GetRIExchangeRecord retrieves an RI exchange record by ID.
func (s *PostgresStore) GetRIExchangeRecord(ctx context.Context, id string) (*RIExchangeRecord, error) {
	query := `
		SELECT id, account_id, exchange_id, region, source_ri_ids,
		       source_instance_type, source_count, target_offering_id,
		       target_instance_type, target_count, payment_due::text,
		       status, approval_token_hash, error, mode,
		       created_at, updated_at, completed_at, expires_at,
		       created_by_user_id, approved_by, ladder_run_id
		FROM ri_exchange_history
		WHERE id = $1
	`

	records, err := s.queryRIExchangeRecords(ctx, query, id)
	if err != nil {
		return nil, err
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("ri exchange record not found: %s", id)
	}

	return &records[0], nil
}

// GetRIExchangeRecordByToken retrieves an RI exchange record by approval
// token. token is the raw, caller-supplied value; only its hash is stored
// (approval_token_hash, issue #103), so the lookup hashes it first.
func (s *PostgresStore) GetRIExchangeRecordByToken(ctx context.Context, token string) (*RIExchangeRecord, error) {
	query := `
		SELECT id, account_id, exchange_id, region, source_ri_ids,
		       source_instance_type, source_count, target_offering_id,
		       target_instance_type, target_count, payment_due::text,
		       status, approval_token_hash, error, mode,
		       created_at, updated_at, completed_at, expires_at,
		       created_by_user_id, approved_by, ladder_run_id
		FROM ri_exchange_history
		WHERE approval_token_hash = $1
	`

	records, err := s.queryRIExchangeRecords(ctx, query, HashApprovalToken(token))
	if err != nil {
		return nil, err
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("ri exchange record not found for token")
	}

	return &records[0], nil
}

// GetRIExchangeHistory retrieves RI exchange history records.
func (s *PostgresStore) GetRIExchangeHistory(ctx context.Context, since time.Time, limit int) ([]RIExchangeRecord, error) {
	query := `
		SELECT id, account_id, exchange_id, region, source_ri_ids,
		       source_instance_type, source_count, target_offering_id,
		       target_instance_type, target_count, payment_due::text,
		       status, approval_token_hash, error, mode,
		       created_at, updated_at, completed_at, expires_at,
		       created_by_user_id, approved_by, ladder_run_id
		FROM ri_exchange_history
		WHERE created_at >= $1
		ORDER BY created_at DESC
		LIMIT $2
	`

	return s.queryRIExchangeRecords(ctx, query, since, limit)
}

// GetRIExchangeHistoryForAccounts retrieves RI exchange history records for
// the given account ids only. The account predicate runs in SQL, before the
// LIMIT, so a scoped caller gets up to limit of their own records rather
// than whatever survives a filter applied to the newest limit records of
// every account (issue #308). An empty or nil accountIDs matches no records:
// "= ANY('{}')" is false and "= ANY(NULL)" is NULL for every row.
func (s *PostgresStore) GetRIExchangeHistoryForAccounts(ctx context.Context, since time.Time, limit int, accountIDs []string) ([]RIExchangeRecord, error) {
	query := `
		SELECT id, account_id, exchange_id, region, source_ri_ids,
		       source_instance_type, source_count, target_offering_id,
		       target_instance_type, target_count, payment_due::text,
		       status, approval_token_hash, error, mode,
		       created_at, updated_at, completed_at, expires_at,
		       created_by_user_id, approved_by, ladder_run_id
		FROM ri_exchange_history
		WHERE created_at >= $1 AND account_id = ANY($3::text[])
		ORDER BY created_at DESC
		LIMIT $2
	`

	return s.queryRIExchangeRecords(ctx, query, since, limit, accountIDs)
}

// TransitionRIExchangeStatus atomically transitions an RI exchange record status.
// Uses a single UPDATE...WHERE...RETURNING for atomicity, then diagnoses failure
// only if zero rows are returned.
// actor is the UUID of the user performing the transition (nil for system-initiated paths).
func (s *PostgresStore) TransitionRIExchangeStatus(ctx context.Context, id, fromStatus, toStatus string, actor *string) (*RIExchangeRecord, error) {
	query := `
		UPDATE ri_exchange_history
		SET status = $3, updated_at = NOW(),
		    transitioned_by = $4, transitioned_at = NOW()
		WHERE id = $1 AND status = $2 AND (expires_at IS NULL OR expires_at > NOW())
		RETURNING id, account_id, exchange_id, region, source_ri_ids,
		          source_instance_type, source_count, target_offering_id,
		          target_instance_type, target_count, payment_due::text,
		          status, approval_token_hash, error, mode,
		          created_at, updated_at, completed_at, expires_at,
		          created_by_user_id, approved_by, ladder_run_id
	`

	records, err := s.queryRIExchangeRecords(ctx, query, id, fromStatus, toStatus, actor)
	if err != nil {
		return nil, err
	}

	if len(records) == 0 {
		// Diagnose: not found vs wrong status vs expired.
		return nil, s.diagnoseTransitionFailure(ctx, id, fromStatus)
	}

	return &records[0], nil
}

// diagnoseTransitionFailure determines why a status transition returned zero rows.
func (s *PostgresStore) diagnoseTransitionFailure(ctx context.Context, id, fromStatus string) error {
	var currentStatus string
	var expired bool
	err := s.db.QueryRow(ctx,
		`SELECT status, (expires_at IS NOT NULL AND expires_at <= NOW()) FROM ri_exchange_history WHERE id = $1`, id,
	).Scan(&currentStatus, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("ri exchange record not found: %s", id)
	}
	if err != nil {
		return fmt.Errorf("failed to diagnose transition failure: %w", err)
	}
	if expired {
		return fmt.Errorf("ri exchange has expired")
	}
	return fmt.Errorf("ri exchange status transition failed: expected status %q but current status is %q", fromStatus, currentStatus)
}

// CompleteRIExchange marks an RI exchange as completed.
func (s *PostgresStore) CompleteRIExchange(ctx context.Context, id, exchangeID string) error {
	query := `
		UPDATE ri_exchange_history
		SET status = 'completed', exchange_id = $2, completed_at = NOW()
		WHERE id = $1
	`

	result, err := s.db.Exec(ctx, query, id, exchangeID)
	if err != nil {
		return fmt.Errorf("failed to complete ri exchange: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("ri exchange record not found: %s", id)
	}

	return nil
}

// CompleteRIExchangeWithPayment marks an RI exchange as completed and updates
// payment_due to the amount AWS actually accepted. Use this on the
// manual-approval path instead of CompleteRIExchange so the daily-spend ledger
// (GetRIExchangeDailySpend) reflects the accepted amount, not the stale
// pre-execution quote (H3 fix).
func (s *PostgresStore) CompleteRIExchangeWithPayment(ctx context.Context, id, exchangeID, acceptedPaymentDue string) error {
	query := `
		UPDATE ri_exchange_history
		SET status = 'completed', exchange_id = $2, completed_at = NOW(), payment_due = $3
		WHERE id = $1
	`

	result, err := s.db.Exec(ctx, query, id, exchangeID, acceptedPaymentDue)
	if err != nil {
		return fmt.Errorf("failed to complete ri exchange with payment: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("ri exchange record not found: %s", id)
	}

	return nil
}

// StampRIExchangeApprovedBy sets the approved_by column on an RI exchange row
// (issue #300). Called after CompleteRIExchangeWithPayment when approval came
// from a session-authed user. The stamping is best-effort (log + continue on
// failure so the exchange itself isn't rolled back just because the audit stamp
// failed).
func (s *PostgresStore) StampRIExchangeApprovedBy(ctx context.Context, id, approverEmail string) error {
	query := `
		UPDATE ri_exchange_history
		SET approved_by = $2
		WHERE id = $1
	`

	result, err := s.db.Exec(ctx, query, id, approverEmail)
	if err != nil {
		return fmt.Errorf("failed to stamp approved_by on ri exchange %s: %w", id, err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("ri exchange record not found when stamping approved_by: %s", id)
	}
	return nil
}

// FailRIExchange marks an RI exchange as failed.
func (s *PostgresStore) FailRIExchange(ctx context.Context, id, errorMsg string) error {
	query := `
		UPDATE ri_exchange_history
		SET status = 'failed', error = $2
		WHERE id = $1
	`

	result, err := s.db.Exec(ctx, query, id, errorMsg)
	if err != nil {
		return fmt.Errorf("failed to fail ri exchange: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("ri exchange record not found: %s", id)
	}

	return nil
}

// GetRIExchangeDailySpend returns total payment_due for completed and in-flight
// (processing) exchanges on a given date (UTC).
//
// M5 fix: including 'processing' rows prevents a TOCTOU race where two
// concurrent approvals both read the same daily-spend total (before either
// exchange's ledger row is committed) and together exceed the daily cap.
// For 'completed' rows the time anchor is completed_at; for 'processing' rows
// it is updated_at (the moment the record transitioned to processing, i.e.
// when it was approved).
func (s *PostgresStore) GetRIExchangeDailySpend(ctx context.Context, date time.Time) (string, error) {
	query := riExchangeDailySpendQuery

	var total string
	err := s.db.QueryRow(ctx, query, date, nil).Scan(&total)
	if err != nil {
		return "", fmt.Errorf("failed to get ri exchange daily spend: %w", err)
	}

	return total, nil
}

// riExchangeDailySpendQuery counts completed exchanges accepted inside the
// requested UTC day plus every processing reservation. A processing row is an
// unsettled hold, so it stays counted across UTC midnight until it completes or
// fails. $2 optionally leaves one row out (an approval's own reservation).
const riExchangeDailySpendQuery = `
		SELECT COALESCE(SUM(payment_due), 0)::text
		FROM ri_exchange_history
		WHERE ($2::uuid IS NULL OR id <> $2::uuid)
		  AND (status = 'processing'
		       OR (status = 'completed'
		           AND COALESCE(completed_at, updated_at) >= (date_trunc('day', $1::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')
		           AND COALESCE(completed_at, updated_at) < (date_trunc('day', $1::timestamptz AT TIME ZONE 'UTC') + INTERVAL '1 day') AT TIME ZONE 'UTC'))
	`

// CancelAllPendingExchanges cancels all pending RI exchange records regardless
// of origin. Kept for interface compatibility; new callers should prefer
// CancelPendingExchangesByOrigin to avoid cross-origin contamination.
func (s *PostgresStore) CancelAllPendingExchanges(ctx context.Context) (int64, error) {
	query := `
		UPDATE ri_exchange_history
		SET status = $1, updated_at = NOW()
		WHERE status = 'pending'
	`

	result, err := s.db.Exec(ctx, query, StatusCanceled)
	if err != nil {
		return 0, fmt.Errorf("failed to cancel pending exchanges: %w", err)
	}

	return result.RowsAffected(), nil
}

// CancelPendingExchangesByOrigin cancels only pending records that match the
// given origin (gap G10 / issue #1348):
//   - common.ExchangeOriginStandalone: cancels WHERE ladder_run_id IS NULL
//   - common.ExchangeOriginLadder:     cancels WHERE ladder_run_id IS NOT NULL
//
// The origin is validated at this boundary; an unknown value fails loud rather
// than silently canceling the wrong partition on a money path.
//
// DELIBERATE COARSE PARTITION: the ExchangeOriginLadder branch cancels EVERY
// ladder-linked pending record (ladder_run_id IS NOT NULL) across ALL ladder
// runs and configs, not just the current run's. This is acceptable today
// because the ladder never creates pending exchange records: buildRIExchangeConfig
// forces Mode=auto, which completes or fails immediately without leaving a
// pending row. Per-run / per-config scoping needs an additional selection key
// (e.g. the specific ladder_run_id or config_id) and is tracked in TODO(#1367).
func (s *PostgresStore) CancelPendingExchangesByOrigin(ctx context.Context, origin common.ExchangeOrigin) (int64, error) {
	if err := origin.Validate(); err != nil {
		return 0, fmt.Errorf("CancelPendingExchangesByOrigin: %w", err)
	}

	var query string
	switch origin {
	case common.ExchangeOriginLadder:
		query = `
			UPDATE ri_exchange_history
			SET status = $1, updated_at = NOW()
			WHERE status = 'pending'
			  AND ladder_run_id IS NOT NULL
		`
	default: // common.ExchangeOriginStandalone (validated non-unknown above)
		query = `
			UPDATE ri_exchange_history
			SET status = $1, updated_at = NOW()
			WHERE status = 'pending'
			  AND ladder_run_id IS NULL
		`
	}

	result, err := s.db.Exec(ctx, query, StatusCanceled)
	if err != nil {
		return 0, fmt.Errorf("failed to cancel pending exchanges by origin %q: %w", origin, err)
	}

	return result.RowsAffected(), nil
}

// GetStaleProcessingExchanges returns processing exchanges older than the given duration.
func (s *PostgresStore) GetStaleProcessingExchanges(ctx context.Context, olderThan time.Duration) ([]RIExchangeRecord, error) {
	query := `
		SELECT id, account_id, exchange_id, region, source_ri_ids,
		       source_instance_type, source_count, target_offering_id,
		       target_instance_type, target_count, payment_due::text,
		       status, approval_token_hash, error, mode,
		       created_at, updated_at, completed_at, expires_at,
		       created_by_user_id, approved_by, ladder_run_id
		FROM ri_exchange_history
		WHERE status = 'processing' AND updated_at < NOW() - $1::interval
	`

	return s.queryRIExchangeRecords(ctx, query, fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
}

// queryRIExchangeRecords is a helper to query and scan RI exchange records.
func (s *PostgresStore) queryRIExchangeRecords(ctx context.Context, query string, args ...any) ([]RIExchangeRecord, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query ri exchange records: %w", err)
	}
	defer rows.Close()

	records := make([]RIExchangeRecord, 0)
	for rows.Next() {
		var record RIExchangeRecord
		var approvalToken, errStr sql.NullString
		var completedAt, expiresAt sql.NullTime
		var createdByUserID, approvedBy, ladderRunID sql.NullString

		err := rows.Scan(
			&record.ID,
			&record.AccountID,
			&record.ExchangeID,
			&record.Region,
			&record.SourceRIIDs,
			&record.SourceInstanceType,
			&record.SourceCount,
			&record.TargetOfferingID,
			&record.TargetInstanceType,
			&record.TargetCount,
			&record.PaymentDue,
			&record.Status,
			&approvalToken,
			&errStr,
			&record.Mode,
			&record.CreatedAt,
			&record.UpdatedAt,
			&completedAt,
			&expiresAt,
			&createdByUserID,
			&approvedBy,
			&ladderRunID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan ri exchange record: %w", err)
		}

		if approvalToken.Valid {
			record.ApprovalToken = approvalToken.String
		}
		if errStr.Valid {
			record.Error = errStr.String
		}
		if completedAt.Valid {
			record.CompletedAt = &completedAt.Time
		}
		if expiresAt.Valid {
			record.ExpiresAt = &expiresAt.Time
		}
		record.CreatedByUserID = nullPtrFromNullString(createdByUserID)
		record.ApprovedBy = nullPtrFromNullString(approvedBy)
		record.LadderRunID = nullPtrFromNullString(ladderRunID)

		records = append(records, record)
	}

	return records, rows.Err()
}

// ==========================================
// CLOUD ACCOUNTS
// ==========================================

// CreateCloudAccount inserts a new cloud account record.
func (s *PostgresStore) CreateCloudAccount(ctx context.Context, account *CloudAccount) error {
	if account.ID == "" {
		account.ID = uuid.New().String()
	}
	now := time.Now()
	account.CreatedAt = now
	account.UpdatedAt = now

	query := `
		INSERT INTO cloud_accounts (
			id, name, description, contact_email, enabled,
			provider, external_id,
			aws_auth_mode, aws_role_arn, aws_external_id, aws_bastion_id, aws_web_identity_token_file, aws_is_org_root,
			azure_subscription_id, azure_tenant_id, azure_client_id, azure_auth_mode,
			gcp_project_id, gcp_client_email, gcp_auth_mode, gcp_wif_audience,
			created_at, updated_at, created_by
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7,
			$8, $9, $10, $11, $12, $13,
			$14, $15, $16, $17,
			$18, $19, $20, $21,
			$22, $23, $24
		)
	`

	_, err := s.db.Exec(ctx, query,
		account.ID,
		account.Name,
		nullStringFromString(account.Description),
		nullStringFromString(account.ContactEmail),
		account.Enabled,
		account.Provider,
		account.ExternalID,
		nullStringFromString(account.AWSAuthMode),
		nullStringFromString(account.AWSRoleARN),
		nullStringFromString(account.AWSExternalID),
		nullStringFromString(account.AWSBastionID),
		nullStringFromString(account.AWSWebIdentityTokenFile),
		account.AWSIsOrgRoot,
		nullStringFromString(account.AzureSubscriptionID),
		nullStringFromString(account.AzureTenantID),
		nullStringFromString(account.AzureClientID),
		nullStringFromString(account.AzureAuthMode),
		nullStringFromString(account.GCPProjectID),
		nullStringFromString(account.GCPClientEmail),
		nullStringFromString(account.GCPAuthMode),
		nullStringFromString(account.GCPWIFAudience),
		account.CreatedAt,
		account.UpdatedAt,
		nullStringFromString(account.CreatedBy),
	)
	if err != nil {
		return fmt.Errorf("failed to create cloud account: %w", err)
	}
	return nil
}

// GetCloudAccount returns a single cloud account by ID with credentials_configured derived.
func (s *PostgresStore) GetCloudAccount(ctx context.Context, id string) (*CloudAccount, error) {
	query := `
		SELECT
			ca.id, ca.name, COALESCE(ca.description,''), COALESCE(ca.contact_email,''),
			ca.enabled, ca.provider, ca.external_id,
			COALESCE(ca.aws_auth_mode,''), COALESCE(ca.aws_role_arn,''),
			COALESCE(ca.aws_external_id,''), COALESCE(ca.aws_bastion_id::text,''),
			COALESCE(ca.aws_web_identity_token_file,''),
			ca.aws_is_org_root,
			COALESCE(ca.azure_subscription_id,''), COALESCE(ca.azure_tenant_id,''),
			COALESCE(ca.azure_client_id,''), COALESCE(ca.azure_auth_mode,''),
			COALESCE(ca.gcp_project_id,''), COALESCE(ca.gcp_client_email,''), COALESCE(ca.gcp_auth_mode,''),
			COALESCE(ca.gcp_wif_audience,''),
			ca.created_at, ca.updated_at, COALESCE(ca.created_by::text,''),
			EXISTS(SELECT 1 FROM account_credentials ac WHERE ac.account_id = ca.id) AS credentials_configured
		FROM cloud_accounts ca
		WHERE ca.id = $1
	`

	var account CloudAccount
	err := s.db.QueryRow(ctx, query, id).Scan(
		&account.ID, &account.Name, &account.Description, &account.ContactEmail,
		&account.Enabled, &account.Provider, &account.ExternalID,
		&account.AWSAuthMode, &account.AWSRoleARN, &account.AWSExternalID, &account.AWSBastionID,
		&account.AWSWebIdentityTokenFile,
		&account.AWSIsOrgRoot,
		&account.AzureSubscriptionID, &account.AzureTenantID, &account.AzureClientID, &account.AzureAuthMode,
		&account.GCPProjectID, &account.GCPClientEmail, &account.GCPAuthMode,
		&account.GCPWIFAudience,
		&account.CreatedAt, &account.UpdatedAt, &account.CreatedBy,
		&account.CredentialsConfigured,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get cloud account: %w", err)
	}
	return &account, nil
}

// GetCloudAccountByExternalID returns a single cloud account matched by
// (provider, external_id). Used by the scheduler ambient-path tagging fix
// for issue #604 — when the Lambda's STS identity matches a registered
// account (regardless of enabled state), rec rows are stamped with that
// account's UUID so the approve modal shows the account name instead of
// `(ambient)`. Returns (nil, nil) when no row matches.
//
// The cloud_accounts table declares `UNIQUE(provider, external_id)`
// (migration 000011), which guarantees the lookup hits an index.
func (s *PostgresStore) GetCloudAccountByExternalID(ctx context.Context, provider, externalID string) (*CloudAccount, error) {
	query := `
		SELECT
			ca.id, ca.name, COALESCE(ca.description,''), COALESCE(ca.contact_email,''),
			ca.enabled, ca.provider, ca.external_id,
			COALESCE(ca.aws_auth_mode,''), COALESCE(ca.aws_role_arn,''),
			COALESCE(ca.aws_external_id,''), COALESCE(ca.aws_bastion_id::text,''),
			COALESCE(ca.aws_web_identity_token_file,''),
			ca.aws_is_org_root,
			COALESCE(ca.azure_subscription_id,''), COALESCE(ca.azure_tenant_id,''),
			COALESCE(ca.azure_client_id,''), COALESCE(ca.azure_auth_mode,''),
			COALESCE(ca.gcp_project_id,''), COALESCE(ca.gcp_client_email,''), COALESCE(ca.gcp_auth_mode,''),
			COALESCE(ca.gcp_wif_audience,''),
			ca.created_at, ca.updated_at, COALESCE(ca.created_by::text,''),
			EXISTS(SELECT 1 FROM account_credentials ac WHERE ac.account_id = ca.id) AS credentials_configured
		FROM cloud_accounts ca
		WHERE ca.provider = $1 AND ca.external_id = $2
	`

	var account CloudAccount
	err := s.db.QueryRow(ctx, query, provider, externalID).Scan(
		&account.ID, &account.Name, &account.Description, &account.ContactEmail,
		&account.Enabled, &account.Provider, &account.ExternalID,
		&account.AWSAuthMode, &account.AWSRoleARN, &account.AWSExternalID, &account.AWSBastionID,
		&account.AWSWebIdentityTokenFile,
		&account.AWSIsOrgRoot,
		&account.AzureSubscriptionID, &account.AzureTenantID, &account.AzureClientID, &account.AzureAuthMode,
		&account.GCPProjectID, &account.GCPClientEmail, &account.GCPAuthMode,
		&account.GCPWIFAudience,
		&account.CreatedAt, &account.UpdatedAt, &account.CreatedBy,
		&account.CredentialsConfigured,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get cloud account by external id: %w", err)
	}
	return &account, nil
}

// UpdateCloudAccount updates mutable fields of a cloud account.
func (s *PostgresStore) UpdateCloudAccount(ctx context.Context, account *CloudAccount) error {
	account.UpdatedAt = time.Now()
	query := `
		UPDATE cloud_accounts SET
			name = $2,
			description = $3,
			contact_email = $4,
			enabled = $5,
			external_id = $6,
			aws_auth_mode = $7,
			aws_role_arn = $8,
			aws_external_id = $9,
			aws_bastion_id = $10,
			aws_web_identity_token_file = $11,
			aws_is_org_root = $12,
			azure_subscription_id = $13,
			azure_tenant_id = $14,
			azure_client_id = $15,
			azure_auth_mode = $16,
			gcp_project_id = $17,
			gcp_client_email = $18,
			gcp_auth_mode = $19,
			gcp_wif_audience = $20,
			updated_at = $21
		WHERE id = $1
	`
	tag, err := s.db.Exec(ctx, query,
		account.ID,
		account.Name,
		nullStringFromString(account.Description),
		nullStringFromString(account.ContactEmail),
		account.Enabled,
		account.ExternalID,
		nullStringFromString(account.AWSAuthMode),
		nullStringFromString(account.AWSRoleARN),
		nullStringFromString(account.AWSExternalID),
		nullStringFromString(account.AWSBastionID),
		nullStringFromString(account.AWSWebIdentityTokenFile),
		account.AWSIsOrgRoot,
		nullStringFromString(account.AzureSubscriptionID),
		nullStringFromString(account.AzureTenantID),
		nullStringFromString(account.AzureClientID),
		nullStringFromString(account.AzureAuthMode),
		nullStringFromString(account.GCPProjectID),
		nullStringFromString(account.GCPClientEmail),
		nullStringFromString(account.GCPAuthMode),
		nullStringFromString(account.GCPWIFAudience),
		account.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to update cloud account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cloud account not found: %s", account.ID)
	}
	return nil
}

// DeleteCloudAccount deletes a cloud account. Cascades to credentials and overrides.
// Terminal purchase executions are kept as history with cloud_account_id set to
// NULL; a live execution still blocks the delete through the FK (issue #265).
// If an approved account_registrations row points at this account, it is reset to
// 'pending' in the same transaction so the admin can re-approve through the normal
// flow instead of being left with a dead-end "Approved (account pending link)" row.
// Enabled plans whose only target is this account are disabled in the same
// transaction (issue #264); see disableSoleAccountPlansTx.
func (s *PostgresStore) DeleteCloudAccount(ctx context.Context, id string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer rollbackTx(ctx, tx, "DeleteCloudAccount")

	if lockErr := lockAccountForDeleteTx(ctx, tx, id); lockErr != nil {
		return lockErr
	}

	// Reset any linked approved registration first (explicit NULL so we don't
	// rely on the FK's ON DELETE SET NULL behavior).
	if _, err = tx.Exec(ctx, `
		UPDATE account_registrations
		   SET status           = 'pending',
		       reviewed_by      = NULL,
		       reviewed_at      = NULL,
		       cloud_account_id = NULL
		 WHERE cloud_account_id = $1
		   AND status           = 'approved'
	`, id); err != nil {
		return fmt.Errorf("failed to reset linked registration: %w", err)
	}

	// Detach the account's terminal executions so they survive as history
	// (issue #265). Migration 000053's ON DELETE RESTRICT FK otherwise blocks
	// the delete for every referencing row whatever its status. Live rows are
	// left attached, so the DELETE below still fails on the FK (SQLSTATE
	// 23503) while any remain, and this UPDATE rolls back with it. Migration
	// 000128 keeps updated_at unchanged on these rows: detaching is not a
	// lifecycle event, and the plan health score and the retention sweep both
	// read updated_at as one.
	if _, err = tx.Exec(ctx, `
		UPDATE purchase_executions
		   SET cloud_account_id = NULL
		 WHERE cloud_account_id = $1
		   AND status = ANY($2)
	`, id, TerminalExecutionStatuses); err != nil {
		return fmt.Errorf("failed to detach terminal executions: %w", err)
	}

	disabled, err := disableSoleAccountPlansTx(ctx, tx, id)
	if err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `DELETE FROM cloud_accounts WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("failed to delete cloud account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cloud account not found: %s", id)
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit cloud account deletion: %w", err)
	}
	if len(disabled) > 0 {
		logging.Infof("deleting cloud account %s disabled %d plan(s) left with no target accounts: %v", id, len(disabled), disabled)
	}
	return nil
}

// lockAccountForDeleteTx takes DeleteCloudAccount's locks (issues #264,
// #1072) in this order: the plans that target the account, then the account
// row, then any plan that gained the account meanwhile. The plan locks
// serialize the delete with a concurrent delete of another of the plan's
// accounts and with SetPlanAccounts (which reads the plan FOR SHARE), so the
// plan disable decides on committed plan_accounts rows. The account lock waits
// for a writer still attaching the account to a plan (it holds KEY SHARE on
// the row through the plan_accounts FK until it commits); the second plan lock
// then takes the plans it attached.
func lockAccountForDeleteTx(ctx context.Context, tx pgx.Tx, id string) error {
	if err := lockAccountPlansTx(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM cloud_accounts WHERE id = $1 FOR UPDATE`, id); err != nil {
		return fmt.Errorf("failed to lock cloud account: %w", err)
	}
	return lockAccountPlansTx(ctx, tx, id)
}

// lockAccountPlansTx locks, in id order, every plan with a plan_accounts row
// for accountID. A fixed order keeps two account deletes that share plans from
// deadlocking on each other.
func lockAccountPlansTx(ctx context.Context, tx pgx.Tx, accountID string) error {
	if _, err := tx.Exec(ctx, `
		SELECT p.id FROM purchase_plans p
		 WHERE p.id IN (SELECT pa.plan_id FROM plan_accounts pa WHERE pa.account_id = $1)
		 ORDER BY p.id
		   FOR UPDATE OF p
	`, accountID); err != nil {
		return fmt.Errorf("failed to lock plans of cloud account: %w", err)
	}
	return nil
}

// disableSoleAccountPlansTx disables every enabled plan whose only
// plan_accounts row is accountID, and returns their IDs (issue #264). The
// account delete cascades those rows away, and a plan with no accounts is the
// unscoped "universal plan" shape migration 000060 removed: the scheduler
// would keep creating executions for it with no account to buy in. Disabling
// rather than refusing the delete keeps the plan and its history for the
// admin to re-target and re-enable. Plans already disabled are not rewritten,
// so their updated_at (the plan edit token) does not move. The caller holds
// the plan rows locked (lockAccountPlansTx), so writers that lock the plan
// before changing its plan_accounts rows (SetPlanAccounts, another account
// delete) cannot change them under this statement's decision.
func disableSoleAccountPlansTx(ctx context.Context, tx pgx.Tx, accountID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		UPDATE purchase_plans p
		   SET enabled = false
		 WHERE p.enabled
		   AND EXISTS (SELECT 1 FROM plan_accounts pa WHERE pa.plan_id = p.id AND pa.account_id = $1)
		   AND NOT EXISTS (SELECT 1 FROM plan_accounts pa WHERE pa.plan_id = p.id AND pa.account_id <> $1)
		RETURNING p.id::text
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to disable plans targeting only this account: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("failed to disable plans targeting only this account: %w", err)
	}
	return ids, nil
}

// ListCloudAccounts returns accounts matching the filter, with credentials_configured derived.
func (s *PostgresStore) ListCloudAccounts(ctx context.Context, filter CloudAccountFilter) ([]CloudAccount, error) {
	query := `
		SELECT
			ca.id, ca.name, COALESCE(ca.description,''), COALESCE(ca.contact_email,''),
			ca.enabled, ca.provider, ca.external_id,
			COALESCE(ca.aws_auth_mode,''), COALESCE(ca.aws_role_arn,''),
			COALESCE(ca.aws_external_id,''), COALESCE(ca.aws_bastion_id::text,''),
			COALESCE(ca.aws_web_identity_token_file,''),
			ca.aws_is_org_root,
			COALESCE(ca.azure_subscription_id,''), COALESCE(ca.azure_tenant_id,''),
			COALESCE(ca.azure_client_id,''), COALESCE(ca.azure_auth_mode,''),
			COALESCE(ca.gcp_project_id,''), COALESCE(ca.gcp_client_email,''), COALESCE(ca.gcp_auth_mode,''),
			COALESCE(ca.gcp_wif_audience,''),
			ca.created_at, ca.updated_at, COALESCE(ca.created_by::text,''),
			EXISTS(SELECT 1 FROM account_credentials ac WHERE ac.account_id = ca.id) AS credentials_configured
		FROM cloud_accounts ca
		WHERE 1=1
	`
	args := []any{}
	i := 1

	if filter.Provider != nil {
		query += fmt.Sprintf(" AND ca.provider = $%d", i)
		args = append(args, *filter.Provider)
		i++
	}
	if filter.Enabled != nil {
		query += fmt.Sprintf(" AND ca.enabled = $%d", i)
		args = append(args, *filter.Enabled)
		i++
	}
	if filter.Search != "" {
		// Escape ILIKE wildcards so user-supplied % and _ are treated as literals.
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(filter.Search)
		// Bind once and reference the same $N twice — Postgres allows parameter
		// reuse, and the single-bind form keeps future filter additions from
		// having to reason about "+2" offsets.
		query += fmt.Sprintf(" AND (ca.name ILIKE $%d ESCAPE '\\' OR ca.external_id ILIKE $%d ESCAPE '\\')", i, i)
		args = append(args, "%"+escaped+"%")
		i++
	}
	if filter.BastionID != nil {
		query += fmt.Sprintf(" AND ca.aws_bastion_id = $%d", i)
		args = append(args, *filter.BastionID)
		i++
	}
	_ = i // suppress "declared but not used" if no more conditions follow

	query += " ORDER BY ca.name"

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list cloud accounts: %w", err)
	}
	defer rows.Close()

	accounts := make([]CloudAccount, 0)
	for rows.Next() {
		var a CloudAccount
		if err := rows.Scan(
			&a.ID, &a.Name, &a.Description, &a.ContactEmail,
			&a.Enabled, &a.Provider, &a.ExternalID,
			&a.AWSAuthMode, &a.AWSRoleARN, &a.AWSExternalID, &a.AWSBastionID,
			&a.AWSWebIdentityTokenFile,
			&a.AWSIsOrgRoot,
			&a.AzureSubscriptionID, &a.AzureTenantID, &a.AzureClientID, &a.AzureAuthMode,
			&a.GCPProjectID, &a.GCPClientEmail, &a.GCPAuthMode,
			&a.GCPWIFAudience,
			&a.CreatedAt, &a.UpdatedAt, &a.CreatedBy,
			&a.CredentialsConfigured,
		); err != nil {
			return nil, fmt.Errorf("failed to scan cloud account: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// ==========================================
// ACCOUNT CREDENTIALS
// ==========================================

// SaveAccountCredential upserts an encrypted credential blob for an account.
func (s *PostgresStore) SaveAccountCredential(ctx context.Context, accountID, credentialType, encryptedBlob string) error {
	query := `
		INSERT INTO account_credentials (id, account_id, credential_type, encrypted_blob)
		VALUES (uuid_generate_v4(), $1, $2, $3)
		ON CONFLICT (account_id, credential_type) DO UPDATE SET
			encrypted_blob = $3,
			updated_at = NOW()
	`
	_, err := s.db.Exec(ctx, query, accountID, credentialType, encryptedBlob)
	if err != nil {
		return fmt.Errorf("failed to save account credential: %w", err)
	}
	return nil
}

// GetAccountCredential returns the encrypted blob for an account credential.
func (s *PostgresStore) GetAccountCredential(ctx context.Context, accountID, credentialType string) (string, error) {
	var blob string
	err := s.db.QueryRow(ctx,
		`SELECT encrypted_blob FROM account_credentials WHERE account_id = $1 AND credential_type = $2`,
		accountID, credentialType,
	).Scan(&blob)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("failed to get account credential: %w", err)
	}
	return blob, nil
}

// DeleteAccountCredentials removes all credential records for an account.
func (s *PostgresStore) DeleteAccountCredentials(ctx context.Context, accountID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM account_credentials WHERE account_id = $1`, accountID)
	if err != nil {
		return fmt.Errorf("failed to delete account credentials: %w", err)
	}
	return nil
}

// HasAccountCredentials returns true if at least one credential exists for the account.
func (s *PostgresStore) HasAccountCredentials(ctx context.Context, accountID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM account_credentials WHERE account_id = $1)`,
		accountID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check account credentials: %w", err)
	}
	return exists, nil
}

// ==========================================
// ACCOUNT SERVICE OVERRIDES
// ==========================================

// GetAccountServiceOverride returns a single override, or nil if none exists.
func (s *PostgresStore) GetAccountServiceOverride(ctx context.Context, accountID, provider, service string) (*AccountServiceOverride, error) {
	query := `
		SELECT id, account_id, provider, service,
			enabled, term, payment, coverage, ramp_schedule,
			include_engines, exclude_engines, include_regions, exclude_regions,
			include_types, exclude_types,
			created_at, updated_at
		FROM account_service_overrides
		WHERE account_id = $1 AND provider = $2 AND service = $3
	`
	var o AccountServiceOverride
	var incEngines, excEngines, incRegions, excRegions, incTypes, excTypes []string
	err := s.db.QueryRow(ctx, query, accountID, provider, service).Scan(
		&o.ID, &o.AccountID, &o.Provider, &o.Service,
		&o.Enabled, &o.Term, &o.Payment, &o.Coverage, &o.RampSchedule,
		&incEngines, &excEngines, &incRegions, &excRegions, &incTypes, &excTypes,
		&o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get service override: %w", err)
	}
	o.IncludeEngines = incEngines
	o.ExcludeEngines = excEngines
	o.IncludeRegions = incRegions
	o.ExcludeRegions = excRegions
	o.IncludeTypes = incTypes
	o.ExcludeTypes = excTypes
	return &o, nil
}

// SaveAccountServiceOverride upserts an account service override.
func (s *PostgresStore) SaveAccountServiceOverride(ctx context.Context, o *AccountServiceOverride) error {
	if o.ID == "" {
		o.ID = uuid.New().String()
	}
	now := time.Now()
	// Only set CreatedAt for new records; preserve the original creation time on updates.
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	o.UpdatedAt = now

	query := `
		INSERT INTO account_service_overrides (
			id, account_id, provider, service,
			enabled, term, payment, coverage, ramp_schedule,
			include_engines, exclude_engines, include_regions, exclude_regions,
			include_types, exclude_types,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (account_id, provider, service) DO UPDATE SET
			enabled = $5, term = $6, payment = $7, coverage = $8, ramp_schedule = $9,
			include_engines = $10, exclude_engines = $11,
			include_regions = $12, exclude_regions = $13,
			include_types = $14, exclude_types = $15,
			updated_at = NOW()
	`
	_, err := s.db.Exec(ctx, query,
		o.ID, o.AccountID, o.Provider, o.Service,
		o.Enabled, o.Term, o.Payment, o.Coverage, o.RampSchedule,
		o.IncludeEngines, o.ExcludeEngines, o.IncludeRegions, o.ExcludeRegions,
		o.IncludeTypes, o.ExcludeTypes,
		o.CreatedAt, o.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to save service override: %w", err)
	}
	return nil
}

// DeleteAccountServiceOverride removes an override, reverting to global defaults.
func (s *PostgresStore) DeleteAccountServiceOverride(ctx context.Context, accountID, provider, service string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM account_service_overrides WHERE account_id = $1 AND provider = $2 AND service = $3`,
		accountID, provider, service,
	)
	if err != nil {
		return fmt.Errorf("failed to delete service override: %w", err)
	}
	return nil
}

// ListAccountServiceOverrides returns all overrides for an account.
func (s *PostgresStore) ListAccountServiceOverrides(ctx context.Context, accountID string) ([]AccountServiceOverride, error) {
	query := `
		SELECT id, account_id, provider, service,
			enabled, term, payment, coverage, ramp_schedule,
			include_engines, exclude_engines, include_regions, exclude_regions,
			include_types, exclude_types,
			created_at, updated_at
		FROM account_service_overrides
		WHERE account_id = $1
		ORDER BY provider, service
	`
	rows, err := s.db.Query(ctx, query, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to list service overrides: %w", err)
	}
	defer rows.Close()

	overrides := make([]AccountServiceOverride, 0)
	for rows.Next() {
		var o AccountServiceOverride
		var incEngines, excEngines, incRegions, excRegions, incTypes, excTypes []string
		if err := rows.Scan(
			&o.ID, &o.AccountID, &o.Provider, &o.Service,
			&o.Enabled, &o.Term, &o.Payment, &o.Coverage, &o.RampSchedule,
			&incEngines, &excEngines, &incRegions, &excRegions, &incTypes, &excTypes,
			&o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan service override: %w", err)
		}
		o.IncludeEngines = incEngines
		o.ExcludeEngines = excEngines
		o.IncludeRegions = incRegions
		o.ExcludeRegions = excRegions
		o.IncludeTypes = incTypes
		o.ExcludeTypes = excTypes
		overrides = append(overrides, o)
	}
	return overrides, rows.Err()
}

// ==========================================
// PLAN ACCOUNTS
// ==========================================

// SetPlanAccounts replaces the full account list for a plan atomically.
func (s *PostgresStore) SetPlanAccounts(ctx context.Context, planID string, accountIDs []string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer rollbackTx(ctx, tx, "SetPlanAccounts")

	if guardErr := requireManagedAccountsTx(ctx, tx, planID, accountIDs); guardErr != nil {
		return guardErr
	}
	if setErr := s.setPlanAccountsTx(ctx, tx, planID, accountIDs); setErr != nil {
		return setErr
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit plan accounts: %w", err)
	}
	return nil
}

func (s *PostgresStore) validatePlanAccountProvidersTx(ctx context.Context, tx pgx.Tx, planID string, accountIDs []string) error {
	services, err := s.getPlanServicesForShareTx(ctx, tx, planID)
	if err != nil {
		return err
	}
	if len(accountIDs) == 0 {
		return nil
	}
	expected := DerivePlanProviders(&PurchasePlan{Services: services})
	if len(expected) == 0 {
		return nil
	}

	mismatches, err := s.findPlanAccountProviderMismatchesTx(ctx, tx, accountIDs, expected)
	if err != nil {
		return err
	}
	if len(mismatches) == 0 {
		return nil
	}

	parts := make([]string, len(mismatches))
	for i, mismatch := range mismatches {
		parts[i] = fmt.Sprintf("account %q has provider=%q, expected one of %v",
			mismatch.Name, mismatch.Provider, expected)
	}
	return fmt.Errorf("plan provider mismatch: %s", strings.Join(parts, "; "))
}

func (s *PostgresStore) getPlanServicesForShareTx(ctx context.Context, tx pgx.Tx, planID string) (map[string]ServiceConfig, error) {
	var servicesJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT services
		FROM purchase_plans
		WHERE id = $1
		FOR SHARE
	`, planID).Scan(&servicesJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: plan %s", ErrNotFound, planID)
		}
		return nil, fmt.Errorf("failed to get plan services: %w", err)
	}

	services := make(map[string]ServiceConfig)
	if err := json.Unmarshal(servicesJSON, &services); err != nil {
		return nil, fmt.Errorf("failed to decode plan services: %w", err)
	}
	return services, nil
}

type planAccountProviderMismatch struct {
	Name     string
	Provider string
}

func (s *PostgresStore) findPlanAccountProviderMismatchesTx(ctx context.Context, tx pgx.Tx, accountIDs, expected []string) ([]planAccountProviderMismatch, error) {
	expectedSet := make(map[string]struct{}, len(expected))
	for _, provider := range expected {
		expectedSet[provider] = struct{}{}
	}

	var mismatches []planAccountProviderMismatch
	for _, accountID := range accountIDs {
		var name, provider string
		if err := tx.QueryRow(ctx, `
			SELECT name, provider
			FROM cloud_accounts
			WHERE id = $1
			FOR SHARE
		`, accountID).Scan(&name, &provider); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: account %s", ErrNotFound, accountID)
			}
			return nil, fmt.Errorf("failed to get account %s: %w", accountID, err)
		}
		if _, ok := expectedSet[provider]; !ok {
			mismatches = append(mismatches, planAccountProviderMismatch{Name: name, Provider: provider})
		}
	}
	return mismatches, nil
}

// GetPlanAccounts returns all cloud accounts associated with a plan.
func (s *PostgresStore) GetPlanAccounts(ctx context.Context, planID string) ([]CloudAccount, error) {
	query := `
		SELECT
			ca.id, ca.name, COALESCE(ca.description,''), COALESCE(ca.contact_email,''),
			ca.enabled, ca.provider, ca.external_id,
			COALESCE(ca.aws_auth_mode,''), COALESCE(ca.aws_role_arn,''),
			COALESCE(ca.aws_external_id,''), COALESCE(ca.aws_bastion_id::text,''),
			COALESCE(ca.aws_web_identity_token_file,''),
			ca.aws_is_org_root,
			COALESCE(ca.azure_subscription_id,''), COALESCE(ca.azure_tenant_id,''),
			COALESCE(ca.azure_client_id,''), COALESCE(ca.azure_auth_mode,''),
			COALESCE(ca.gcp_project_id,''), COALESCE(ca.gcp_client_email,''), COALESCE(ca.gcp_auth_mode,''),
			COALESCE(ca.gcp_wif_audience,''),
			ca.created_at, ca.updated_at, COALESCE(ca.created_by::text,''),
			EXISTS(SELECT 1 FROM account_credentials ac WHERE ac.account_id = ca.id) AS credentials_configured
		FROM cloud_accounts ca
		JOIN plan_accounts pa ON pa.account_id = ca.id
		WHERE pa.plan_id = $1
		ORDER BY ca.name
	`
	rows, err := s.db.Query(ctx, query, planID)
	if err != nil {
		return nil, fmt.Errorf("failed to get plan accounts: %w", err)
	}
	defer rows.Close()

	accounts := make([]CloudAccount, 0)
	for rows.Next() {
		var a CloudAccount
		if err := rows.Scan(
			&a.ID, &a.Name, &a.Description, &a.ContactEmail,
			&a.Enabled, &a.Provider, &a.ExternalID,
			&a.AWSAuthMode, &a.AWSRoleARN, &a.AWSExternalID, &a.AWSBastionID,
			&a.AWSWebIdentityTokenFile,
			&a.AWSIsOrgRoot,
			&a.AzureSubscriptionID, &a.AzureTenantID, &a.AzureClientID, &a.AzureAuthMode,
			&a.GCPProjectID, &a.GCPClientEmail, &a.GCPAuthMode,
			&a.GCPWIFAudience,
			&a.CreatedAt, &a.UpdatedAt, &a.CreatedBy,
			&a.CredentialsConfigured,
		); err != nil {
			return nil, fmt.Errorf("failed to scan plan account: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// ==========================================
// HELPER FUNCTIONS
// ==========================================

// timeFromTTL converts a Unix timestamp (TTL) to a nullable time.Time.
func timeFromTTL(ttl int64) any {
	if ttl == 0 {
		return nil
	}
	t := time.Unix(ttl, 0)
	return &t
}

// ttlFromTime converts a time.Time to Unix timestamp.
func ttlFromTime(t time.Time) int64 {
	return t.Unix()
}

// nullStringFromString converts a string to sql.NullString.
func nullStringFromString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullPtrFromNullString converts a scanned sql.NullString to a *string for
// optional pointer fields. Returns nil when the DB value is NULL.
func nullPtrFromNullString(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}

func nullPtrFromNullFloat64(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}

// ==========================================
// NOTIFICATION MUTES (issue #297)
// ==========================================

// UpsertNotificationMute inserts or replaces the mute row for (recipientEmail,
// scope). ON CONFLICT updates muted_at and unmute_token so a repeated
// one-click opt-out resets the audit timestamp without error.
func (s *PostgresStore) UpsertNotificationMute(ctx context.Context, recipientEmail, scope, unmuteToken string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO muted_recipients (recipient_email, scope, muted_at, unmute_token)
		VALUES (LOWER($1), $2, NOW(), $3)
		ON CONFLICT (recipient_email, scope)
		DO UPDATE SET muted_at = NOW(), unmute_token = EXCLUDED.unmute_token
	`, recipientEmail, scope, unmuteToken)
	if err != nil {
		return fmt.Errorf("upsert notification mute: %w", err)
	}
	return nil
}

// IsNotificationMuted returns true when (email, scope) has a matching row
// in muted_recipients. The email lookup is case-insensitive (LOWER on insert
// plus LOWER($1) here).
func (s *PostgresStore) IsNotificationMuted(ctx context.Context, recipientEmail, scope string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM muted_recipients
			WHERE recipient_email = LOWER($1) AND scope = $2
		)
	`, recipientEmail, scope).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check notification mute: %w", err)
	}
	return exists, nil
}
