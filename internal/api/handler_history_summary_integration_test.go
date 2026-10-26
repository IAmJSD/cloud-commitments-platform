//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/migrations"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// summarySeedRow is one seeded History row and how the summary must treat it.
// bucket is the HistorySummary counter it lands in ("" for canceled, which
// only counts toward total_purchases); dollars marks rows whose upfront and
// savings count toward the dollar totals.
type summarySeedRow struct {
	tenant  string // "a", "b" or "" for an unattributed (ambient) execution
	owner   string // creator of an ambient execution
	bucket  string
	dollars bool
	upfront float64
	savings float64
	at      time.Time
}

type historySummaryEnv struct {
	store   *config.PostgresStore
	tenantA *config.CloudAccount
	tenantB *config.CloudAccount
	other   string
	rows    []summarySeedRow
}

func setupHistorySummaryEnv(ctx context.Context, t *testing.T) (*historySummaryEnv, *testhelpers.PostgresContainer) {
	t.Helper()
	container := testhelpers.RequirePostgresContainer(ctx, t)
	t.Cleanup(func() { _ = container.Cleanup(context.Background()) })
	require.NoError(t, migrations.RunMigrations(ctx, container.DB.Pool(), getMigrationsPath(), "", ""))
	env := &historySummaryEnv{store: config.NewPostgresStore(container.DB)}
	env.tenantA = &config.CloudAccount{Name: "Tenant A", Provider: "aws", ExternalID: "ext-tenant-a", Enabled: true}
	env.tenantB = &config.CloudAccount{Name: "Tenant B", Provider: "aws", ExternalID: "ext-tenant-b", Enabled: true}
	require.NoError(t, env.store.CreateCloudAccount(ctx, env.tenantA))
	require.NoError(t, env.store.CreateCloudAccount(ctx, env.tenantB))
	env.other = uuid.NewString()
	for i, id := range []string{scopedUserID, env.other} {
		_, err := container.DB.Pool().Exec(ctx, `INSERT INTO users (id, email, password_hash, group_ids)
			SELECT $1, $2, 'x', ARRAY[id] FROM groups ORDER BY name LIMIT 1`, id, fmt.Sprintf("history-summary-%d@example.com", i))
		require.NoError(t, err)
	}
	return env, container
}

func (e *historySummaryEnv) account(tenant string) *config.CloudAccount {
	if tenant == "a" {
		return e.tenantA
	}
	return e.tenantB
}

// addPurchase saves a purchase_history row. legacy rows carry only the
// external account number, as direct-execute rows do.
func (e *historySummaryEnv) addPurchase(ctx context.Context, t *testing.T, c *testhelpers.PostgresContainer, tenant string, at time.Time, upfront, savings float64, legacy, revoked bool) {
	t.Helper()
	acct := e.account(tenant)
	rec := &config.PurchaseHistoryRecord{
		AccountID: acct.ExternalID, PurchaseID: uuid.NewString(), Timestamp: at, Provider: "aws",
		Service: "ec2", Region: "us-east-1", ResourceType: "t3.micro", Count: 1, Term: 1,
		Payment: "no-upfront", UpfrontCost: &upfront, EstimatedSavings: savings,
	}
	if !legacy {
		rec.CloudAccountID = &acct.ID
	}
	require.NoError(t, e.store.SavePurchaseHistory(ctx, rec))
	if legacy {
		// SavePurchaseHistory now links a row to the registered account with
		// the same external id (#1235); clear it so this stays a legacy row.
		tag, err := c.DB.Pool().Exec(ctx, `UPDATE purchase_history SET cloud_account_id = NULL WHERE purchase_id = $1`, rec.PurchaseID)
		require.NoError(t, err)
		require.Equal(t, int64(1), tag.RowsAffected())
	}
	row := summarySeedRow{tenant: tenant, bucket: "completed", dollars: true, upfront: upfront, savings: savings, at: at}
	if revoked {
		_, err := c.DB.Pool().Exec(ctx, `UPDATE purchase_history SET revoked_at = now() WHERE purchase_id = $1`, rec.PurchaseID)
		require.NoError(t, err)
		row.bucket, row.dollars = "revoked", false
	}
	e.rows = append(e.rows, row)
}

