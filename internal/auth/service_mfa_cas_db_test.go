//go:build integration

package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func totpNow(secret string) string { return generateTOTP(secret, time.Now().Unix()/30) }

// Issue #527: disable and recovery-code regeneration write only the MFA
// columns, guarded by the secret and codes they read.
func TestIntegration_MFADisableAndRegenerateAreColumnScoped(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()

	spendRecoveryCode := func(f *credentialRaceFixture, code string) func(context.Context, *Service) {
		return func(ctx context.Context, other *Service) {
			_, err := other.Login(ctx, LoginRequest{Email: f.user.Email, Password: credentialRacePassword, MFACode: code})
			require.NoError(t, err)
		}
	}

	t.Run("disable-and-regenerate-work-when-uncontended", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-cas-plain@example.com")
		codes := f.enrollMFA()
		before := f.stored()

		fresh, err := f.svc.MFARegenerateRecoveryCodes(ctx, f.user.ID, totpNow(before.MFASecret))
		require.NoError(t, err)
		after := f.stored()
		assert.Len(t, after.MFARecoveryCodes, len(fresh))
		assert.NotEqual(t, before.MFARecoveryCodes, after.MFARecoveryCodes)
		assertOnlyChanged(t, before, after, func(w *User) { w.MFARecoveryCodes = after.MFARecoveryCodes })

		_, err = f.svc.Login(ctx, LoginRequest{Email: f.user.Email, Password: credentialRacePassword, MFACode: codes[0]})
		require.ErrorIs(t, err, ErrInvalidMFACode, "pre-regeneration codes must stop working")
		_, err = f.svc.Login(ctx, LoginRequest{Email: f.user.Email, Password: credentialRacePassword, MFACode: fresh[0]})
		require.NoError(t, err)

		require.NoError(t, f.svc.MFADisable(ctx, f.user.ID, credentialRacePassword, fresh[1]))
		off := f.stored()
		assert.False(t, off.MFAEnabled)
		assert.Empty(t, off.MFASecret)
		assert.Empty(t, off.MFARecoveryCodes)
		_, err = f.svc.Login(ctx, LoginRequest{Email: f.user.Email, Password: credentialRacePassword})
		require.NoError(t, err)
	})

	t.Run("stale-regenerate-after-concurrent-recovery-login", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-cas-regen-spend@example.com")
		codes := f.enrollMFA()
		secret := f.stored().MFASecret
		winner := f.onRead(spendRecoveryCode(f, codes[0]))
		fresh, err := f.svc.MFARegenerateRecoveryCodes(ctx, f.user.ID, totpNow(secret))
		require.ErrorIs(t, err, ErrUserChanged)
		assert.Nil(t, fresh)
		assertOnlyChanged(t, winner(), f.stored(), func(*User) {})
		assert.Equal(t, winner().MFARecoveryCodes, f.stored().MFARecoveryCodes, "the stale write must not replace the codes the login left")
		assert.Len(t, f.stored().MFARecoveryCodes, len(codes)-1)
	})

	t.Run("stale-disable-after-concurrent-recovery-login", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-cas-disable-spend@example.com")
		codes := f.enrollMFA()
		secret := f.stored().MFASecret
		winner := f.onRead(spendRecoveryCode(f, codes[0]))
		err := f.svc.MFADisable(ctx, f.user.ID, credentialRacePassword, totpNow(secret))
		require.ErrorIs(t, err, ErrUserChanged)
		assertOnlyChanged(t, winner(), f.stored(), func(*User) {})
		assert.True(t, f.stored().MFAEnabled)
	})

	t.Run("stale-disable-with-recovery-code-after-concurrent-spend-of-same-code", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-cas-disable-same@example.com")
		codes := f.enrollMFA()
		enrolled := f.stored()
		winner := f.onRead(spendRecoveryCode(f, codes[0]))
		err := f.svc.MFADisable(ctx, f.user.ID, credentialRacePassword, codes[0])
		require.ErrorIs(t, err, ErrUserChanged, "a recovery code must be spent once")
		assert.Equal(t, winner(), f.stored())
		f.requireFactorEnforced(f.user.Email, credentialRacePassword, &User{
			MFASecret: enrolled.MFASecret, MFARecoveryCodes: winner().MFARecoveryCodes,
		})
	})

	t.Run("stale-regenerate-after-disable-with-zero-recovery-codes", func(t *testing.T) {
		// Issue #568 (a): with every recovery code spent, the disabled row's
		// codes ('{}') still match the stale read, so only the mfa_enabled and
		// mfa_secret guards refuse the write. Spending the codes is stood in
		// for by emptying the column.
		f := newCredentialRaceFixture(t, store, "mfa-cas-regen-nocodes@example.com")
		f.enrollMFA()
		_, err := store.db.Exec(ctx, "UPDATE users SET mfa_recovery_codes = '{}' WHERE id = $1", f.user.ID)
		require.NoError(t, err)
		secret := f.stored().MFASecret
		step := time.Now().Unix() / 30
		winner := f.onRead(func(ctx context.Context, other *Service) {
			require.NoError(t, other.MFADisable(ctx, f.user.ID, credentialRacePassword, generateTOTP(secret, step)))
		})
		fresh, err := f.svc.MFARegenerateRecoveryCodes(ctx, f.user.ID, generateTOTP(secret, step+1))
		require.ErrorIs(t, err, ErrUserChanged)
		assert.Nil(t, fresh)
		stored := f.stored()
		assert.False(t, stored.MFAEnabled)
		assert.Empty(t, stored.MFASecret)
		assert.Empty(t, stored.MFARecoveryCodes, "the stale regenerate must not give a disabled account recovery codes")
		assertOnlyChanged(t, winner(), stored, func(*User) {})
	})

	for name, act := range map[string]func(f *credentialRaceFixture) error{
		"disable": func(f *credentialRaceFixture) error {
			return f.svc.MFADisable(ctx, f.user.ID, credentialRacePassword, totpNow(f.stored().MFASecret))
		},
		"regenerate": func(f *credentialRaceFixture) error {
			_, err := f.svc.MFARegenerateRecoveryCodes(ctx, f.user.ID, totpNow(f.stored().MFASecret))
			return err
		},
	} {
		t.Run("concurrent-password-change-is-kept-by-"+name, func(t *testing.T) {
			f := newCredentialRaceFixture(t, store, "mfa-cas-pw-"+name+"@example.com")
			f.enrollMFA()
			enrolled := f.stored()
			winner := f.onRead(func(ctx context.Context, other *Service) {
				require.NoError(t, other.ChangePassword(ctx, f.user.ID, ChangePasswordRequest{
					CurrentPassword: credentialRacePassword, NewPassword: credentialRaceNew,
				}))
			})
			require.NoError(t, act(f))
			stored := f.stored()
			assert.Equal(t, winner().PasswordHash, stored.PasswordHash, "the stale write reverted the password change")
			assert.Equal(t, winner().PasswordVersion, stored.PasswordVersion)
			assert.Equal(t, winner().PasswordHistory, stored.PasswordHistory)
			assert.True(t, f.svc.verifyPassword(credentialRaceNew, stored.PasswordHash))
			assert.NotEqual(t, enrolled.MFARecoveryCodes, stored.MFARecoveryCodes, "the MFA write itself must still land")
		})
	}
}

