//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Real-PostgreSQL tests for issue #1102 item 2: approving a registration
// flipped it to approved and only then created the cloud account, in separate
// statements, so a failed create left it approved with no account. Approval,
// account creation and the link now share one transaction.

func newRegistrationForApproval(ctx context.Context, t *testing.T, f *createConcurrencyFixture, externalID string) string {
	t.Helper()
	reg := &config.AccountRegistration{
		ReferenceToken: "tok-" + externalID, Status: "pending", Provider: "azure",
		ExternalID: externalID, AccountName: "Fixture " + externalID, ContactEmail: "owner@example.com",
	}
	require.NoError(t, f.store.CreateAccountRegistration(ctx, reg))
	return reg.ID
}

func approveRequest(t *testing.T, externalID string) *events.LambdaFunctionURLRequest {
	t.Helper()
	body, err := json.Marshal(CloudAccountRequest{
		Name: "Fixture " + externalID, Provider: "azure", ExternalID: externalID, AzureAuthMode: "managed_identity",
	})
	require.NoError(t, err)
	return &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}, Body: string(body)}
}

type registrationRow struct {
	status         string
	cloudAccountID *string
	reviewedAt     *time.Time
}

func readRegistration(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) registrationRow {
	t.Helper()
	var r registrationRow
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, cloud_account_id::text, reviewed_at FROM account_registrations WHERE id = $1`, id,
	).Scan(&r.status, &r.cloudAccountID, &r.reviewedAt))
	return r
}

func countAccounts(ctx context.Context, t *testing.T, pool *pgxpool.Pool, externalID string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM cloud_accounts WHERE provider = 'azure' AND external_id = $1`, externalID).Scan(&n))
	return n
}

// A failed account insert must leave the registration exactly as it was.
func TestApproveRegistration_FailedAccountCreateLeavesRegistrationPending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	f := newCreateConcurrencyFixture(ctx, t)

	const ext = "fixture-subscription-dup"
	require.NoError(t, f.store.CreateCloudAccount(ctx, &config.CloudAccount{
		Name: "Already registered", Provider: "azure", ExternalID: ext, Enabled: true,
	}))
	regID := newRegistrationForApproval(ctx, t, f, ext)

	_, err := f.handler.approveRegistration(ctx, approveRequest(t, ext), regID)
	requireClientCode(t, err, 409)

	row := readRegistration(ctx, t, f.pool, regID)
	assert.Equal(t, "pending", row.status, "a failed approval must not leave the registration approved")
	assert.Nil(t, row.cloudAccountID)
	assert.Nil(t, row.reviewedAt)
	assert.Equal(t, 1, countAccounts(ctx, t, f.pool, ext))

	// Still pending, so the admin can act on it again.
	_, err = f.handler.rejectRegistration(ctx, approveRequest(t, ext), regID)
	require.NoError(t, err)
}

// Concurrent approvals of one registration: exactly one wins, one account is
// created, and the registration is linked to it.
func TestApproveRegistration_ConcurrentApprovalsCreateOneAccount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	f := newCreateConcurrencyFixture(ctx, t)

	const ext = "fixture-subscription-race"
	regID := newRegistrationForApproval(ctx, t, f, ext)

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]any, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.handler.approveRegistration(ctx, approveRequest(t, ext), regID)
		}()
	}
	close(start)
	wg.Wait()

	var winner *config.CloudAccount
	for i := range workers {
		if errs[i] == nil {
			require.Nil(t, winner, "more than one approval succeeded")
			winner = results[i].(*config.CloudAccount)
			continue
		}
		requireClientCode(t, errs[i], 409)
	}
	require.NotNil(t, winner, "no approval succeeded")

	row := readRegistration(ctx, t, f.pool, regID)
	assert.Equal(t, "approved", row.status)
	require.NotNil(t, row.cloudAccountID)
	assert.Equal(t, winner.ID, *row.cloudAccountID)
	assert.Equal(t, 1, countAccounts(ctx, t, f.pool, ext))
}

