//go:build integration
// +build integration

package config

// Real-DB test for registration uniqueness (issue #137). The constraint names
// the store matches on come from migration 000017, so they are checked against
// the errors Postgres actually raises rather than hand-built ones.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

func newPendingRegistration(token, externalID string) *AccountRegistration {
	return &AccountRegistration{
		ReferenceToken: token, Status: "pending", Provider: "aws", ExternalID: externalID,
		AccountName: "Prod", ContactEmail: "owner@example.com",
	}
}

func TestCreateAccountRegistration_UniquenessPostgres(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath())
	store := NewPostgresStore(container.DB)

	first := newPendingRegistration("tok-137-a", "111111111111")
	require.NoError(t, store.CreateAccountRegistration(ctx, first))

	// Same account submitted twice while the first is pending.
	err := store.CreateAccountRegistration(ctx, newPendingRegistration("tok-137-b", "111111111111"))
	require.ErrorIs(t, err, ErrDuplicatePendingRegistration)
	assert.NotErrorIs(t, err, ErrReferenceTokenCollision)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, pgUniqueViolation, pgErr.Code)
	assert.Equal(t, registrationPendingUniqueIndex, pgErr.ConstraintName)

	// A different account reusing an existing token is a token collision,
	// not a duplicate registration.
	err = store.CreateAccountRegistration(ctx, newPendingRegistration("tok-137-a", "222222222222"))
	require.ErrorIs(t, err, ErrReferenceTokenCollision)
	assert.NotErrorIs(t, err, ErrDuplicatePendingRegistration)
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, registrationReferenceTokenConstraint, pgErr.ConstraintName)

	// A unique violation on any other constraint (here the primary key) is
	// neither sentinel.
	dupID := newPendingRegistration("tok-137-c", "333333333333")
	dupID.ID = first.ID
	err = store.CreateAccountRegistration(ctx, dupID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrDuplicatePendingRegistration)
	assert.NotErrorIs(t, err, ErrReferenceTokenCollision)

	// Once the first registration leaves pending, the account may register again.
	_, err = container.DB.Pool().Exec(ctx,
		`UPDATE account_registrations SET status = 'rejected' WHERE id = $1`, first.ID)
	require.NoError(t, err)
	require.NoError(t, store.CreateAccountRegistration(ctx, newPendingRegistration("tok-137-d", "111111111111")))
}
