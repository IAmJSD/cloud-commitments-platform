package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// ----- resolveAdminPassword -----

func TestResolveAdminPassword_MissingEnvVar(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "")

	app := &Application{}
	_, err := app.resolveAdminPassword(context.Background())
	testutil.AssertError(t, err)
	testutil.AssertContains(t, err.Error(), "ADMIN_PASSWORD_SECRET")
}

func TestResolveAdminPassword_NilResolver(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "arn:aws:secretsmanager:us-east-1:123456789012:secret:admin-pass")

	app := &Application{secretResolver: nil}
	_, err := app.resolveAdminPassword(context.Background())
	testutil.AssertError(t, err)
	testutil.AssertContains(t, err.Error(), "secret resolver is not configured")
}

func TestResolveAdminPassword_ResolverError(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "arn:aws:secretsmanager:us-east-1:123456789012:secret:admin-pass")

	app := &Application{secretResolver: &mockSecretResolver{
		getErr: errors.New("access denied"),
	}}
	_, err := app.resolveAdminPassword(context.Background())
	testutil.AssertError(t, err)
	testutil.AssertContains(t, err.Error(), "failed to resolve admin password secret")
}

func TestResolveAdminPassword_Success(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "arn:aws:secretsmanager:us-east-1:123456789012:secret:admin-pass")

	app := &Application{secretResolver: &mockSecretResolver{
		getResult: "supersecret",
	}}
	password, err := app.resolveAdminPassword(context.Background())
	testutil.AssertNoError(t, err)
	testutil.AssertEqual(t, "supersecret", password)
}

// ----- buildAdminPasswordSyncCallback -----

func TestBuildAdminPasswordSyncCallback_MissingSecretEnv(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "")
	testutil.SetEnv(t, "ADMIN_EMAIL", "admin@example.com")

	cb := buildAdminPasswordSyncCallback(&mockAuthStoreForHealth{}, &mockSecretResolver{})
	testutil.AssertTrue(t, cb == nil, "callback should be nil when secret env is unset")
}

func TestBuildAdminPasswordSyncCallback_MissingAdminEmail(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "arn:aws:secret")
	testutil.SetEnv(t, "ADMIN_EMAIL", "")

	cb := buildAdminPasswordSyncCallback(&mockAuthStoreForHealth{}, &mockSecretResolver{})
	testutil.AssertTrue(t, cb == nil, "callback should be nil when admin email is unset")
}

func TestBuildAdminPasswordSyncCallback_NilResolver(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "arn:aws:secret")
	testutil.SetEnv(t, "ADMIN_EMAIL", "admin@example.com")

	cb := buildAdminPasswordSyncCallback(&mockAuthStoreForHealth{}, nil)
	testutil.AssertTrue(t, cb == nil, "callback should be nil when resolver is nil")
}

func TestBuildAdminPasswordSyncCallback_UserNotFound(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "arn:aws:secret")
	testutil.SetEnv(t, "ADMIN_EMAIL", "admin@example.com")

	// GetUserByID returns nil, nil — user not found
	store := &mockAuthStoreForHealth{}
	resolver := &mockSecretResolver{}

	cb := buildAdminPasswordSyncCallback(store, resolver)
	testutil.AssertTrue(t, cb != nil, "callback should not be nil")

	// Calling the callback should not panic when user is not found
	cb(context.Background(), "user-123", "newpass")
	testutil.AssertEqual(t, "", resolver.putKey) // PutSecret should NOT be called
}

func TestBuildAdminPasswordSyncCallback_WrongEmail(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "arn:aws:secret")
	testutil.SetEnv(t, "ADMIN_EMAIL", "admin@example.com")

	store := &mockAuthStoreWithUser{
		user: &auth.User{ID: "user-123", Email: "other@example.com"},
	}
	resolver := &mockSecretResolver{}

	cb := buildAdminPasswordSyncCallback(store, resolver)
	cb(context.Background(), "user-123", "newpass")
	testutil.AssertEqual(t, "", resolver.putKey) // PutSecret should NOT be called (wrong email)
}

func TestBuildAdminPasswordSyncCallback_AdminEmailMatches(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "arn:aws:secret")
	testutil.SetEnv(t, "ADMIN_EMAIL", "admin@example.com")

	store := &mockAuthStoreWithUser{
		user: &auth.User{ID: "user-123", Email: "admin@example.com"},
	}
	resolver := &mockSecretResolver{}

	cb := buildAdminPasswordSyncCallback(store, resolver)
	cb(context.Background(), "user-123", "newpass")
	testutil.AssertEqual(t, "arn:aws:secret", resolver.putKey)
	testutil.AssertEqual(t, "newpass", resolver.putValue)
}