// Issue #527: the pending-secret expiry is enforced by the write itself, not
// only on the value read before it. Issue #568 (d): the caller is told the
// enrollment expired, not that the account changed.
func TestIntegration_MFAEnableExpiryEnforcedInWrite(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()
	f := newCredentialRaceFixture(t, store, "mfa-cas-expiry@example.com")

	setup, err := f.svc.MFASetup(ctx, f.user.ID, credentialRacePassword)
	require.NoError(t, err)
	f.onRead(func(ctx context.Context, _ *Service) {
		_, execErr := store.db.Exec(ctx, "UPDATE users SET mfa_pending_secret_expires_at = NOW() - interval '1 minute' WHERE id = $1", f.user.ID)
		require.NoError(t, execErr)
	})
	codes, err := f.svc.MFAEnable(ctx, f.user.ID, totpNow(setup.Secret))
	require.ErrorIs(t, err, ErrMFAEnrollmentExpired)
	assert.NotErrorIs(t, err, ErrUserChanged)
	assert.Nil(t, codes)
	stored := f.stored()
	assert.False(t, stored.MFAEnabled, "an expired enrollment must not be promoted")
	assert.Empty(t, stored.MFASecret)
	assert.Equal(t, setup.Secret, stored.MFAPendingSecret, "a refused enable must not clear the pending enrollment")
}

