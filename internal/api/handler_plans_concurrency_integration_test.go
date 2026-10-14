//go:build integration
// +build integration

package api

// Real-Postgres concurrency tests for the create-planned-purchases path (issue
// #1861). The guard that keeps a create off a ramp step some account already
// bought is only a guard if it is decided and acted on under the same lock the
// completion path takes. Read outside that lock it is advisory: two creates both
// see the step free, each mints a root row for it, and approving both
// re-fans-out over the accounts that already bought under a fresh idempotency
// lineage, which is the duplicate commitment the whole guard exists to prevent.
//
// A mock cannot hold a row lock, so this measures against a real database.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// createConcurrencyFixture is a plan sitting on ramp step 2 of 4, wired to a
// handler backed by a real store.
type createConcurrencyFixture struct {
	store   *config.PostgresStore
	pool    *pgxpool.Pool
	handler *Handler
	planID  string
}

func newCreateConcurrencyFixture(ctx context.Context, t *testing.T) *createConcurrencyFixture {
	t.Helper()

	container, err := testhelpers.SetupPostgresContainer(ctx, t)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Cleanup(context.Background()) })
	require.NoError(t, migrations.RunMigrations(ctx, container.DB.Pool(), getMigrationsPath(), "", ""))

	store := config.NewPostgresStore(container.DB)

	plan := &config.PurchasePlan{
		Name:                   "Concurrent Create Plan",
		Enabled:                true,
		NotificationDaysBefore: 2,
		Services:               map[string]config.ServiceConfig{"azure:compute": {Provider: "azure", Service: "compute", Enabled: true, Term: 1, Payment: "monthly", Coverage: 80}},
		RampSchedule: config.RampSchedule{
			Type: "weekly", PercentPerStep: 25, StepIntervalDays: 7,
			CurrentStep: 2, TotalSteps: 4, StartDate: time.Now().AddDate(0, 0, -14),
		},
	}
	require.NoError(t, store.CreatePurchasePlan(ctx, plan))

	acct := &config.CloudAccount{
		Name: "acct-a", Provider: "azure", ExternalID: "fixture-subscription-a", Enabled: true,
	}
	require.NoError(t, store.CreateCloudAccount(ctx, acct))
	require.NoError(t, store.SetPlanAccounts(ctx, plan.ID, []string{acct.ID}))

	// A real users row: createPurchaseExecutionsTx stamps the session user onto
	// created_by_user_id, which is a foreign key, so a synthetic UUID would make
	// every insert fail for a reason unrelated to what these tests measure.
	userID := uuid.New().String()
	_, err = container.DB.Pool().Exec(ctx, `
		INSERT INTO users (id, email, password_hash, salt, active, group_ids, created_at, updated_at)
		SELECT $1, 'creator@example.com', 'x', 'y', true, ARRAY[g.id], now(), now()
		  FROM groups g ORDER BY g.name LIMIT 1`, userID)
	require.NoError(t, err)

	mockAuth := new(MockAuthService)
	session := &Session{UserID: userID, Email: "creator@example.com"}
	mockAuth.On("ValidateSession", mock.Anything, "admin-token").Return(session, nil).Maybe()
	mockAuth.grantAdmin()

	return &createConcurrencyFixture{
		store:   store,
		pool:    container.DB.Pool(),
		handler: &Handler{config: store, auth: mockAuth},
		planID:  plan.ID,
	}
}

// create exercises authentication and scheduling against the real store.
func (f *createConcurrencyFixture) create(ctx context.Context, count int) (*CreatePlannedPurchasesResponse, error) {
	body, err := json.Marshal(CreatePlannedPurchasesRequest{Count: count, StartDate: time.Now().AddDate(0, 0, 7).Format("2006-01-02")})
	if err != nil {
		return nil, err
	}
	return f.handler.createPlannedPurchases(ctx, &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}, Body: string(body)}, f.planID)
}

type membershipChangingStore struct {
	config.StoreInterface
	beforeLock func() error
}

func (s membershipChangingStore) LockPurchasePlanTx(ctx context.Context, tx pgx.Tx, id string) (*config.PurchasePlan, error) {
	if err := s.beforeLock(); err != nil {
		return nil, err
	}
	return s.StoreInterface.LockPurchasePlanTx(ctx, tx, id)
}

