//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// historyPagingEnv is two tenants' History rows in a real database, with the
// stream key of every row so a test can compute the expected page order.
type historyPagingEnv struct {
	pg      *testhelpers.PostgresContainer
	store   *config.PostgresStore
	tenantA *config.CloudAccount
	tenantB *config.CloudAccount
	// stream maps purchase_id (a purchase_history row) or execution_id (a
	// terminal execution) to its stream key, per tenant ("" for ambient rows).
	stream map[string]config.HistoryPageKey
	tenant map[string]string
	owner  map[string]string
	// actionable are the pending executions' ids, per tenant.
	actionable map[string]string
}

func setupHistoryPagingEnv(ctx context.Context, t *testing.T) *historyPagingEnv {
	t.Helper()
	pg := testhelpers.RequireMigratedDatabase(ctx, t, getMigrationsPath())
	env := &historyPagingEnv{
		pg: pg, store: config.NewPostgresStore(pg.DB),
		stream: map[string]config.HistoryPageKey{}, tenant: map[string]string{},
		owner: map[string]string{}, actionable: map[string]string{},
	}
	env.tenantA = &config.CloudAccount{Name: "Paging Tenant A", Provider: "aws", ExternalID: "ext-paging-a", Enabled: true}
	env.tenantB = &config.CloudAccount{Name: "Paging Tenant B", Provider: "aws", ExternalID: "ext-paging-b", Enabled: true}
	require.NoError(t, env.store.CreateCloudAccount(ctx, env.tenantA))
	require.NoError(t, env.store.CreateCloudAccount(ctx, env.tenantB))
	return env
}

// addUser inserts a user so executions can reference it as their creator.
func (e *historyPagingEnv) addUser(ctx context.Context, t *testing.T, id string) {
	t.Helper()
	_, err := e.pg.DB.Pool().Exec(ctx, `INSERT INTO users (id, email, password_hash, group_ids)
		SELECT $1, $2, 'x', ARRAY[id] FROM groups ORDER BY name LIMIT 1`, id, fmt.Sprintf("paging-%s@example.com", id))
	require.NoError(t, err)
}

// getHistoryPage calls getHistory as the token's user and round-trips the
// response through JSON, the shape a client sees.
func getHistoryPage(ctx context.Context, t *testing.T, h *Handler, token string, params map[string]string) HistoryResponse {
	t.Helper()
	req := &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer " + token},
		QueryStringParameters: params,
	}
	out, err := h.getHistory(ctx, req, params)
	require.NoError(t, err)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	var resp HistoryResponse
	require.NoError(t, json.Unmarshal(raw, &resp))
	return resp
}

func (e *historyPagingEnv) account(tenant string) *config.CloudAccount {
	if tenant == "a" {
		return e.tenantA
	}
	return e.tenantB
}

// addPurchase saves a purchase_history row; legacy rows carry only the
// external account number, as direct-execute rows do.
func (e *historyPagingEnv) addPurchase(ctx context.Context, t *testing.T, tenant string, at time.Time, legacy bool) string {
	t.Helper()
	acct := e.account(tenant)
	up := 10.0
	rec := &config.PurchaseHistoryRecord{
		AccountID: acct.ExternalID, PurchaseID: "ph-" + uuid.NewString(), Timestamp: at, Provider: "aws",
		Service: "ec2", Region: "us-east-1", ResourceType: "t3.micro", Count: 1, Term: 1,
		Payment: "no-upfront", UpfrontCost: &up, EstimatedSavings: 1,
	}
	if !legacy {
		rec.CloudAccountID = &acct.ID
	}
	require.NoError(t, e.store.SavePurchaseHistory(ctx, rec))
	var rowID string
	require.NoError(t, e.pg.DB.Pool().QueryRow(ctx,
		`SELECT id::text FROM purchase_history WHERE purchase_id = $1`, rec.PurchaseID).Scan(&rowID))
	e.stream[rec.PurchaseID] = config.HistoryPageKey{Timestamp: at, Source: config.HistorySourcePurchase, ID: rowID}
	e.tenant[rec.PurchaseID] = tenant
	return rec.PurchaseID
}