// installSlowAccountInsert makes every cloud_accounts insert sleep, and fail
// when the external_id ends in "-fail", so an approval holds its transaction
// open long enough for a concurrent reject to queue behind it.
func installSlowAccountInsert(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		CREATE FUNCTION test_slow_account_insert() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_sleep(1);
			IF NEW.external_id LIKE '%-fail' THEN
				RAISE EXCEPTION 'injected cloud_accounts insert failure';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER test_slow_account_insert BEFORE INSERT ON cloud_accounts
			FOR EACH ROW EXECUTE FUNCTION test_slow_account_insert();`)
	require.NoError(t, err)
}

// waitForActivity polls pg_stat_activity until a backend matches the predicate.
func waitForActivity(ctx context.Context, t *testing.T, pool *pgxpool.Pool, predicate string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE `+predicate).Scan(&n)
		return err == nil && n > 0
	}, 10*time.Second, 10*time.Millisecond, "no backend matched %s", predicate)
}

// A reject that arrives while an approval is mid-transaction waits for it.
// When the approval commits, the reject gets 409; when the approval fails and
// rolls back, the reject proceeds and no account or approval is left behind.
func TestApproveRegistration_RejectQueuedBehindApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	f := newCreateConcurrencyFixture(ctx, t)
	installSlowAccountInsert(ctx, t, f.pool)

	for _, tc := range []struct {
		ext            string
		approveCode    int // 0: approval succeeds
		wantStatus     string
		wantRejectCode int // 0: reject succeeds
		wantAccounts   int
	}{
		{ext: "fixture-subscription-commit", wantStatus: "approved", wantRejectCode: 409, wantAccounts: 1},
		{ext: "fixture-subscription-fail", approveCode: 500, wantStatus: "rejected", wantAccounts: 0},
	} {
		t.Run(tc.ext, func(t *testing.T) {
			regID := newRegistrationForApproval(ctx, t, f, tc.ext)

			approveDone := make(chan error, 1)
			go func() {
				_, err := f.handler.approveRegistration(ctx, approveRequest(t, tc.ext), regID)
				approveDone <- err
			}()
			// The approval has changed the status and is now inside the
			// account insert, holding the registration row lock.
			waitForActivity(ctx, t, f.pool, `wait_event = 'PgSleep' AND query LIKE '%INSERT INTO cloud_accounts%'`)

			rejectDone := make(chan error, 1)
			go func() {
				_, err := f.handler.rejectRegistration(ctx, &events.LambdaFunctionURLRequest{
					Headers: map[string]string{"Authorization": "Bearer admin-token"},
				}, regID)
				rejectDone <- err
			}()
			// The reject's status change is blocked on that lock, not run after.
			waitForActivity(ctx, t, f.pool, `wait_event_type = 'Lock' AND query LIKE '%UPDATE account_registrations%'`)

			approveErr := <-approveDone
			rejectErr := <-rejectDone
			switch tc.approveCode {
			case 0:
				require.NoError(t, approveErr)
			default:
				require.Error(t, approveErr)
				_, isClient := IsClientError(approveErr)
				assert.False(t, isClient, "an injected database failure is a 500, got %v", approveErr)
			}
			if tc.wantRejectCode == 0 {
				require.NoError(t, rejectErr)
			} else {
				requireClientCode(t, rejectErr, tc.wantRejectCode)
			}

			row := readRegistration(ctx, t, f.pool, regID)
			assert.Equal(t, tc.wantStatus, row.status)
			assert.Equal(t, tc.wantAccounts, countAccounts(ctx, t, f.pool, tc.ext))
			if tc.wantAccounts == 0 {
				assert.Nil(t, row.cloudAccountID, fmt.Sprintf("registration %s", regID))
			} else {
				assert.NotNil(t, row.cloudAccountID)
			}
		})
	}
}