// mfaColumns is a users row's MFA state, written directly so a test can put
// the row in exactly the state one guard is meant to refuse. expiresIn is a
// Postgres interval added to NOW(); nil leaves the pending expiry NULL.
type mfaColumns struct {
	enabled   bool
	secret    string
	codes     []string
	pending   string
	expiresIn *string
}

func pgInterval(s string) *string { return &s }

func newMFAColumnsUser(t *testing.T, store *PostgresStore, email string, row mfaColumns) *User {
	t.Helper()
	u := &User{Email: email, PasswordHash: "unused-hash", Active: true, GroupIDs: []string{DefaultPurchaserGroupID}}
	require.NoError(t, store.CreateUser(t.Context(), u))
	if row.codes == nil {
		row.codes = []string{} // the column is NOT NULL DEFAULT '{}'
	}
	_, err := store.db.Exec(t.Context(), `
		UPDATE users SET mfa_enabled = $2, mfa_secret = $3, mfa_recovery_codes = $4,
			mfa_pending_secret = $5, mfa_pending_secret_expires_at = NOW() + $6::interval
		WHERE id = $1
	`, u.ID, row.enabled, row.secret, row.codes, row.pending, row.expiresIn)
	require.NoError(t, err)
	stored, err := store.GetUserByID(t.Context(), u.ID)
	require.NoError(t, err)
	return stored
}

// Issue #568 (a), (e): each conjunct of the DisableMFA and
// ReplaceMFARecoveryCodes guards refuses a stale write on its own (the other
// two still match the read), and DisableMFA clears the pending enrollment too.
func TestIntegration_MFAWriteGuardsArePinnedIndividually(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()
	const readSecret = "READ-SECRET"
	readCodes := []string{"read-code-1", "read-code-2"}
	writes := map[string]func(userID string) error{
		"disable": func(id string) error { return store.DisableMFA(ctx, id, readSecret, readCodes) },
		"regenerate": func(id string) error {
			return store.ReplaceMFARecoveryCodes(ctx, id, readSecret, readCodes, []string{"new-code"})
		},
	}
	stale := map[string]mfaColumns{
		"mfa-enabled-false":          {enabled: false, secret: readSecret, codes: readCodes},
		"mfa-secret-replaced":        {enabled: true, secret: "OTHER-SECRET", codes: readCodes},
		"mfa-recovery-codes-changed": {enabled: true, secret: readSecret, codes: readCodes[1:]},
	}
	for write, act := range writes {
		for guard, row := range stale {
			t.Run(write+"-refused-on-"+guard, func(t *testing.T) {
				before := newMFAColumnsUser(t, store, "guard-"+write+"-"+guard+"@example.com", row)
				require.ErrorIs(t, act(before.ID), ErrUserChanged)
				after, err := store.GetUserByID(ctx, before.ID)
				require.NoError(t, err)
				assert.Equal(t, before, after, "a refused write must leave the row untouched")
			})
		}
	}

	matching := mfaColumns{enabled: true, secret: readSecret, codes: readCodes, pending: "PENDING-SECRET", expiresIn: pgInterval("10 minutes")}
	t.Run("disable-clears-every-mfa-column", func(t *testing.T) {
		before := newMFAColumnsUser(t, store, "guard-disable-match@example.com", matching)
		require.NotNil(t, before.MFAPendingSecretExpiresAt)
		require.NoError(t, writes["disable"](before.ID))
		after, err := store.GetUserByID(ctx, before.ID)
		require.NoError(t, err)
		assert.Empty(t, after.MFAPendingSecret, "DisableMFA must clear mfa_pending_secret")
		assert.Nil(t, after.MFAPendingSecretExpiresAt, "DisableMFA must clear mfa_pending_secret_expires_at")
		assertOnlyChanged(t, before, after, func(w *User) {
			w.MFAEnabled, w.MFASecret, w.MFARecoveryCodes = false, "", []string{}
			w.MFAPendingSecret, w.MFAPendingSecretExpiresAt = "", nil
		})
	})
	t.Run("regenerate-replaces-only-the-codes", func(t *testing.T) {
		before := newMFAColumnsUser(t, store, "guard-regenerate-match@example.com", matching)
		require.NoError(t, writes["regenerate"](before.ID))
		after, err := store.GetUserByID(ctx, before.ID)
		require.NoError(t, err)
		assertOnlyChanged(t, before, after, func(w *User) { w.MFARecoveryCodes = []string{"new-code"} })
	})
}

