//go:build integration

package auth

// Issue #313: users.salt is a leftover from before bcrypt and nothing needs
// it. This test drops the column from a fully migrated database and drives
// every store statement that used to read or write it. If any of them still
// names the column, Postgres fails it with `column "salt" does not exist`,
// so a pass shows the code no longer depends on the column and a later
// migration can drop it without breaking this code.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegration_UserStoreWorksWithoutSaltColumn(t *testing.T) {
	db := setupAuthTestDB(t)
	ctx := t.Context()
	_, err := db.Exec(ctx, "ALTER TABLE users DROP COLUMN salt")
	require.NoError(t, err)
	store := NewPostgresStore(db)

	// CreateAdminIfNone: the conditional bootstrap INSERT.
	created, err := store.CreateAdminIfNone(ctx, newBootstrapCandidate("no-salt-admin@example.com"))
	require.NoError(t, err)
	require.True(t, created)

	// CreateUser, then every SELECT that goes through scanUser.
	user := &User{Email: "no-salt@example.com", PasswordHash: "hash-1", Active: true,
		GroupIDs: []string{DefaultPurchaserGroupID}}
	require.NoError(t, store.CreateUser(ctx, user))

	byID, err := store.GetUserByID(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, user.Email, byID.Email)

	byEmail, err := store.GetUserByEmail(ctx, user.Email)
	require.NoError(t, err)
	assert.Equal(t, user.ID, byEmail.ID)

	users, err := store.ListUsers(ctx)
	require.NoError(t, err)
	assert.Len(t, users, 2)

	// UpdateUserCredentials: the compare-and-set credential write.
	changed := *byID
	changed.PasswordHash = "hash-2"
	changed.PasswordHistory = []string{"hash-1"}
	require.NoError(t, store.UpdateUserCredentials(ctx, &changed, byID.Email, byID.PasswordHash))

	// SetPasswordResetToken, GetUserByResetToken, CompletePasswordReset.
	expiry := time.Now().Add(time.Hour)
	withToken := changed
	withToken.PasswordResetToken = "reset-token-hash-313"
	withToken.PasswordResetExpiry = &expiry
	require.NoError(t, store.SetPasswordResetToken(ctx, &withToken, nil))

	byToken, err := store.GetUserByResetToken(ctx, withToken.PasswordResetToken)
	require.NoError(t, err)
	assert.Equal(t, user.ID, byToken.ID)

	reset := *byToken
	reset.PasswordHash = "hash-3"
	reset.PasswordHistory = []string{"hash-2", "hash-1"}
	require.NoError(t, store.CompletePasswordReset(ctx, &reset, byToken.PasswordResetToken, byToken.PasswordHash))

	final, err := store.GetUserByID(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "hash-3", final.PasswordHash)
	assert.Empty(t, final.PasswordResetToken)
}
