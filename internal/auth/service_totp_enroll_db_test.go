//go:build integration

package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #442: the code that turns MFA on is spent like any other TOTP code.
func TestIntegration_EnrollmentCodeIsSpent(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()
	step := func() int64 { return time.Now().Unix() / 30 }
	login := func(f *credentialRaceFixture, svc *Service, code string) error {
		_, err := svc.Login(ctx, LoginRequest{Email: f.user.Email, Password: credentialRacePassword, MFACode: code})
		return err
	}
	lastCounter := func(f *credentialRaceFixture) int64 {
		var c int64
		require.NoError(t, store.db.QueryRow(ctx, `SELECT mfa_last_totp_counter FROM users WHERE id = $1`, f.user.ID).Scan(&c))
		return c
	}

	t.Run("enrollment-code-cannot-log-in", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "totp-enroll-login@example.com")
		setup, err := f.svc.MFASetup(ctx, f.user.ID, credentialRacePassword)
		require.NoError(t, err)
		s := step()
		code := generateTOTP(setup.Secret, s)

		_, err = f.svc.MFAEnable(ctx, f.user.ID, code)
		require.NoError(t, err)
		assert.Equal(t, s, lastCounter(f), "EnableMFA must record the enrollment step")
		require.ErrorIs(t, login(f, f.svc, code), ErrInvalidMFACode, "the enrollment code must not log in")
		require.NoError(t, login(f, f.svc, generateTOTP(setup.Secret, s+1)))
	})

	t.Run("re-enroll-in-same-step-after-disable", func(t *testing.T) {
		// The counter is replaced on enable, not raised: a high counter left by
		// the old secret must not refuse the new secret's current code.
		f := newCredentialRaceFixture(t, store, "totp-enroll-reenroll@example.com")
		f.enrollMFA()
		old := f.stored().MFASecret
		s := step()
		require.NoError(t, f.svc.MFADisable(ctx, f.user.ID, credentialRacePassword, generateTOTP(old, s+1)))
		require.Equal(t, s+1, lastCounter(f))

		setup, err := f.svc.MFASetup(ctx, f.user.ID, credentialRacePassword)
		require.NoError(t, err)
		_, err = f.svc.MFAEnable(ctx, f.user.ID, generateTOTP(setup.Secret, s))
		require.NoError(t, err)
		assert.Equal(t, s, lastCounter(f))
		assert.True(t, f.stored().MFAEnabled)
	})

	t.Run("concurrent-enables-with-one-code-then-login-replay", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "totp-enroll-race@example.com")
		setup, err := f.svc.MFASetup(ctx, f.user.ID, credentialRacePassword)
		require.NoError(t, err)
		code := generateTOTP(setup.Secret, step())

		const racers = 8
		var accepted atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range racers {
			svc := f.newService(store, f.mail)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := svc.MFAEnable(ctx, f.user.ID, code); err == nil {
					accepted.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		assert.Equal(t, int32(1), accepted.Load(), "one enrollment must win")
		require.ErrorIs(t, login(f, f.svc, code), ErrInvalidMFACode)
	})
}