// Issue #568 (d): EnableMFA reports ErrMFAEnrollmentExpired only when the
// write was refused by the expiry check, and ErrUserChanged for every other
// refusal, including the losers of concurrent enables of one enrollment.
func TestIntegration_EnableMFARefusalReason(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()
	const pending = "PENDING-SECRET"
	enable := func(id string) error { return store.EnableMFA(ctx, id, pending, 1, []string{"new-code"}) }

	refusals := map[string]struct {
		row  mfaColumns
		want error
	}{
		"expired":                   {mfaColumns{pending: pending, expiresIn: pgInterval("-1 minute")}, ErrMFAEnrollmentExpired},
		"replaced-by-expired-setup": {mfaColumns{pending: "OTHER-PENDING", expiresIn: pgInterval("-1 minute")}, ErrUserChanged},
		"replaced-by-live-setup":    {mfaColumns{pending: "OTHER-PENDING", expiresIn: pgInterval("10 minutes")}, ErrUserChanged},
		"enrollment-cleared":        {mfaColumns{}, ErrUserChanged},
		"no-expiry":                 {mfaColumns{pending: pending}, ErrUserChanged},
		"mfa-on-with-expired-pending": {mfaColumns{enabled: true, secret: "ACTIVE-SECRET", codes: []string{"c"},
			pending: pending, expiresIn: pgInterval("-1 minute")}, ErrUserChanged},
	}
	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			before := newMFAColumnsUser(t, store, "enable-refusal-"+name+"@example.com", tc.row)
			err := enable(before.ID)
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, errors.Is(tc.want, ErrMFAEnrollmentExpired), errors.Is(err, ErrMFAEnrollmentExpired))
			assert.Equal(t, errors.Is(tc.want, ErrUserChanged), errors.Is(err, ErrUserChanged))
			after, getErr := store.GetUserByID(ctx, before.ID)
			require.NoError(t, getErr)
			assert.Equal(t, before, after, "a refused enable must leave the row untouched")
		})
	}

	t.Run("concurrent-enables-of-one-live-enrollment", func(t *testing.T) {
		const racers, rounds = 8, 5
		for round := range rounds {
			u := newMFAColumnsUser(t, store, fmt.Sprintf("enable-race-%d@example.com", round),
				mfaColumns{pending: pending, expiresIn: pgInterval("10 minutes")})
			errs := make([]error, racers)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range racers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					errs[i] = enable(u.ID)
				}()
			}
			close(start)
			wg.Wait()
			wins := 0
			for i, err := range errs {
				if err == nil {
					wins++
					continue
				}
				require.ErrorIs(t, err, ErrUserChanged, "round %d racer %d", round, i)
				require.NotErrorIs(t, err, ErrMFAEnrollmentExpired, "round %d racer %d", round, i)
			}
			require.Equal(t, 1, wins, "round %d: exactly one enable must win", round)
			stored, err := store.GetUserByID(ctx, u.ID)
			require.NoError(t, err)
			assert.True(t, stored.MFAEnabled)
			assert.Equal(t, pending, stored.MFASecret)
		}
	})
}
