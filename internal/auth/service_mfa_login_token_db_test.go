//go:build integration

package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #134: the mfa_token login form on real Postgres. A correct password
// for an MFA user returns a short-lived, single-use token bound to that user;
// {email, mfa_token, mfa_code} then signs in without the password.

// mfaChallenge signs in with the password only and returns the issued token.
func (f *credentialRaceFixture) mfaChallenge() string {
	f.t.Helper()
	_, err := f.svc.Login(f.t.Context(), LoginRequest{Email: f.user.Email, Password: credentialRacePassword})
	var challenge *MFARequiredError
	require.ErrorAs(f.t, err, &challenge)
	require.NotEmpty(f.t, challenge.Token)
	require.WithinDuration(f.t, time.Now().Add(MFALoginTokenTTL), challenge.ExpiresAt, time.Minute)
	return challenge.Token
}

func (f *credentialRaceFixture) tokenLogin(svc *Service, email, token, code string) (*LoginResponse, error) {
	return svc.Login(f.t.Context(), LoginRequest{Email: email, MFAToken: token, MFACode: code})
}

func (f *credentialRaceFixture) countRows(query string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.store.db.QueryRow(f.t.Context(), query, args...).Scan(&n))
	return n
}

func (f *credentialRaceFixture) sessionCount() int {
	return f.countRows(`SELECT count(*) FROM sessions WHERE user_id = $1`, f.user.ID)
}

func (f *credentialRaceFixture) tokenRows() int {
	return f.countRows(`SELECT count(*) FROM mfa_login_tokens WHERE user_id = $1`, f.user.ID)
}