// addExecution saves an execution. tenant "" makes it ambient (no account on
// the execution or its recommendation), owned by owner. webBulk puts the
// account on the recommendation only, as web bulk purchases do.
func (e *historySummaryEnv) addExecution(ctx context.Context, t *testing.T, tenant, owner, status, execErr, bucket string, at time.Time, webBulk bool) {
	t.Helper()
	exec := &config.PurchaseExecution{
		ExecutionID: uuid.NewString(), IdempotencyKey: uuid.NewString(), Status: status, Error: execErr,
		ScheduledDate: at, TotalUpfrontCost: 500, EstimatedSavings: 50,
	}
	rec := config.RecommendationRecord{Provider: "aws", Service: "ec2", Region: "us-east-1", UpfrontCost: 500, Savings: 50}
	if tenant != "" {
		id := e.account(tenant).ID
		rec.CloudAccountID = &id
		if !webBulk {
			exec.CloudAccountID = &id
		}
	}
	if owner != "" {
		exec.CreatedByUserID = &owner
	}
	exec.Recommendations = []config.RecommendationRecord{rec}
	require.NoError(t, e.store.SavePurchaseExecution(ctx, exec))
	if bucket != "skip" {
		e.rows = append(e.rows, summarySeedRow{tenant: tenant, owner: owner, bucket: bucket, at: at})
	}
}

// seedTenant writes one tenant's rows. The canceled rows alone exceed the
// terminal-executions cap (DefaultListLimit) and the purchase rows exceed the
// limit the test requests, so the listed page is truncated.
func (e *historySummaryEnv) seedTenant(ctx context.Context, t *testing.T, c *testhelpers.PostgresContainer, tenant string, scale float64, recent, old time.Time) {
	t.Helper()
	for i := 0; i < 9; i++ {
		e.addPurchase(ctx, t, c, tenant, recent.Add(time.Duration(i)*time.Hour), scale*float64(i+1)+0.25, scale*float64(i)+0.5, i%3 == 0, false)
	}
	e.addPurchase(ctx, t, c, tenant, recent.Add(20*time.Hour), scale*999, scale*99, false, true)
	e.addPurchase(ctx, t, c, tenant, recent.Add(21*time.Hour), scale*888, scale*88, true, true)
	for i := 0; i < config.DefaultListLimit+5; i++ {
		e.addExecution(ctx, t, tenant, "", config.StatusCanceled, "", "", old.Add(time.Duration(i)*time.Minute), false)
	}
	steps := []struct{ status, err, bucket string }{
		{"pending", "", "pending"}, {"notified", "", "pending"}, {"scheduled", "", "pending"},
		{"approved", "", "in_progress"}, {"running", "", "in_progress"}, {"paused", "", "in_progress"},
		{"failed", "boom", "failed"}, {"failed", "boom", "failed"}, {"expired", "", "expired"},
		{"completed", "history write failed", "completed"}, // audit gap: counted, no dollars
		{"partially_completed", "one rec failed", "completed"},
		{"completed", "", "skip"}, // clean completed: shown via purchase_history only
		{config.StatusCanceled, "", ""},
	}
	for i, s := range steps {
		e.addExecution(ctx, t, tenant, "", s.status, s.err, s.bucket, recent.Add(time.Duration(30+i)*time.Hour), false)
	}
	e.addExecution(ctx, t, tenant, "", "pending", "", "pending", recent.Add(50*time.Hour), true)
}

// expectedSummary is the manual sum over the seeded rows a viewer may see.
func expectedSummary(rows []summarySeedRow, inScope func(summarySeedRow) bool) HistorySummary {
	want := HistorySummary{TotalUpfront: new(float64)}
	for _, r := range rows {
		if !inScope(r) {
			continue
		}
		want.TotalPurchases++
		switch r.bucket {
		case "completed":
			want.TotalCompleted++
		case "revoked":
			want.TotalRevoked++
		case "pending":
			want.TotalPending++
		case "in_progress":
			want.TotalInProgress++
		case "failed":
			want.TotalFailed++
		case "expired":
			want.TotalExpired++
		}
		if r.dollars {
			*want.TotalUpfront += r.upfront
			want.TotalMonthlySavings += r.savings
		}
	}
	want.TotalAnnualSavings = want.TotalMonthlySavings * 12
	return want
}

func assertSummaryEqual(t *testing.T, want, got HistorySummary) {
	t.Helper()
	assert.Equal(t, want.TotalPurchases, got.TotalPurchases, "total_purchases")
	assert.Equal(t, want.TotalCompleted, got.TotalCompleted, "total_completed")
	assert.Equal(t, want.TotalRevoked, got.TotalRevoked, "total_revoked")
	assert.Equal(t, want.TotalPending, got.TotalPending, "total_pending")
	assert.Equal(t, want.TotalInProgress, got.TotalInProgress, "total_in_progress")
	assert.Equal(t, want.TotalFailed, got.TotalFailed, "total_failed")
	assert.Equal(t, want.TotalExpired, got.TotalExpired, "total_expired")
	require.NotNil(t, got.TotalUpfront)
	assert.InDelta(t, *want.TotalUpfront, *got.TotalUpfront, 1e-6, "total_upfront")
	assert.InDelta(t, want.TotalMonthlySavings, got.TotalMonthlySavings, 1e-6, "total_monthly_savings")
	assert.InDelta(t, want.TotalAnnualSavings, got.TotalAnnualSavings, 1e-6, "total_annual_savings")
}