// addExecution saves an execution. tenant "" makes it ambient (no account on
// it or its recommendation), created by owner.
func (e *historyPagingEnv) addExecution(ctx context.Context, t *testing.T, tenant, owner, status string, at time.Time) string {
	t.Helper()
	exec := &config.PurchaseExecution{
		ExecutionID: uuid.NewString(), IdempotencyKey: uuid.NewString(), Status: status, Error: "boom",
		ScheduledDate: at,
	}
	rec := config.RecommendationRecord{Provider: "aws", Service: "ec2", Region: "us-east-1"}
	if tenant != "" {
		id := e.account(tenant).ID
		exec.CloudAccountID = &id
		rec.CloudAccountID = &id
	}
	if owner != "" {
		exec.CreatedByUserID = &owner
		e.owner[exec.ExecutionID] = owner
	}
	exec.Recommendations = []config.RecommendationRecord{rec}
	require.NoError(t, e.store.SavePurchaseExecution(ctx, exec))
	if slices.Contains(actionableExecutionStatuses, status) {
		e.actionable[exec.ExecutionID] = tenant
		return exec.ExecutionID
	}
	e.stream[exec.ExecutionID] = config.HistoryPageKey{Timestamp: at, Source: config.HistorySourceExecution, ID: exec.ExecutionID}
	e.tenant[exec.ExecutionID] = tenant
	return exec.ExecutionID
}

// seed writes a tenant's rows: purchases and terminal executions sharing
// timestamps within and across the two tables, plus one pending approval
// older than all of them.
func (e *historyPagingEnv) seed(ctx context.Context, t *testing.T, tenant string, base time.Time) {
	t.Helper()
	for i := 0; i < 14; i++ {
		e.addPurchase(ctx, t, tenant, base.Add(-time.Duration(i/3)*time.Hour), i%4 == 0)
	}
	statuses := []string{"failed", "expired", config.StatusCanceled, "partially_completed", "completed"}
	for i := 0; i < 10; i++ {
		e.addExecution(ctx, t, tenant, "", statuses[i%len(statuses)], base.Add(-time.Duration(i/3)*time.Hour))
	}
	e.addExecution(ctx, t, tenant, "", "pending", base.Add(-48*time.Hour))
}

// expected returns the ids of the stream rows include keeps, in stream order.
func (e *historyPagingEnv) expected(include func(id string) bool) []string {
	var ids []string
	for id := range e.stream {
		if include(id) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return e.stream[ids[i]].SortsBefore(e.stream[ids[j]]) })
	return ids
}

// walk pages through /api/history and returns the stream rows' ids in the
// order received and the actionable rows seen per page. between, when set,
// runs after each page.
func (e *historyPagingEnv) walk(ctx context.Context, t *testing.T, h *Handler, token string, limit int, between func(page int)) (stream []string, actionable []map[string]bool, cursors []string) {
	t.Helper()
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 200, "paging must terminate")
		params := map[string]string{"limit": strconv.Itoa(limit)}
		if cursor != "" {
			params["cursor"] = cursor
		}
		resp := getHistoryPage(ctx, t, h, token, params)
		seen := map[string]bool{}
		streamRows := 0
		for _, row := range resp.Purchases {
			if _, ok := e.actionable[row.PurchaseID]; ok {
				seen[row.PurchaseID] = true
				continue
			}
			stream = append(stream, row.PurchaseID)
			streamRows++
		}
		actionable = append(actionable, seen)
		assert.LessOrEqual(t, streamRows, limit)
		assert.Equal(t, resp.HasMore, resp.NextCursor != "")
		if resp.HasMore {
			assert.Equal(t, limit, streamRows, "only the last page may be short")
		}
		if between != nil {
			between(page)
		}
		if !resp.HasMore {
			return stream, actionable, cursors
		}
		cursor = resp.NextCursor
		cursors = append(cursors, cursor)
	}
}

func historyPagingHandlers(env *historyPagingEnv) (admin, scoped *Handler) {
	scopedAuth := new(MockAuthService)
	scopedAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
	scopedAuth.On("GetUser", mock.Anything, mock.Anything).Return(&User{Email: "creator@example.com"}, nil).Maybe()
	scopedAuth.grantScoped(env.tenantA.Name)
	adminAuth := new(MockAuthService)
	adminAuth.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{UserID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}, nil).Maybe()
	adminAuth.On("GetUser", mock.Anything, mock.Anything).Return(&User{Email: "creator@example.com"}, nil).Maybe()
	adminAuth.grantAdmin()
	return &Handler{config: env.store, auth: adminAuth}, &Handler{config: env.store, auth: scopedAuth}
}