func TestCreatePlannedPurchases_MembershipChangesBeforeLock(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	accounts, err := f.store.GetPlanAccounts(ctx, f.planID)
	require.NoError(t, err)
	inaccessible := &config.CloudAccount{Name: "acct-b", Provider: "azure", ExternalID: "fixture-subscription-b", Enabled: true}
	require.NoError(t, f.store.CreateCloudAccount(ctx, inaccessible))
	var userID string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT id FROM users WHERE email='creator@example.com'`).Scan(&userID))
	auth := new(MockAuthService)
	auth.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{UserID: userID}, nil)
	auth.grantScoped(accounts[0].ID)
	f.handler.auth = auth
	f.handler.config = membershipChangingStore{StoreInterface: f.store, beforeLock: func() error {
		return f.store.SetPlanAccounts(ctx, f.planID, []string{inaccessible.ID})
	}}
	_, err = f.create(ctx, 1)
	require.ErrorIs(t, err, errNotFound)
	assert.Zero(t, f.executionsForStep(ctx, t, 3))
}

func TestCreatePlannedPurchases_RoutedDatesAndRollback(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	auth := f.handler.auth.(*MockAuthService)
	auth.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	first := time.Now().AddDate(0, 0, 7).UTC().Truncate(24 * time.Hour)
	request := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token", "X-CSRF-Token": "test-csrf", "Content-Type": "application/json"},
		Body:           `{"count":2,"start_date":"` + first.Format("2006-01-02") + `"}`,
		RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "POST", Path: "/api/plans/" + f.planID + "/purchases"}}}
	// Fail the second insert in this dedicated test database, proving atomic rollback.
	_, err := f.pool.Exec(ctx, `CREATE FUNCTION fail_step_four() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.step_number = 4 THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_step_four BEFORE INSERT ON purchase_executions FOR EACH ROW EXECUTE FUNCTION fail_step_four()`)
	require.NoError(t, err)
	response, err := f.handler.HandleRequest(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 500, response.StatusCode)
	assert.Zero(t, f.executionsForStep(ctx, t, 3))
	_, err = f.pool.Exec(ctx, `ALTER TABLE purchase_executions DISABLE TRIGGER fail_step_four`)
	require.NoError(t, err)
	response, err = f.handler.HandleRequest(ctx, request)
	require.NoError(t, err)
	require.Equal(t, 200, response.StatusCode)
	assert.JSONEq(t, `{"created":2}`, response.Body)
	var scheduled time.Time
	var creator *string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT scheduled_date, created_by_user_id FROM purchase_executions WHERE plan_id=$1 AND step_number=3`, f.planID).Scan(&scheduled, &creator))
	assert.True(t, first.Equal(scheduled))
	require.NotNil(t, creator)
	plan, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	assert.True(t, first.Equal(*plan.NextExecutionDate))
	plan.RampSchedule.CurrentStep++
	assert.True(t, plan.RampSchedule.GetNextPurchaseDate().Equal(first.AddDate(0, 0, 7)))
}