func historyRequest(token string, params map[string]string) *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{
		Headers:               map[string]string{"Authorization": "Bearer " + token},
		QueryStringParameters: params,
	}
}

func getHistoryJSON(ctx context.Context, t *testing.T, h *Handler, token string, params map[string]string) (HistoryResponse, map[string]any) {
	t.Helper()
	out, err := h.getHistory(ctx, historyRequest(token, params), params)
	require.NoError(t, err)
	resp := out.(HistoryResponse)
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	return resp, fields
}

// TestIntegration_HistorySummary_TotalsCoverAllInScopeRows is the issue #675
// regression on real Postgres. Two tenants each hold more rows than the
// History caps, including revoked, audit-gap, partially completed, canceled
// and in-progress rows plus ambient executions owned by different users. The
// summary for a user scoped to tenant A must equal a manual sum over every
// tenant-A row (and the ambient row that user created), never including a
// tenant-B row; the unrestricted admin's summary must equal the sum over all
// rows. Before the fix the summary was computed from the capped page.
func TestIntegration_HistorySummary_TotalsCoverAllInScopeRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	env, container := setupHistorySummaryEnv(ctx, t)
	// Recent enough that no pending row is stale, so the expire sweep a
	// request fires cannot change the next request's rows.
	recent := time.Now().UTC().Add(-3 * 24 * time.Hour).Truncate(time.Second)
	old := recent.Add(-100 * 24 * time.Hour)
	env.seedTenant(ctx, t, container, "a", 1, recent, old)
	env.seedTenant(ctx, t, container, "b", 1000, recent, old)
	env.addExecution(ctx, t, "", scopedUserID, "pending", "", "pending", recent.Add(60*time.Hour), false)
	env.addExecution(ctx, t, "", env.other, "failed", "boom", "failed", recent.Add(61*time.Hour), false)

	const adminToken = "admin-token"
	const adminUserID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	scopedAuth := new(MockAuthService)
	scopedAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
	scopedAuth.On("GetUser", mock.Anything, mock.Anything).Return(&User{Email: "creator@example.com"}, nil).Maybe()
	scopedAuth.grantScoped(env.tenantA.Name)
	adminAuth := new(MockAuthService)
	adminAuth.On("GetUser", mock.Anything, mock.Anything).Return(&User{Email: "creator@example.com"}, nil).Maybe()
	adminAuth.On("ValidateSession", mock.Anything, adminToken).Return(&Session{UserID: adminUserID}, nil).Maybe()
	adminAuth.grantAdmin()
	scoped := &Handler{config: env.store, auth: scopedAuth}
	admin := &Handler{config: env.store, auth: adminAuth}

	scopedRow := func(r summarySeedRow) bool {
		return r.tenant == "a" || (r.tenant == "" && r.owner == scopedUserID)
	}
	everyRow := func(summarySeedRow) bool { return true }

	t.Run("scoped user sees only tenant A totals", func(t *testing.T) {
		resp, fields := getHistoryJSON(ctx, t, scoped, scopedToken, map[string]string{"limit": "5"})
		require.True(t, resp.Truncated, "fixture must exceed the caps")
		want := expectedSummary(env.rows, scopedRow)
		require.Greater(t, want.TotalPurchases, len(resp.Purchases), "totals must exceed the listed page")
		assertSummaryEqual(t, want, resp.Summary)
		assert.Equal(t, true, fields["summary_complete"])
		assert.Equal(t, 130, want.TotalPurchases, "11 purchases + 105 canceled + 13 steps - 1 clean + web bulk + own ambient")
	})

	t.Run("admin sees both tenants", func(t *testing.T) {
		resp, fields := getHistoryJSON(ctx, t, admin, adminToken, map[string]string{"limit": "5"})
		require.True(t, resp.Truncated)
		assertSummaryEqual(t, expectedSummary(env.rows, everyRow), resp.Summary)
		assert.Equal(t, true, fields["summary_complete"])
	})

	t.Run("scoped user filtering on tenant B gets no cross-tenant counts", func(t *testing.T) {
		resp, _ := getHistoryJSON(ctx, t, scoped, scopedToken, map[string]string{"account_ids": env.tenantB.ID})
		assert.Empty(t, resp.Purchases)
		assertSummaryEqual(t, HistorySummary{TotalUpfront: new(float64)}, resp.Summary)
	})

	t.Run("account filter on tenant A for admin", func(t *testing.T) {
		resp, _ := getHistoryJSON(ctx, t, admin, adminToken, map[string]string{"account_ids": env.tenantA.ID, "limit": "5"})
		tenantA := func(r summarySeedRow) bool { return r.tenant == "a" }
		assertSummaryEqual(t, expectedSummary(env.rows, tenantA), resp.Summary)
	})
}

