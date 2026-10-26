//go:build integration
// +build integration

package config

// Real-DB test for GetAccountCredential's missing-row contract (#120): a
// missing credential is ErrNotFound, and only a stored empty blob reads back
// as ("", nil).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
)

func TestGetAccountCredential_MissingRowIsNotFoundPostgres(t *testing.T) {
	ctx := context.Background()
	container := testhelpers.RequireMigratedDatabase(ctx, t, getTestMigrationsPath())
	store := NewPostgresStore(container.DB)

	account := &CloudAccount{Name: "credential fixture", Provider: "aws", ExternalID: "111111111111", Enabled: true}
	require.NoError(t, store.CreateCloudAccount(ctx, account))

	blob, err := store.GetAccountCredential(ctx, account.ID, "aws_access_keys")
	require.ErrorIs(t, err, ErrNotFound, "no row for the account must be ErrNotFound")
	assert.Empty(t, blob)

	// A stored empty blob is a row: it reads back as ("", nil), which is
	// exactly why a missing row must not return the same pair.
	require.NoError(t, store.SaveAccountCredential(ctx, account.ID, "aws_role_arn", ""))
	blob, err = store.GetAccountCredential(ctx, account.ID, "aws_role_arn")
	require.NoError(t, err)
	assert.Empty(t, blob)

	// Another type's row does not satisfy the lookup.
	_, err = store.GetAccountCredential(ctx, account.ID, "aws_access_keys")
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, store.SaveAccountCredential(ctx, account.ID, "aws_access_keys", "ciphertext"))
	blob, err = store.GetAccountCredential(ctx, account.ID, "aws_access_keys")
	require.NoError(t, err)
	assert.Equal(t, "ciphertext", blob)

	require.NoError(t, store.DeleteAccountCredentials(ctx, account.ID))
	_, err = store.GetAccountCredential(ctx, account.ID, "aws_access_keys")
	require.ErrorIs(t, err, ErrNotFound, "a deleted credential must read back as ErrNotFound")
}