// #199: a failed write (for example a 403 because the runtime identity has
// read-only access to the vault) leaves the secret manager copy stale while
// the database already holds the new password. That must be an ERROR naming
// the secret, not a WARN that reads as harmless.
func TestBuildAdminPasswordSyncCallback_PutSecretFailureLogsError(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "admin-password")
	testutil.SetEnv(t, "ADMIN_EMAIL", "admin@example.com")
	logs := captureLogs(t)

	store := &mockAuthStoreWithUser{
		user: &auth.User{ID: "user-123", Email: "admin@example.com"},
	}
	resolver := &mockSecretResolver{putErr: errors.New("403 Forbidden: setSecret not permitted")}

	cb := buildAdminPasswordSyncCallback(store, resolver)
	cb(context.Background(), "user-123", "newpass")

	testutil.AssertEqual(t, "admin-password", resolver.putKey)
	errs := errorLines(logs.String())
	testutil.AssertEqual(t, 1, len(errs))
	testutil.AssertContains(t, errs[0], `"admin-password"`)
	testutil.AssertContains(t, errs[0], "403 Forbidden")
	testutil.AssertContains(t, errs[0], "no longer matches the database password")
	testutil.AssertFalse(t, strings.Contains(logs.String(), "newpass"), "the password must never be logged")
	testutil.AssertFalse(t, strings.Contains(logs.String(), "[WARN]"), "the failure must not be downgraded to WARN")
	testutil.AssertFalse(t, strings.Contains(logs.String(), "synced to secret manager"), "a failed sync must not also report success")
}

// #199: when the user lookup fails the callback cannot tell whether the admin
// changed their password, so it must say the copy may be stale instead of
// returning silently.
func TestBuildAdminPasswordSyncCallback_UserLookupFailureLogsError(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "admin-password")
	testutil.SetEnv(t, "ADMIN_EMAIL", "admin@example.com")
	logs := captureLogs(t)

	store := &mockAuthStoreWithUser{err: errors.New("connection reset")}
	resolver := &mockSecretResolver{}

	cb := buildAdminPasswordSyncCallback(store, resolver)
	cb(context.Background(), "user-123", "newpass")

	testutil.AssertEqual(t, "", resolver.putKey)
	errs := errorLines(logs.String())
	testutil.AssertEqual(t, 1, len(errs))
	testutil.AssertContains(t, errs[0], "user-123")
	testutil.AssertContains(t, errs[0], "connection reset")
	testutil.AssertContains(t, errs[0], `"admin-password"`)
	testutil.AssertFalse(t, strings.Contains(logs.String(), "newpass"), "the password must never be logged")
}

// The quiet paths stay quiet: a successful sync and a non-admin user log no
// ERROR, so the new ERROR lines keep meaning "the copies diverged".
func TestBuildAdminPasswordSyncCallback_QuietPathsLogNoError(t *testing.T) {
	testutil.SetEnv(t, "ADMIN_PASSWORD_SECRET", "admin-password")
	testutil.SetEnv(t, "ADMIN_EMAIL", "admin@example.com")
	logs := captureLogs(t)

	admin := &mockAuthStoreWithUser{user: &auth.User{ID: "user-123", Email: "admin@example.com"}}
	other := &mockAuthStoreWithUser{user: &auth.User{ID: "user-456", Email: "other@example.com"}}

	buildAdminPasswordSyncCallback(admin, &mockSecretResolver{})(context.Background(), "user-123", "newpass")
	buildAdminPasswordSyncCallback(other, &mockSecretResolver{})(context.Background(), "user-456", "newpass")
	buildAdminPasswordSyncCallback(&mockAuthStoreForHealth{}, &mockSecretResolver{})(context.Background(), "user-789", "newpass")

	testutil.AssertEqual(t, 0, len(errorLines(logs.String())))
	testutil.AssertContains(t, logs.String(), "Admin password synced to secret manager")
}

// ----- Close with nil DB (already covered in app_test.go; duplicate here for clarity) -----

func TestClose_NilDB_NoPanic(t *testing.T) {
	app := &Application{DB: nil}
	err := app.Close()
	testutil.AssertNoError(t, err)
}

// ----- ensureDB with pre-set dbErr -----

func TestEnsureDB_PresetError(t *testing.T) {
	app := &Application{
		dbConfig: &database.Config{Host: "unreachable"},
		dbErr:    errors.New("pre-set connection error"),
	}
	err := app.ensureDB(context.Background())
	testutil.AssertError(t, err)
	testutil.AssertContains(t, err.Error(), "pre-set connection error")
}

// ----- mock helpers -----

type mockSecretResolver struct {
	getErr    error
	putErr    error
	getResult string
	putKey    string
	putValue  string
}

func (m *mockSecretResolver) GetSecret(ctx context.Context, secretID string) (string, error) {
	if m.getErr != nil {
		return "", m.getErr
	}
	return m.getResult, nil
}

func (m *mockSecretResolver) GetSecretJSON(ctx context.Context, secretID string) (map[string]any, error) {
	return nil, nil
}

func (m *mockSecretResolver) PutSecret(ctx context.Context, secretID, value string) error {
	m.putKey = secretID
	m.putValue = value
	return m.putErr
}

func (m *mockSecretResolver) ListSecrets(ctx context.Context, filter string) ([]string, error) {
	return nil, nil
}

func (m *mockSecretResolver) Close() error {
	return nil
}

type mockAuthStoreWithUser struct {
	mockAuthStoreForHealth
	user *auth.User
	err  error
}

func (m *mockAuthStoreWithUser) GetUserByID(ctx context.Context, userID string) (*auth.User, error) {
	return m.user, m.err
}