// TestIntegration_HistorySummary_MatchesListedRowsUnderTheCap is the drift
// guard between the row list and the aggregate: when nothing is capped, the
// summary must equal the summary of the listed rows for every viewer and
// filter. Any provider, account, date or scope predicate applied to one path
// and not the other makes them differ.
func TestIntegration_HistorySummary_MatchesListedRowsUnderTheCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	env, container := setupHistorySummaryEnv(ctx, t)
	recent := time.Now().UTC().Add(-3 * 24 * time.Hour).Truncate(24 * time.Hour)
	old := recent.Add(-100 * 24 * time.Hour)
	env.seedTenant(ctx, t, container, "a", 1, recent, old)
	env.seedTenant(ctx, t, container, "b", 1000, recent, old)
	env.addExecution(ctx, t, "", scopedUserID, "pending", "", "pending", recent.Add(60*time.Hour), false)
	env.addExecution(ctx, t, "", env.other, "failed", "boom", "failed", recent.Add(61*time.Hour), false)
	// Rows on and just outside the date window's edges.
	start := recent
	end := recent.Add(3*24*time.Hour - time.Second)
	for _, at := range []time.Time{start.Add(-time.Microsecond), start, end, end.Add(time.Microsecond)} {
		env.addExecution(ctx, t, "a", "", "failed", "edge", "failed", at, false)
	}

	scopedAuth := new(MockAuthService)
	scopedAuth.On("ValidateSession", mock.Anything, scopedToken).Return(&Session{UserID: scopedUserID}, nil).Maybe()
	scopedAuth.On("GetUser", mock.Anything, mock.Anything).Return(&User{Email: "creator@example.com"}, nil).Maybe()
	scopedAuth.grantScoped(env.tenantA.Name)
	adminAuth := new(MockAuthService)
	adminAuth.On("GetUser", mock.Anything, mock.Anything).Return(&User{Email: "creator@example.com"}, nil).Maybe()
	adminAuth.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{UserID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}, nil).Maybe()
	adminAuth.grantAdmin()
	viewers := map[string]struct {
		h     *Handler
		token string
	}{
		"scoped": {&Handler{config: env.store, auth: scopedAuth}, scopedToken},
		"admin":  {&Handler{config: env.store, auth: adminAuth}, "admin-token"},
	}
	// The date window excludes the old canceled rows, so every in-window row is
	// listed and the listed rows are the complete in-scope set.
	window := map[string]string{"start": start.Format("2006-01-02"), "end": end.Format("2006-01-02"), "limit": "1000"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range window {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	cases := map[string]map[string]string{
		"date window":       with(nil),
		"provider aws":      with(map[string]string{"provider": "aws"}),
		"provider azure":    with(map[string]string{"provider": "azure"}),
		"account A":         with(map[string]string{"account_ids": env.tenantA.ID}),
		"account B":         with(map[string]string{"account_ids": env.tenantB.ID}),
		"legacy account id": with(map[string]string{"account_id": env.tenantA.ExternalID}),
	}
	expectEmpty := map[string]bool{"scoped/provider azure": true, "admin/provider azure": true, "scoped/account B": true}
	for vname, v := range viewers {
		for cname, params := range cases {
			t.Run(vname+"/"+cname, func(t *testing.T) {
				resp, fields := getHistoryJSON(ctx, t, v.h, v.token, params)
				// Truncated is set because the terminal fetch fills its cap with
				// the old canceled rows, but those are all older than the window,
				// so every row inside the window is still listed.
				assert.Equal(t, true, fields["summary_complete"])
				if !expectEmpty[vname+"/"+cname] {
					require.NotEmpty(t, resp.Purchases, "a vacuous comparison proves nothing")
				}
				assertSummaryEqual(t, summarizePurchaseHistory(resp.Purchases), resp.Summary)
			})
		}
	}
}