func TestIntegration_MFALoginToken(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))

	t.Run("happy-path-stores-hash-and-signs-in", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-token-happy@example.com")
		f.enrollMFA()
		token := f.mfaChallenge()

		assert.Equal(t, 1, f.countRows(`SELECT count(*) FROM mfa_login_tokens WHERE token_hash = $1`, hashSessionToken(token)))
		assert.Zero(t, f.countRows(`SELECT count(*) FROM mfa_login_tokens WHERE token_hash = $1`, token), "the raw token must not be stored")

		resp, err := f.tokenLogin(f.svc, f.user.Email, token, f.currentTOTP())
		require.NoError(t, err)
		_, err = f.svc.ValidateSession(t.Context(), resp.Token)
		require.NoError(t, err)
		assert.Zero(t, f.tokenRows(), "a used token is deleted")
	})

	t.Run("expired", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-token-expired@example.com")
		f.enrollMFA()
		token := f.mfaChallenge()
		_, err := store.db.Exec(t.Context(), `UPDATE mfa_login_tokens SET expires_at = NOW() - interval '1 second' WHERE token_hash = $1`, hashSessionToken(token))
		require.NoError(t, err)

		// Refused before any code is checked: a wrong code is not counted
		// and a right one is not spent.
		_, err = f.tokenLogin(f.svc, f.user.Email, token, wrongMFACode(t, f.stored().MFASecret))
		require.ErrorIs(t, err, ErrInvalidMFAToken)
		assert.Zero(t, f.stored().FailedLoginAttempts, "an expired token checks no code")
		_, err = f.tokenLogin(f.svc, f.user.Email, token, f.currentTOTP())
		require.ErrorIs(t, err, ErrInvalidMFAToken)
		assert.Zero(t, f.countRows(`SELECT attempts FROM mfa_login_tokens WHERE token_hash = $1`, hashSessionToken(token)),
			"an expired token spends no attempts")

		// The next password login sweeps it.
		f.mfaChallenge()
		assert.Zero(t, f.countRows(`SELECT count(*) FROM mfa_login_tokens WHERE token_hash = $1`, hashSessionToken(token)))
	})

	t.Run("reused", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-token-reused@example.com")
		recovery := f.enrollMFA()
		token := f.mfaChallenge()
		_, err := f.tokenLogin(f.svc, f.user.Email, token, f.currentTOTP())
		require.NoError(t, err)
		sessions := f.sessionCount()
		codesBefore := f.stored().MFARecoveryCodes

		// A second valid factor (a recovery code: the TOTP step is spent)
		// does not revive the token.
		_, err = f.tokenLogin(f.svc, f.user.Email, token, recovery[0])
		require.ErrorIs(t, err, ErrInvalidMFAToken)
		assert.Equal(t, sessions, f.sessionCount())
		assert.Equal(t, codesBefore, f.stored().MFARecoveryCodes, "a dead token must not burn a recovery code")
	})

	t.Run("wrong-user", func(t *testing.T) {
		a := newCredentialRaceFixture(t, store, "mfa-token-owner@example.com")
		a.enrollMFA()
		b := newCredentialRaceFixture(t, store, "mfa-token-other@example.com")
		b.enrollMFA()
		token := a.mfaChallenge()

		// A's token cannot sign in as B, even with B's valid code.
		_, err := a.tokenLogin(a.svc, b.user.Email, token, b.currentTOTP())
		require.ErrorIs(t, err, ErrInvalidMFAToken)
		// B's code is not A's factor.
		_, err = a.tokenLogin(a.svc, a.user.Email, token, b.currentTOTP())
		require.ErrorIs(t, err, ErrInvalidMFACode)
		// Nor is A's token B's, the other way round.
		bToken := b.mfaChallenge()
		_, err = b.tokenLogin(b.svc, a.user.Email, bToken, a.currentTOTP())
		require.ErrorIs(t, err, ErrInvalidMFAToken)
		assert.Equal(t, 1, a.sessionCount(), "no session beyond the fixture's")
		assert.Equal(t, 1, b.sessionCount(), "no session beyond the fixture's")
	})

	// The token's user is found by the token, not by the submitted email, so
	// a mismatched email gets the same answer whether it names no account, an
	// inactive one or another active one. So does the token's own user once
	// it is inactive or locked.
	t.Run("every-mismatch-same-answer", func(t *testing.T) {
		owner := newCredentialRaceFixture(t, store, "mfa-token-mismatch-owner@example.com")
		owner.enrollMFA()
		other := newCredentialRaceFixture(t, store, "mfa-token-mismatch-other@example.com")
		inactive := newCredentialRaceFixture(t, store, "mfa-token-mismatch-inactive@example.com")
		_, err := store.db.Exec(t.Context(), `UPDATE users SET active = false WHERE id = $1`, inactive.user.ID)
		require.NoError(t, err)

		refusal := func(email string) error {
			t.Helper()
			_, loginErr := owner.tokenLogin(owner.svc, email, owner.mfaChallenge(), owner.currentTOTP())
			require.Error(t, loginErr)
			return loginErr
		}
		answers := map[string]error{
			"non-existent email":  refusal("mfa-token-mismatch-nobody@example.com"),
			"inactive user email": refusal(inactive.user.Email),
			"other user email":    refusal(other.user.Email),
		}

		lockedToken, inactiveToken := owner.mfaChallenge(), owner.mfaChallenge()
		_, err = store.db.Exec(t.Context(), `UPDATE users SET locked_until = NOW() + interval '1 hour' WHERE id = $1`, owner.user.ID)
		require.NoError(t, err)
		_, answers["token user locked"] = owner.tokenLogin(owner.svc, owner.user.Email, lockedToken, owner.currentTOTP())
		_, err = store.db.Exec(t.Context(), `UPDATE users SET locked_until = NULL, active = false WHERE id = $1`, owner.user.ID)
		require.NoError(t, err)
		_, answers["token user inactive"] = owner.tokenLogin(owner.svc, owner.user.Email, inactiveToken, owner.currentTOTP())

		for name, got := range answers {
			assert.ErrorIs(t, got, ErrInvalidMFAToken, name)
		}
		assert.Zero(t, owner.stored().FailedLoginAttempts, "no code was checked")
		assert.Equal(t, 1, owner.sessionCount(), "no session beyond the fixture's")
	})

	t.Run("too-many-attempts", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-token-attempts@example.com")
		f.enrollMFA()
		secret := f.stored().MFASecret
		token := f.mfaChallenge()
		for i := 0; i < MaxMFALoginTokenAttempts; i++ {
			_, err := f.tokenLogin(f.svc, f.user.Email, token, wrongMFACode(t, secret))
			require.ErrorIs(t, err, ErrInvalidMFACode, "attempt %d", i+1)
		}
		_, err := f.tokenLogin(f.svc, f.user.Email, token, f.currentTOTP())
		require.ErrorIs(t, err, ErrInvalidMFAToken, "the token is spent after %d wrong codes", MaxMFALoginTokenAttempts)

		// The account is not locked yet (the cap sits below the lockout), so
		// starting again from the password still signs in.
		stored := f.stored()
		assert.Nil(t, stored.LockedUntil)
		assert.Equal(t, MaxMFALoginTokenAttempts, stored.FailedLoginAttempts, "wrong codes count toward the lockout")
		_, err = f.tokenLogin(f.svc, f.user.Email, f.mfaChallenge(), f.currentTOTP())
		require.NoError(t, err)
	})

	t.Run("password-changed-voids-token", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-token-pwchange@example.com")
		f.enrollMFA()
		token := f.mfaChallenge()
		_, err := store.db.Exec(t.Context(), `UPDATE users SET password_hash = $2 WHERE id = $1`, f.user.ID, f.stored().PasswordHash+"x")
		require.NoError(t, err)

		_, err = f.tokenLogin(f.svc, f.user.Email, token, f.currentTOTP())
		require.ErrorIs(t, err, ErrInvalidMFAToken)
	})

	t.Run("old-password-and-code-form-still-works", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-token-oldform@example.com")
		f.enrollMFA()
		token := f.mfaChallenge()
		resp, err := f.svc.Login(t.Context(), LoginRequest{Email: f.user.Email, Password: credentialRacePassword, MFACode: f.currentTOTP()})
		require.NoError(t, err)
		require.NotEmpty(t, resp.Token)
		assert.Equal(t, 1, f.countRows(`SELECT count(*) FROM mfa_login_tokens WHERE token_hash = $1`, hashSessionToken(token)),
			"the password form neither needs nor spends a token")

		// Without MFA the password alone still signs in and issues no token.
		plain := newCredentialRaceFixture(t, store, "mfa-token-nomfa@example.com")
		_, err = plain.svc.Login(t.Context(), LoginRequest{Email: plain.user.Email, Password: credentialRacePassword})
		require.NoError(t, err)
		assert.Zero(t, plain.tokenRows())
	})

	// Concurrent wrong guesses on one token cannot check more codes than the
	// cap: the attempt is claimed atomically before each check.
	t.Run("concurrent-guesses-capped", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-token-race-guess@example.com")
		f.enrollMFA()
		wrong := wrongMFACode(t, f.stored().MFASecret)
		token := f.mfaChallenge()
		const callers = 12
		errs := make([]error, callers)
		var start, wg sync.WaitGroup
		start.Add(1)
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start.Wait()
				_, errs[i] = f.svc.Login(context.Background(), LoginRequest{Email: f.user.Email, MFAToken: token, MFACode: wrong})
			}()
		}
		start.Done()
		wg.Wait()

		var checked, refused int
		for _, err := range errs {
			switch {
			case errors.Is(err, ErrInvalidMFACode):
				checked++
			case errors.Is(err, ErrInvalidMFAToken):
				refused++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		assert.Equal(t, MaxMFALoginTokenAttempts, checked)
		assert.Equal(t, callers-MaxMFALoginTokenAttempts, refused)
		assert.Equal(t, MaxMFALoginTokenAttempts, f.stored().FailedLoginAttempts)
	})

	// Two requests with two different valid factors that both pass the code
	// check before either consumes the token: only one gets a session.
	t.Run("concurrent-valid-codes-one-session", func(t *testing.T) {
		f := newCredentialRaceFixture(t, store, "mfa-token-race-valid@example.com")
		recovery := f.enrollMFA()
		token := f.mfaChallenge()
		barrier := &consumeBarrier{StoreInterface: store, parties: 2, arrived: make(chan struct{}, 2), release: make(chan struct{})}
		svc := f.newService(barrier, &recordingMailSink{})
		sessions := f.sessionCount()

		codes := []string{f.currentTOTP(), recovery[0]}
		errs := make([]error, len(codes))
		var wg sync.WaitGroup
		for i, code := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = f.tokenLogin(svc, f.user.Email, token, code)
			}()
		}
		barrier.waitAndRelease(t)
		wg.Wait()

		var ok, refused int
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrInvalidMFAToken):
				refused++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		assert.Equal(t, 1, ok, "exactly one request may complete the login")
		assert.Equal(t, 1, refused)
		assert.Equal(t, sessions+1, f.sessionCount())
	})
}

// consumeBarrier holds every ConsumeMFALoginToken call until parties callers
// have reached it, so the code checks of all of them finish first.
type consumeBarrier struct {
	StoreInterface
	arrived chan struct{}
	release chan struct{}
	parties int
}

func (b *consumeBarrier) ConsumeMFALoginToken(ctx context.Context, tokenHash string) (bool, error) {
	b.arrived <- struct{}{}
	<-b.release
	return b.StoreInterface.ConsumeMFALoginToken(ctx, tokenHash)
}

func (b *consumeBarrier) waitAndRelease(t *testing.T) {
	t.Helper()
	for i := 0; i < b.parties; i++ {
		select {
		case <-b.arrived:
		case <-time.After(30 * time.Second):
			close(b.release)
			t.Fatalf("only %d of %d callers reached the consume step", i, b.parties)
		}
	}
	close(b.release)
}