// executionsForStep counts the plan's execution rows stamped with step.
func (f *createConcurrencyFixture) executionsForStep(ctx context.Context, t *testing.T, step int) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM purchase_executions WHERE plan_id = $1 AND step_number = $2`,
		f.planID, step).Scan(&n))
	return n
}

// TestCreatePlannedPurchases_ConcurrentCreatesMintOneStepEach is the issue
// #1861 / F-A regression guard.
//
// Two creates enter with the same CurrentStep and race. Exactly one may win:
// the loser must see the winner's row and be refused, not mint a second root
// row for the same step. Serialized by hand this proves nothing, because the
// loser would read the winner's committed row anyway; the goroutines are
// released together from a shared gate so they contend for real.
func TestCreatePlannedPurchases_ConcurrentCreatesMintOneStepEach(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)

	const racers = 6
	start := make(chan struct{})
	errs := make(chan error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.create(ctx, 2)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	won, refused, seen := 0, 0, 0
	for err := range errs {
		seen++
		if err == nil {
			won++
			continue
		}
		ce, ok := IsClientError(err)
		require.True(t, ok, "a losing create must be refused cleanly, got %v", err)
		require.Equal(t, 409, ce.code, "the loser must be refused as a conflict, got %d: %v", ce.code, err)
		refused++
	}
	require.Equal(t, racers, seen, "every racer must report")
	assert.Equal(t, 1, won, "exactly one create may mint ramp step 3")
	assert.Equal(t, racers-1, refused)

	assert.Equal(t, 1, f.executionsForStep(ctx, t, 3),
		"ramp step 3 must end with exactly one execution row, not one per racer")
	assert.Equal(t, 1, f.executionsForStep(ctx, t, 4),
		"the same winning transaction must also create step 4 exactly once")
}

// TestCreatePlannedPurchases_BlocksOnTheRampLockAndSeesTheWinner forces the
// interleaving the race test can only hope for, so the guard's dependence on the
// plan row lock is measured rather than argued.
//
// A transaction outside the handler takes the plan row's FOR UPDATE lock and
// inserts a pending root row for step 3, exactly as a winning create would.
// While that transaction is open, a concurrent create must BLOCK rather than
// read the pre-insert state: if it could proceed, it would find step 3 free and
// mint a second root row for it. Once the holder commits, the blocked create
// must observe the committed row and refuse.
func TestCreatePlannedPurchases_BlocksOnTheRampLockAndSeesTheWinner(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)

	holder, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(ctx) }()

	var lockedID string
	require.NoError(t,
		holder.QueryRow(ctx, `SELECT id FROM purchase_plans WHERE id = $1 FOR UPDATE`, f.planID).Scan(&lockedID))
	require.Equal(t, f.planID, lockedID)

	// The winning create's insert, still uncommitted.
	_, err = holder.Exec(ctx, `
		INSERT INTO purchase_executions (plan_id, execution_id, status, step_number, scheduled_date)
		VALUES ($1, $2, 'pending', 3, now())`, f.planID, uuid.New().String())
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, createErr := f.create(ctx, 1)
		done <- createErr
	}()

	// Wait until Postgres reports the create's own locking read blocked. Match
	// the query text: if the handler stopped taking the row lock on the READ,
	// the blocked statement would be something else and this poll would never
	// match, failing the test rather than silently passing it.
	require.Eventually(t, func() bool {
		var blocked int
		if qErr := f.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock'
			   AND state = 'active'
			   AND pid <> pg_backend_pid()
			   AND query ILIKE '%purchase_plans%FOR UPDATE%'`,
		).Scan(&blocked); qErr != nil {
			return false
		}
		return blocked > 0
	}, 15*time.Second, 25*time.Millisecond,
		"the concurrent create's locking read must wait on the plan row lock")

	select {
	case earlyErr := <-done:
		t.Fatalf("createPlannedPurchases returned (%v) while another transaction held the plan row lock: it did not serialize", earlyErr)
	default:
	}

	require.NoError(t, holder.Commit(ctx))

	select {
	case loserErr := <-done:
		require.Error(t, loserErr, "the blocked create must see the committed row and refuse")
		ce, ok := IsClientError(loserErr)
		require.True(t, ok)
		assert.Equal(t, 409, ce.code)
	case <-time.After(15 * time.Second):
		t.Fatal("createPlannedPurchases never returned after the plan row lock was released")
	}

	assert.Equal(t, 1, f.executionsForStep(ctx, t, 3),
		"the blocked create must not have added a second row for step 3")
}

// TestCreatePlannedPurchases_CanceledStepIsReschedulable is the positive
// control for the widened predicate. A step whose executions were all canceled
// bought nothing and has nothing in flight, so the operator must be able to
// schedule it again; a guard that refused it would turn a cancel into a
// dead end.
func TestCreatePlannedPurchases_CanceledStepIsReschedulable(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)

	require.NoError(t, f.store.SavePurchaseExecution(ctx, &config.PurchaseExecution{
		ExecutionID:   uuid.New().String(),
		PlanID:        f.planID,
		Status:        config.StatusCanceled,
		StepNumber:    3,
		ScheduledDate: time.Now(),
	}))

	resp, err := f.create(ctx, 1)
	require.NoError(t, err, "a step whose only rows were canceled must be reschedulable")
	assert.Equal(t, 1, resp.Created)
}

