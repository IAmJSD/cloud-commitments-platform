//go:build integration

package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type loginReadBarrierStore struct {
	StoreInterface
	afterRead func(context.Context, *User) error
}

func (s *loginReadBarrierStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	u, err := s.StoreInterface.GetUserByEmail(ctx, email)
	if err == nil {
		err = s.afterRead(ctx, u)
	}
	return u, err
}

// overwriteSecurityColumns stands in for every security change that can land
// while a login is in flight (credentials, MFA, reset token, password history,
// membership and active state), all in one statement. No store writer covers
// them all: the whole-row UpdateUser that did was removed in #583. It writes
// group_ids and active, so like the store's own writers it takes the
// admin-invariant lock first (see adminInvariantAdvisoryLockKey).
func overwriteSecurityColumns(ctx context.Context, store *PostgresStore, u *User) error {
	return store.withAdminInvariantLock(ctx, "overwrite security columns", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE users SET
				email = $2, password_hash = $3, group_ids = $4,
				active = $5, deactivated_at = $6, mfa_enabled = $7, mfa_secret = $8,
				mfa_pending_secret = $9, mfa_pending_secret_expires_at = $10,
				mfa_recovery_codes = $11, password_reset_token = $12,
				password_reset_expiry = $13, password_history = $14, updated_at = NOW()
			WHERE id = $1`,
			u.ID, u.Email, u.PasswordHash, u.GroupIDs,
			u.Active, u.DeactivatedAt, u.MFAEnabled, u.MFASecret,
			u.MFAPendingSecret, u.MFAPendingSecretExpiresAt,
			u.MFARecoveryCodes, u.PasswordResetToken,
			u.PasswordResetExpiry, u.PasswordHistory)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("overwrite security columns: %d rows for user %s", tag.RowsAffected(), u.ID)
		}
		return nil
	})
}

func TestIntegration_LoginPreservesSecurityChanges(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx := t.Context()
	for _, scenario := range []string{"wrong-password", "wrong-mfa", "password-success", "totp-success"} {
		t.Run(scenario, func(t *testing.T) {
			service := NewService(ServiceConfig{Store: store})
			service.bcryptCostOverride = 4
			hash, err := service.hashPassword("OriginalPassword123!")
			require.NoError(t, err)
			user := &User{Email: scenario + "@example.com", PasswordHash: hash, Active: true,
				GroupIDs: []string{DefaultPurchaserGroupID}, MFASecret: "JBSWY3DPEHPK3PXP",
				MFAEnabled: scenario == "wrong-mfa" || scenario == "totp-success"}
			require.NoError(t, store.CreateUser(ctx, user))
			var changed *User
			service.store = &loginReadBarrierStore{StoreInterface: store, afterRead: func(ctx context.Context, snapshot *User) error {
				current, readErr := store.GetUserByID(ctx, snapshot.ID)
				if readErr != nil {
					return readErr
				}
				stamp := time.Now().UTC().Truncate(time.Microsecond)
				current.PasswordHash = "new-password-hash"
				current.Email = "changed-" + current.Email
				current.MFAEnabled, current.MFASecret = !current.MFAEnabled, "changed-secret"
				current.MFAPendingSecret, current.MFAPendingSecretExpiresAt = "pending-secret", &stamp
				current.MFARecoveryCodes = []string{"new-code"}
				current.GroupIDs = []string{DefaultAdminGroupID}
				current.Active, current.DeactivatedAt = false, &stamp
				current.PasswordResetToken, current.PasswordResetExpiry = "new-reset-token", &stamp
				current.PasswordHistory = []string{"new-history"}
				if updateErr := overwriteSecurityColumns(ctx, store, current); updateErr != nil {
					return updateErr
				}
				changed, readErr = store.GetUserByID(ctx, current.ID)
				return readErr
			}}
			request := LoginRequest{Email: user.Email, Password: "OriginalPassword123!"}
			success := scenario == "password-success" || scenario == "totp-success"
			if scenario == "wrong-password" {
				request.Password = "incorrect"
			}
			if scenario == "wrong-mfa" {
				request.MFACode = "not-a-code"
			}
			if scenario == "totp-success" {
				request.MFACode = generateTOTP(user.MFASecret, time.Now().Unix()/30)
			}
			response, loginErr := service.Login(ctx, request)
			if success {
				require.NoError(t, loginErr)
				require.NotEmpty(t, response.Token)
			} else {
				require.Error(t, loginErr)
				require.Nil(t, response)
			}
			require.NotNil(t, changed)
			stored, err := store.GetUserByID(ctx, user.ID)
			require.NoError(t, err)
			if success {
				assert.Zero(t, stored.FailedLoginAttempts)
				require.NotNil(t, stored.LastLoginAt)
			} else {
				assert.Equal(t, 1, stored.FailedLoginAttempts)
				assert.Nil(t, stored.LastLoginAt)
			}
			stored.UpdatedAt, stored.LastLoginAt = changed.UpdatedAt, changed.LastLoginAt
			stored.FailedLoginAttempts, stored.LockedUntil = changed.FailedLoginAttempts, changed.LockedUntil
			assert.Equal(t, changed, stored, "login must preserve every non-bookkeeping column")
		})
	}
}

func TestIntegration_LoginConcurrentFailures(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	service := NewService(ServiceConfig{Store: store})
	service.bcryptCostOverride = 4
	hash, err := service.hashPassword("OriginalPassword123!")
	require.NoError(t, err)
	user := &User{Email: "parallel-login@example.com", PasswordHash: hash, Active: true, GroupIDs: []string{DefaultPurchaserGroupID}}
	require.NoError(t, store.CreateUser(ctx, user))
	arrived, release := make(chan struct{}, MaxFailedLoginAttempts), make(chan struct{})
	service.store = &loginReadBarrierStore{StoreInterface: store, afterRead: func(ctx context.Context, _ *User) error {
		arrived <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	results := make(chan error, MaxFailedLoginAttempts)
	before := time.Now()
	for range MaxFailedLoginAttempts {
		workers.Add(1)
		go func() {
			defer workers.Done()
			response, loginErr := service.Login(ctx, LoginRequest{Email: user.Email, Password: "incorrect"})
			if response != nil {
				results <- fmt.Errorf("failed login returned a session")
				return
			}
			results <- loginErr
		}()
	}
	for range MaxFailedLoginAttempts {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	for range MaxFailedLoginAttempts {
		require.EqualError(t, <-results, genericLoginError)
	}
	stored, err := store.GetUserByID(t.Context(), user.ID)
	require.NoError(t, err)
	assert.Equal(t, MaxFailedLoginAttempts, stored.FailedLoginAttempts)
	require.NotNil(t, stored.LockedUntil)
	assert.WithinRange(t, *stored.LockedUntil, before.Add(AccountLockoutDuration), time.Now().Add(AccountLockoutDuration))
	service.store = store
	response, err := service.Login(t.Context(), LoginRequest{Email: user.Email, Password: "OriginalPassword123!"})
	require.EqualError(t, err, genericLoginError)
	assert.Nil(t, response)
}

type loginWriteOrderStore struct {
	StoreInterface
	firstDone    chan error
	proceed      chan struct{}
	successFirst bool
}

func TestIntegration_LoginSequentialFailures(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	service := NewService(ServiceConfig{Store: store})
	service.bcryptCostOverride = 4
	hash, err := service.hashPassword("OriginalPassword123!")
	require.NoError(t, err)
	user := &User{Email: "sequential-login@example.com", PasswordHash: hash, Active: true, GroupIDs: []string{DefaultPurchaserGroupID}}
	require.NoError(t, store.CreateUser(t.Context(), user))
	for attempt := 1; attempt <= MaxFailedLoginAttempts; attempt++ {
		response, loginErr := service.Login(t.Context(), LoginRequest{Email: user.Email, Password: "incorrect"})
		require.EqualError(t, loginErr, genericLoginError)
		assert.Nil(t, response)
		stored, readErr := store.GetUserByID(t.Context(), user.ID)
		require.NoError(t, readErr)
		assert.Equal(t, attempt, stored.FailedLoginAttempts)
		assert.Equal(t, attempt == MaxFailedLoginAttempts, stored.LockedUntil != nil)
	}
}

func (s *loginWriteOrderStore) RecordFailedLogin(ctx context.Context, id string) error {
	if s.successFirst {
		select {
		case <-s.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := s.StoreInterface.RecordFailedLogin(ctx, id)
	if !s.successFirst {
		s.firstDone <- err
		select {
		case <-s.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *loginWriteOrderStore) RecordSuccessfulLogin(ctx context.Context, id string) error {
	if !s.successFirst {
		select {
		case <-s.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := s.StoreInterface.RecordSuccessfulLogin(ctx, id)
	if s.successFirst {
		s.firstDone <- err
		select {
		case <-s.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func TestIntegration_LoginMixedOrdering(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	for _, initial := range []int{0, MaxFailedLoginAttempts - 1} {
		for _, successFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("initial-%d-success-first-%v", initial, successFirst), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				var workers sync.WaitGroup
				defer func() { cancel(); workers.Wait() }()
				service := NewService(ServiceConfig{Store: store})
				service.bcryptCostOverride = 4
				hash, err := service.hashPassword("OriginalPassword123!")
				require.NoError(t, err)
				user := &User{Email: fmt.Sprintf("mixed-%d-%v@example.com", initial, successFirst), PasswordHash: hash, Active: true,
					GroupIDs: []string{DefaultPurchaserGroupID}, FailedLoginAttempts: initial}
				require.NoError(t, store.CreateUser(ctx, user))
				ordered := &loginWriteOrderStore{StoreInterface: store, successFirst: successFirst, firstDone: make(chan error, 1), proceed: make(chan struct{})}
				arrived, release := make(chan struct{}, 2), make(chan struct{})
				service.store = &loginReadBarrierStore{StoreInterface: ordered, afterRead: func(ctx context.Context, _ *User) error {
					arrived <- struct{}{}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}}
				type result struct {
					response *LoginResponse
					err      error
				}
				successResult, failureResult := make(chan result, 1), make(chan result, 1)
				workers.Add(2)
				go func() {
					defer workers.Done()
					r, e := service.Login(ctx, LoginRequest{Email: user.Email, Password: "OriginalPassword123!"})
					successResult <- result{r, e}
				}()
				go func() {
					defer workers.Done()
					r, e := service.Login(ctx, LoginRequest{Email: user.Email, Password: "wrong"})
					failureResult <- result{r, e}
				}()
				for range 2 {
					select {
					case <-arrived:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				close(release)
				select {
				case firstErr := <-ordered.firstDone:
					require.NoError(t, firstErr)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				intermediate, err := store.GetUserByID(ctx, user.ID)
				require.NoError(t, err)
				if successFirst {
					assert.Zero(t, intermediate.FailedLoginAttempts)
					assert.Nil(t, intermediate.LockedUntil)
				} else {
					assert.Equal(t, initial+1, intermediate.FailedLoginAttempts)
					assert.Equal(t, initial+1 >= MaxFailedLoginAttempts, intermediate.LockedUntil != nil)
				}
				close(ordered.proceed)
				success, failure := <-successResult, <-failureResult
				require.NoError(t, success.err)
				require.NotNil(t, success.response)
				_, err = service.ValidateSession(ctx, success.response.Token)
				require.NoError(t, err)
				require.EqualError(t, failure.err, genericLoginError)
				assert.Nil(t, failure.response)
				stored, err := store.GetUserByID(ctx, user.ID)
				require.NoError(t, err)
				expected := 0
				if successFirst {
					expected = 1
				}
				assert.Equal(t, expected, stored.FailedLoginAttempts)
				assert.Nil(t, stored.LockedUntil)
				require.NotNil(t, stored.LastLoginAt)
			})
		}
	}
}

// dbClock reads the database's own clock. last_login_at and locked_until are
// stamped with NOW() on the server, so bounds taken from the test process's
// clock compare two clocks and fail when they disagree by a few milliseconds
// (issue #568 (f)); bounds read here share the clock and precision of the
// stamped value.
func dbClock(t *testing.T, store *PostgresStore) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, store.db.QueryRow(t.Context(), "SELECT clock_timestamp()").Scan(&now))
	return now
}

func TestIntegration_LoginLockoutExpiry(t *testing.T) {
	store := NewPostgresStore(setupAuthTestDB(t))
	service := NewService(ServiceConfig{Store: store})
	service.bcryptCostOverride = 4
	hash, err := service.hashPassword("OriginalPassword123!")
	require.NoError(t, err)
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprintf("success-%v", success), func(t *testing.T) {
			expired := time.Now().Add(-time.Minute)
			user := &User{Email: fmt.Sprintf("expired-%v@example.com", success), PasswordHash: hash, Active: true,
				GroupIDs: []string{DefaultPurchaserGroupID}, FailedLoginAttempts: MaxFailedLoginAttempts, LockedUntil: &expired}
			require.NoError(t, store.CreateUser(t.Context(), user))
			password := "wrong"
			if success {
				password = "OriginalPassword123!"
			}
			before := dbClock(t, store)
			response, loginErr := service.Login(t.Context(), LoginRequest{Email: user.Email, Password: password})
			after := dbClock(t, store)
			stored, err := store.GetUserByID(t.Context(), user.ID)
			require.NoError(t, err)
			if success {
				require.NoError(t, loginErr)
				require.NotNil(t, response)
				assert.Zero(t, stored.FailedLoginAttempts)
				assert.Nil(t, stored.LockedUntil)
				require.NotNil(t, stored.LastLoginAt)
				assert.WithinRange(t, *stored.LastLoginAt, before, after)
			} else {
				require.EqualError(t, loginErr, genericLoginError)
				assert.Nil(t, response)
				assert.Equal(t, MaxFailedLoginAttempts+1, stored.FailedLoginAttempts)
				require.NotNil(t, stored.LockedUntil)
				assert.WithinRange(t, *stored.LockedUntil, before.Add(AccountLockoutDuration), after.Add(AccountLockoutDuration))
			}
		})
	}
}
