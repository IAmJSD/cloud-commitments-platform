package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// azConfigSchemaChecker is implemented by the Postgres store. Stores without a
// schema (test doubles) have nothing to check.
type azConfigSchemaChecker interface {
	CheckAZConfigMigration(ctx context.Context) error
}

// azConfigNotReadyError maps ErrAZConfigMigrationNotReady to the 503 every
// az_config-dependent path returns, and passes any other error through.
func azConfigNotReadyError(err error) error {
	if !errors.Is(err, config.ErrAZConfigMigrationNotReady) {
		return err
	}
	return NewClientErrorWithDetails(503,
		fmt.Sprintf("database migrations %06d and %06d are not applied; recommendations and purchases are unavailable until they are",
			config.AZConfigRecommendationsMigration, config.AZConfigSuppressionsMigration),
		map[string]any{})
}

// requireAZConfigSchema refuses a purchase submission before its transaction
// opens when the az_config expand migrations are missing: the suppression
// insert names az_config, so it would fail mid-transaction.
func (h *Handler) requireAZConfigSchema(ctx context.Context) error {
	checker, ok := h.config.(azConfigSchemaChecker)
	if !ok {
		return nil
	}
	err := checker.CheckAZConfigMigration(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, config.ErrAZConfigMigrationNotReady) {
		return azConfigNotReadyError(err)
	}
	return fmt.Errorf("check az_config migration: %w", err)
}