func TestUpdatePlan_CapPresencePostgres(t *testing.T) {
	cap100, cap5 := 100.0, 5.0
	cases := []struct {
		name   string
		field  string
		auto   bool
		status int
		want   *float64
	}{
		{"manual null clears", `,"max_step_commitment":null`, false, 200, nil},
		{"omitted preserves", "", false, 200, &cap100},
		{"numeric replaces", `,"max_step_commitment":5`, false, 200, &cap5},
		{"automatic omitted preserves", "", true, 200, &cap100},
		{"automatic numeric decreases", `,"max_step_commitment":5`, true, 200, &cap5},
		{"automatic null refuses", `,"max_step_commitment":null`, true, 400, &cap100},
		{"string refuses", `,"max_step_commitment":"5"`, false, 400, &cap100},
		{"zero refuses", `,"max_step_commitment":0`, false, 400, &cap100},
		{"negative refuses", `,"max_step_commitment":-1`, false, 400, &cap100},
		{"precision refuses", `,"max_step_commitment":1.005`, false, 400, &cap100},
		{"unknown remains ignored", `,"max_step_commitment":null,"future_field":false`, false, 200, nil},
		{"case-insensitive null", `,"MAX_STEP_COMMITMENT":null`, false, 200, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newCreateConcurrencyFixture(ctx, t)
			plan, err := f.store.GetPurchasePlan(ctx, f.planID)
			require.NoError(t, err)
			plan.Enabled, plan.AutoPurchase = true, tc.auto
			plan.NotificationDaysBefore = 2
			plan.MaxStepCommitment = &cap100
			plan.Services = map[string]config.ServiceConfig{"aws:ec2": {Provider: "aws", Service: "ec2", Enabled: true, Term: 1, Payment: "no-upfront", Coverage: 80}}
			next := time.Now().AddDate(0, 0, 7)
			plan.NextExecutionDate = &next
			require.NoError(t, f.store.UpdatePurchasePlan(ctx, plan))
			before, err := f.store.GetPurchasePlan(ctx, f.planID)
			require.NoError(t, err)
			auth := f.handler.auth.(*MockAuthService)
			auth.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil)
			body := fmt.Sprintf(`{"name":"Concurrent Create Plan","provider":"aws","service":"ec2","term":1,"payment":"no-upfront","target_coverage":80,"ramp_schedule":"weekly-25pct","enabled":true,"auto_purchase":%t,"notification_days_before":2%s}`, tc.auto, tc.field)
			req := &events.LambdaFunctionURLRequest{
				Headers: map[string]string{"Authorization": "Bearer admin-token", "Content-Type": "application/json", "X-CSRF-Token": "test-csrf"}, Body: body,
				RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "PUT", Path: "/api/plans/" + f.planID}},
			}
			response, err := f.handler.HandleRequest(ctx, req)
			require.NoError(t, err)
			require.Equal(t, tc.status, response.StatusCode, response.Body)
			stored, err := f.store.GetPurchasePlan(ctx, f.planID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, stored.MaxStepCommitment)
			assert.Equal(t, tc.auto, stored.AutoPurchase)
			assert.Equal(t, before.RampSchedule.CurrentStep, stored.RampSchedule.CurrentStep)
			assert.True(t, before.RampSchedule.StartDate.Equal(stored.RampSchedule.StartDate))
			assert.True(t, before.NextExecutionDate.Equal(*stored.NextExecutionDate))
			req.RequestContext.HTTP.Method, req.Body = "GET", ""
			response, err = f.handler.HandleRequest(ctx, req)
			require.NoError(t, err)
			require.Equal(t, 200, response.StatusCode, response.Body)
			var returned config.PurchasePlan
			require.NoError(t, json.Unmarshal([]byte(response.Body), &returned))
			assert.Equal(t, tc.want, returned.MaxStepCommitment)
		})
	}
}

type concurrentCapStore struct{ config.StoreInterface }

func (s concurrentCapStore) UpdatePurchasePlan(ctx context.Context, plan *config.PurchasePlan) error {
	winner, err := s.GetPurchasePlan(ctx, plan.ID)
	if err != nil {
		return err
	}
	cap80 := 80.0
	winner.MaxStepCommitment = &cap80
	if err = s.StoreInterface.UpdatePurchasePlan(ctx, winner); err != nil {
		return err
	}
	return s.StoreInterface.UpdatePurchasePlan(ctx, plan)
}

func TestUpdatePlan_CapNullPreservesConcurrentWinner(t *testing.T) {
	ctx := context.Background()
	f := newCreateConcurrencyFixture(ctx, t)
	plan, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	cap100 := 100.0
	plan.MaxStepCommitment = &cap100
	require.NoError(t, f.store.UpdatePurchasePlan(ctx, plan))
	f.handler.auth.(*MockAuthService).On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	f.handler.config = concurrentCapStore{StoreInterface: f.store}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token", "Content-Type": "application/json", "X-CSRF-Token": "test-csrf"}, Body: `{"name":"Concurrent Create Plan","ramp_schedule":"weekly-25pct","enabled":false,"auto_purchase":false,"max_step_commitment":null}`, RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "PUT", Path: "/api/plans/" + f.planID}}}
	response, err := f.handler.HandleRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, 409, response.StatusCode, response.Body)
	stored, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	require.NotNil(t, stored.MaxStepCommitment)
	assert.Equal(t, 80.0, *stored.MaxStepCommitment)
}
