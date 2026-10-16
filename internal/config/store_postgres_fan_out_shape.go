package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Fan-out shapes of a purchase execution lineage (issue #97). The shape decides
// which key the per-rec provider idempotency tokens derive from: the lineage key
// itself on the single-account path, "<lineage key>:<account id>" per account on
// the fan-out path.
const (
	FanOutShapeSingle = "single"
	FanOutShapeMulti  = "multi"
)

// PinExecutionFanOutShape records shape as the fan-out shape of the execution
// lineage keyed by lineageKey unless one is already recorded, and returns the
// recorded shape. The first writer wins and the row is never updated, so every
// later attempt of the lineage gets back the shape its first attempt ran with,
// however the plan's accounts have changed since.
func (s *PostgresStore) PinExecutionFanOutShape(ctx context.Context, lineageKey, shape string) (string, error) {
	if lineageKey == "" {
		return "", fmt.Errorf("cannot pin a fan-out shape for an empty lineage key")
	}
	if shape != FanOutShapeSingle && shape != FanOutShapeMulti {
		return "", fmt.Errorf("cannot pin unknown fan-out shape %q for lineage %s", shape, lineageKey)
	}
	// DO UPDATE with a no-op SET (rather than DO NOTHING) so RETURNING yields
	// the stored row on conflict, in one statement that also waits out a
	// concurrent first insert of the same key.
	var pinned string
	err := s.db.QueryRow(ctx, `
		INSERT INTO purchase_execution_fan_out_shapes (idempotency_key, shape)
		VALUES ($1, $2)
		ON CONFLICT (idempotency_key) DO UPDATE SET shape = purchase_execution_fan_out_shapes.shape
		RETURNING shape`, lineageKey, shape).Scan(&pinned)
	if err != nil {
		return "", fmt.Errorf("failed to pin fan-out shape for lineage %s: %w", lineageKey, err)
	}
	return pinned, nil
}

// GetExecutionFanOutShape returns the fan-out shape pinned for the execution
// lineage keyed by lineageKey, or "" when none is pinned yet.
func (s *PostgresStore) GetExecutionFanOutShape(ctx context.Context, lineageKey string) (string, error) {
	var shape string
	err := s.db.QueryRow(ctx,
		`SELECT shape FROM purchase_execution_fan_out_shapes WHERE idempotency_key = $1`, lineageKey).Scan(&shape)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read fan-out shape for lineage %s: %w", lineageKey, err)
	}
	return shape, nil
}
