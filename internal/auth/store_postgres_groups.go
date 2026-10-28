package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ==========================================
// GROUP OPERATIONS
// ==========================================

// GetGroup retrieves a group by ID.
func (s *PostgresStore) GetGroup(ctx context.Context, groupID string) (*Group, error) {
	query := `
		SELECT id, name, description, permissions, allowed_accounts,
		       system_managed, created_at, updated_at, created_by
		FROM groups
		WHERE id = $1
	`

	return s.scanGroup(s.db.QueryRow(ctx, query, groupID))
}

// CreateGroup creates a new group.
func (s *PostgresStore) CreateGroup(ctx context.Context, group *Group) error {
	// Generate UUID if not provided
	if group.ID == "" {
		group.ID = uuid.New().String()
	}

	// Set timestamps
	now := time.Now()
	group.CreatedAt = now
	group.UpdatedAt = now

	// Marshal permissions to JSONB
	permissionsJSON, err := json.Marshal(group.Permissions)
	if err != nil {
		return fmt.Errorf("failed to marshal permissions: %w", err)
	}

	query := `
		INSERT INTO groups (
			id, name, description, permissions, allowed_accounts,
			created_at, updated_at, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`

	// created_by is a nullable UUID FK; convert empty string to NULL so
	// Postgres doesn't reject the insert with "invalid input syntax for
	// type uuid" when the caller doesn't have a user ID.
	var createdBy any = group.CreatedBy
	if group.CreatedBy == "" {
		createdBy = nil
	}

	_, err = s.db.Exec(ctx, query,
		group.ID,
		group.Name,
		group.Description,
		permissionsJSON,
		group.AllowedAccounts,
		group.CreatedAt,
		group.UpdatedAt,
		createdBy,
	)

	if err != nil {
		return fmt.Errorf("failed to create group: %w", err)
	}

	return nil
}

// UpdateGroup updates an existing group.
func (s *PostgresStore) UpdateGroup(ctx context.Context, group *Group) error {
	group.UpdatedAt = time.Now()

	// Marshal permissions to JSONB
	permissionsJSON, err := json.Marshal(group.Permissions)
	if err != nil {
		return fmt.Errorf("failed to marshal permissions: %w", err)
	}

	query := `
		UPDATE groups SET
			name = $2,
			description = $3,
			permissions = $4,
			allowed_accounts = $5,
			updated_at = $6
		WHERE id = $1
	`

	result, err := s.db.Exec(ctx, query,
		group.ID,
		group.Name,
		group.Description,
		permissionsJSON,
		group.AllowedAccounts,
		group.UpdatedAt,
	)

	if err != nil {
		return fmt.Errorf("failed to update group: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("group not found: %s", group.ID)
	}

	return nil
}

// DeleteGroup deletes a group.
func (s *PostgresStore) DeleteGroup(ctx context.Context, groupID string) error {
	query := `DELETE FROM groups WHERE id = $1`

	result, err := s.db.Exec(ctx, query, groupID)
	if err != nil {
		return fmt.Errorf("failed to delete group: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("group not found: %s", groupID)
	}

	return nil
}

// ListGroups lists all groups.
func (s *PostgresStore) ListGroups(ctx context.Context) ([]Group, error) {
	// LIMIT provides a safety cap against unbounded memory allocation.
	// Pagination support should be added if this limit proves insufficient.
	query := `
		SELECT id, name, description, permissions, allowed_accounts,
		       system_managed, created_at, updated_at, created_by
		FROM groups
		ORDER BY created_at DESC
		LIMIT 10000
	`

	rows, err := s.db.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list groups: %w", err)
	}
	defer rows.Close()

	groups := make([]Group, 0)
	for rows.Next() {
		group, err := s.scanGroup(rows)
		if err != nil {
			return nil, err
		}
		if group == nil {
			continue
		}
		groups = append(groups, *group)
	}

	return groups, rows.Err()
}

// scanGroup scans a group from a database row.
func (s *PostgresStore) scanGroup(scanner Scanner) (*Group, error) {
	var group Group
	var permissionsJSON []byte
	var allowedAccounts []string
	var createdBy sql.NullString

	err := scanner.Scan(
		&group.ID,
		&group.Name,
		&group.Description,
		&permissionsJSON,
		&allowedAccounts,
		&group.SystemManaged,
		&group.CreatedAt,
		&group.UpdatedAt,
		&createdBy,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, pgx.ErrNoRows
		}
		return nil, fmt.Errorf("failed to scan group: %w", err)
	}

	// Unmarshal permissions — guard against NULL column, consistent with scanAPIKey
	if len(permissionsJSON) > 0 {
		if err := json.Unmarshal(permissionsJSON, &group.Permissions); err != nil {
			return nil, fmt.Errorf("failed to unmarshal permissions: %w", err)
		}
	}

	group.AllowedAccounts = allowedAccounts

	if createdBy.Valid {
		group.CreatedBy = createdBy.String
	}

	return &group, nil
}