// TestIntegration_HistoryPaging_EveryRowOnceInOrder is the issue #674
// regression on real Postgres. Two tenants hold far more History rows than
// the page size, with completed purchases and terminal executions sharing
// timestamps within and across tables. Paging with the cursor must return
// every row exactly once, in stream order, with both pending approvals (older
// than every other row) on the first page and on no other. Rows written at the
// top between two page loads must not shift, repeat or skip any row.
func TestIntegration_HistoryPaging_EveryRowOnceInOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	env := setupHistoryPagingEnv(ctx, t)
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	env.seed(ctx, t, "a", base)
	env.seed(ctx, t, "b", base.Add(-30*time.Minute))
	admin, _ := historyPagingHandlers(env)
	want := env.expected(func(id string) bool {
		k := env.stream[id]
		// A clean completed execution is listed through purchase_history only.
		return k.Source == config.HistorySourcePurchase || !isCleanCompleted(ctx, t, env, id)
	})
	require.Greater(t, len(want), 4*7, "the fixture must span several pages")

	var inserted []string
	got, actionable, _ := env.walk(ctx, t, admin, "admin-token", 7, func(page int) {
		if page == 0 {
			// New rows land at the top, ahead of the cursor.
			inserted = append(inserted,
				env.addPurchase(ctx, t, "a", time.Now().UTC().Truncate(time.Microsecond), false),
				env.addExecution(ctx, t, "b", "", "failed", time.Now().UTC().Truncate(time.Microsecond)))
		}
	})

	assert.Equal(t, want, got, "every row exactly once, in stream order")
	for _, id := range inserted {
		assert.NotContains(t, got, id, "a row written above the cursor belongs to a fresh first page")
	}
	require.NotEmpty(t, actionable)
	for id := range env.actionable {
		assert.True(t, actionable[0][id], "pending approval %s on the first page", id)
		for n := 1; n < len(actionable); n++ {
			assert.False(t, actionable[n][id], "pending approval %s repeated on page %d", id, n)
		}
	}
}

// TestIntegration_HistoryPaging_ScopedUserNeverPagesIntoAnotherTenant pages as
// a user scoped to tenant A while tenant B's rows are newer, interleaved and
// older. The user must get exactly tenant A's rows and their own ambient
// execution, each once and in order, never a tenant-B row or another user's
// ambient row; no cursor may name a hidden row; and has_more must be false on
// the last page although tenant-B rows follow it.
func TestIntegration_HistoryPaging_ScopedUserNeverPagesIntoAnotherTenant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	env := setupHistoryPagingEnv(ctx, t)
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	for i := 0; i < 40; i++ {
		env.addPurchase(ctx, t, "b", base.Add(time.Duration(i+1)*time.Minute), i%2 == 0)
	}
	env.seed(ctx, t, "a", base)
	env.seed(ctx, t, "b", base)
	for i := 0; i < 25; i++ {
		env.addExecution(ctx, t, "b", "", "failed", base.Add(-time.Duration(100+i)*time.Hour))
	}
	other := uuid.NewString()
	env.addUser(ctx, t, scopedUserID)
	env.addUser(ctx, t, other)
	env.addExecution(ctx, t, "", scopedUserID, "expired", base.Add(-5*time.Hour))
	env.addExecution(ctx, t, "", other, "expired", base.Add(-5*time.Hour))
	_, scoped := historyPagingHandlers(env)

	visible := func(id string) bool {
		if isCleanCompleted(ctx, t, env, id) {
			return false
		}
		return env.tenant[id] == "a" || (env.tenant[id] == "" && env.owner[id] == scopedUserID)
	}
	want := env.expected(visible)
	require.Greater(t, len(want), 3*5, "the fixture must span several pages")

	got, actionable, cursors := env.walk(ctx, t, scoped, scopedToken, 5, nil)

	assert.Equal(t, want, got, "exactly the in-scope rows, each once, in stream order")
	for _, id := range got {
		assert.True(t, visible(id), "row %s is outside the caller's scope", id)
	}
	byCursor := map[string]string{}
	for id, k := range env.stream {
		byCursor[encodeHistoryCursor(k)] = id
	}
	require.NotEmpty(t, cursors)
	for _, c := range cursors {
		id, ok := byCursor[c]
		require.True(t, ok, "cursor must name a seeded row")
		assert.True(t, visible(id), "cursor names hidden row %s", id)
	}
	for id, tenant := range env.actionable {
		assert.Equal(t, tenant == "a", actionable[0][id], "pending approval %s (tenant %q) on the first page iff in scope", id, tenant)
	}
}

// isCleanCompleted reports whether id is a completed execution with no error.
// The fixture gives every execution an error, so none is; the check keeps the
// expectation honest if that changes.
func isCleanCompleted(ctx context.Context, t *testing.T, env *historyPagingEnv, id string) bool {
	t.Helper()
	if env.stream[id].Source != config.HistorySourceExecution {
		return false
	}
	exec, err := env.store.GetExecutionByID(ctx, id)
	require.NoError(t, err)
	return exec.Status == "completed" && exec.Error == ""
}
