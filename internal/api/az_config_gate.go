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

// requireAZConfigSchema refuses a purchase submission before its transaction
// opens when the az_config expand migrations (000154, 000155) are missing: the
// suppression insert names az_config, so it would fail mid-transaction.
func (h *Handler) requireAZConfigSchema(ctx context.Context) error {
	checker, ok := h.config.(azConfigSchemaChecker)
	if !ok {
		return nil
	}
	err := checker.CheckAZConfigMigration(ctx)
	if errors.Is(err, config.ErrAZConfigMigrationNotReady) {
		return NewClientErrorWithDetails(503,
			fmt.Sprintf("purchases are unavailable until database migrations 000154 and 000155 are applied: %v", err),
			map[string]any{})
	}
	if err != nil {
		return fmt.Errorf("check az_config migration: %w", err)
	}
	return nil
}
