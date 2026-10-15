//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type combinedCredentials struct{ MockCredentialStore }

func (*combinedCredentials) LoadRaw(context.Context, string, string) ([]byte, error) {
	return []byte(`{"client_secret":"fixture-unused"}`), nil
}

type combinedPurchase struct {
	recommendation common.Recommendation
	options        common.PurchaseOptions
	frozen         []config.RecommendationRecord
}

type combinedService struct {
	fanoutServiceClient
	store  *config.PostgresStore
	rootID string
	mu     sync.Mutex
	calls  []combinedPurchase
}

func (s *combinedService) PurchaseCommitment(ctx context.Context, rec common.Recommendation, opts common.PurchaseOptions) (common.PurchaseResult, error) {
	row, err := s.store.GetExecutionByID(ctx, s.rootID)
	if err != nil {
		return common.PurchaseResult{}, err
	}
	if row == nil {
		return common.PurchaseResult{}, fmt.Errorf("provider boundary missing durable execution")
	}
	s.mu.Lock()
	s.calls = append(s.calls, combinedPurchase{rec, opts, append([]config.RecommendationRecord(nil), row.Recommendations...)})
	s.mu.Unlock()
	return common.PurchaseResult{Success: true, CommitmentID: "fixture-" + opts.IdempotencyToken}, nil
}

func (s *combinedService) recorded() []combinedPurchase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]combinedPurchase(nil), s.calls...)
}

func (*combinedService) GetServiceType() common.ServiceType { return common.ServiceCompute }
func (*combinedService) GetRegion() string                  { return "westeurope" }

type combinedProvider struct {
	fanoutProvider
	service *combinedService
}

func (*combinedProvider) Name() string { return "azure" }
func (*combinedProvider) GetSupportedServices() []common.ServiceType {
	return []common.ServiceType{common.ServiceCompute}
}
func (p *combinedProvider) GetServiceClient(_ context.Context, service common.ServiceType, region string) (provider.ServiceClient, error) {
	if service != common.ServiceCompute || region != "westeurope" {
		return nil, fmt.Errorf("fixture refuses unexpected service or region")
	}
	return p.service, nil
}

type combinedFactory struct{ provider *combinedProvider }

func (f combinedFactory) CreateAndValidateProvider(_ context.Context, name string, _ *provider.ProviderConfig) (provider.Provider, error) {
	if name != "azure" {
		return nil, fmt.Errorf("fixture refuses unexpected provider %q", name)
	}
	return f.provider, nil
}

type combinedPlanFixture struct {
	*createConcurrencyFixture
	manager  *purchase.Manager
	service  *combinedService
	account  string
	creator  string
	first    time.Time
	root     *config.PurchaseExecution
	notified int
}

func combinedRoute(t *testing.T, h *Handler, method, path string, body any) events.LambdaFunctionURLResponse {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	response, err := h.HandleRequest(context.Background(), &events.LambdaFunctionURLRequest{
		Headers: map[string]string{"Authorization": "Bearer admin-token", "X-CSRF-Token": "test-csrf", "Content-Type": "application/json"}, Body: string(encoded),
		RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: method, Path: path}},
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	return *response
}

func newCombinedPlanFixture(t *testing.T, automatic bool, capAmount *float64) *combinedPlanFixture {
	t.Helper()
	ctx := context.Background()
	f := &combinedPlanFixture{createConcurrencyFixture: newCreateConcurrencyFixture(ctx, t)}
	accounts, err := f.store.GetPlanAccounts(ctx, f.planID)
	require.NoError(t, err)
	f.account = accounts[0].ID
	_, err = f.pool.Exec(ctx, `UPDATE cloud_accounts SET azure_tenant_id=$1, azure_client_id=$2 WHERE id=$3`, uuid.NewString(), uuid.NewString(), f.account)
	require.NoError(t, err)
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT id FROM users WHERE email='creator@example.com'`).Scan(&f.creator))
	creatorAuth := new(MockAuthService)
	creatorAuth.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{UserID: f.creator, Email: "creator@example.com"}, nil).Maybe()
	creatorAuth.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	creatorAuth.grantAdminPurchaser()
	f.handler.auth = creatorAuth
	global, err := f.store.GetGlobalConfig(ctx)
	require.NoError(t, err)
	recipient := "fixture@example.invalid"
	global.NotificationEmail, global.AutoPurchaseEnabled, global.RequireDifferentApprover = &recipient, automatic, true
	global.PurchaseDelayHours = 0
	require.NoError(t, f.store.SaveGlobalConfig(ctx, global))
	response := combinedRoute(t, f.handler, "POST", "/api/plans", PlanRequest{
		Name: "combined Azure", Provider: "azure", Service: "compute", Payment: "monthly", Term: 1,
		TargetAccounts: []string{f.account}, TargetCoverage: 80, RampSchedule: "weekly-25pct",
		Enabled: true, AutoPurchase: automatic, NotificationDaysBefore: 3, MaxStepCommitment: capAmount,
	})
	require.Equal(t, 200, response.StatusCode, response.Body)
	var plan config.PurchasePlan
	require.NoError(t, json.Unmarshal([]byte(response.Body), &plan))
	f.planID = plan.ID
	if automatic {
		creatorAuth.AssertCalled(t, "HasPermissionAPI", mock.Anything, f.creator, auth.ActionExecute, auth.ResourcePurchases)
	}
	f.first = time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 4)
	require.GreaterOrEqual(t, time.Until(f.first), 72*time.Hour)
	require.Less(t, time.Until(f.first), 96*time.Hour)
	response = combinedRoute(t, f.handler, "POST", "/api/plans/"+f.planID+"/purchases", CreatePlannedPurchasesRequest{Count: 2, StartDate: f.first.Format("2006-01-02")})
	require.Equal(t, 200, response.StatusCode, response.Body)
	assert.JSONEq(t, `{"created":2}`, response.Body)
	monthly := 1000.0
	require.NoError(t, f.store.UpsertRecommendations(ctx, time.Now().UTC(), []config.RecommendationRecord{{
		ID: "combined-azure-rec", Provider: "azure", Service: "compute", Region: "westeurope", ResourceType: "Standard_D2s_v3",
		Count: 100, Term: 1, Payment: "monthly", MonthlyCost: &monthly, Savings: 500, CloudAccountID: &f.account,
	}}, nil))
	f.service = &combinedService{store: f.store}
	factory := combinedFactory{provider: &combinedProvider{service: f.service}}
	f.manager = purchase.NewManager(purchase.ManagerConfig{ConfigStore: f.store, EmailSender: email.NewNopSender(), CredentialStore: &combinedCredentials{}, ProviderFactory: factory})
	source := scheduler.NewScheduler(scheduler.SchedulerConfig{ConfigStore: f.store, PurchaseManager: f.manager, EmailSender: email.NewNopSender(), ProviderFactory: factory})
	f.manager.SetRecommendationSource(source)
	f.handler.purchase = f.manager
	shown, err := source.ListRecommendationsStrict(ctx, config.RecommendationFilter{AccountIDs: []string{f.account}})
	require.NoError(t, err)
	require.Len(t, shown, 1)
	assert.Equal(t, 100, shown[0].Count)
	result, err := f.manager.SendUpcomingPurchaseNotifications(ctx)
	require.NoError(t, err)
	f.notified = result.Notified
	rows := f.rows(t, 1)
	require.Len(t, rows, 1)
	f.root = &rows[0]
	f.service.rootID = f.root.ExecutionID
	return f
}

func (f *combinedPlanFixture) rows(t *testing.T, step int) []config.PurchaseExecution {
	t.Helper()
	var rows []config.PurchaseExecution
	require.NoError(t, f.store.WithTx(context.Background(), func(tx pgx.Tx) error {
		var err error
		rows, err = f.store.ListExecutionsForPlanStepTx(context.Background(), tx, f.planID, step)
		return err
	}))
	return rows
}

func (f *combinedPlanFixture) assertAttached(t *testing.T) {
	t.Helper()
	assert.Equal(t, 1, f.notified)
	assert.Equal(t, "notified", f.root.Status)
	assert.True(t, f.root.ScheduledDate.Equal(f.first))
	require.NotNil(t, f.root.CreatedByUserID)
	assert.Equal(t, f.creator, *f.root.CreatedByUserID)
	require.Len(t, f.root.Recommendations, 1)
	rec := f.root.Recommendations[0]
	assert.Equal(t, 20, rec.Count)
	require.NotNil(t, rec.MonthlyCost)
	assert.Equal(t, 200.0, *rec.MonthlyCost)
	assert.Equal(t, 2400.0, rec.TotalCommitment())
	assert.Zero(t, f.root.TotalUpfrontCost)
	assert.Equal(t, 100.0, f.root.EstimatedSavings)
	var suppressed int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT COALESCE(SUM(suppressed_count),0) FROM purchase_suppressions WHERE execution_id=$1`, f.root.ExecutionID).Scan(&suppressed))
	assert.Equal(t, 20, suppressed)
	second := f.rows(t, 2)
	require.Len(t, second, 1)
	assert.Empty(t, second[0].Recommendations)
	assert.True(t, second[0].ScheduledDate.Equal(f.first.AddDate(0, 0, 7)))
}

func (f *combinedPlanFixture) approveAs(t *testing.T, userID string, maxAmount float64) events.LambdaFunctionURLResponse {
	t.Helper()
	a := new(MockAuthService)
	a.On("ValidateSession", mock.Anything, "admin-token").Return(&Session{UserID: userID}, nil).Maybe()
	a.On("ValidateCSRFToken", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	a.grantPermissionsScoped([]auth.Permission{{Action: "approve-any", Resource: "purchases", Constraints: &auth.PermissionConstraints{AccountIDs: []string{f.account}, MaxPurchaseAmount: maxAmount}}}, []string{f.account})
	f.handler.auth = a
	return combinedRoute(t, f.handler, "POST", "/api/purchases/approve/"+f.root.ExecutionID, map[string]any{})
}

func (f *combinedPlanFixture) assertCompleted(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	row, err := f.store.GetExecutionByID(ctx, f.root.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, "completed", row.Status)
	assert.Equal(t, f.root.Recommendations, row.Recommendations)
	plan, err := f.store.GetPurchasePlan(ctx, f.planID)
	require.NoError(t, err)
	assert.Equal(t, 1, plan.RampSchedule.CurrentStep)
	require.NotNil(t, plan.NextExecutionDate)
	assert.True(t, plan.NextExecutionDate.Equal(f.first.AddDate(0, 0, 7)))
	assert.True(t, plan.RampSchedule.StartDate.Equal(f.first))
	assert.Equal(t, "pending", f.rows(t, 2)[0].Status)
	calls := f.service.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, 20, calls[0].recommendation.Count)
	assert.Equal(t, common.ProviderAzure, calls[0].recommendation.Provider)
	assert.Equal(t, common.ServiceCompute, calls[0].recommendation.Service)
	assert.Equal(t, "westeurope", calls[0].recommendation.Region)
	assert.Equal(t, "Standard_D2s_v3", calls[0].recommendation.ResourceType)
	assert.Equal(t, "monthly", calls[0].recommendation.PaymentOption)
	assert.Equal(t, "1yr", calls[0].recommendation.Term)
	assert.NotEmpty(t, calls[0].options.IdempotencyToken)
	assert.NotEqual(t, row.ExecutionID, calls[0].options.ExecutionID)
	child, err := f.store.GetExecutionByID(ctx, calls[0].options.ExecutionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", child.Status)
	require.NotNil(t, child.CloudAccountID)
	assert.Equal(t, f.account, *child.CloudAccountID)
	require.Len(t, child.Recommendations, 1)
	assert.Equal(t, row.Recommendations[0].ID, child.Recommendations[0].ID)
	assert.Equal(t, 20, child.Recommendations[0].Count)
	assert.Equal(t, 2400.0, child.Recommendations[0].TotalCommitment())
	assert.True(t, child.Recommendations[0].Purchased)
	assert.Equal(t, "fixture-"+calls[0].options.IdempotencyToken, child.Recommendations[0].PurchaseID)
	require.Len(t, calls[0].frozen, 1)
	assert.Equal(t, 2400.0, calls[0].frozen[0].TotalCommitment())
	var history int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM purchase_history WHERE plan_id=$1`, f.planID).Scan(&history))
	assert.Equal(t, 1, history)
	require.NoError(t, f.store.WithTx(ctx, func(tx pgx.Tx) error {
		baseline, readErr := f.store.GetPlanRampBaselineTx(ctx, tx, f.planID)
		if readErr != nil {
			return readErr
		}
		require.Len(t, baseline, 1)
		for _, value := range baseline {
			assert.Equal(t, 100, value.Count)
		}
		committed, readErr := f.store.ListPlanCommittedTx(ctx, tx, f.planID)
		if readErr != nil {
			return readErr
		}
		require.Len(t, committed, 1)
		for _, value := range committed {
			assert.Equal(t, 20, value.Count)
		}
		return nil
	}))
}

func TestCombinedPlanPurchase_ManualOffRoutedApproval(t *testing.T) {
	f := newCombinedPlanFixture(t, false, nil)
	f.assertAttached(t)
	// Fixture-only clock advancement reaches the due scheduler without changing the ramp anchor.
	_, err := f.pool.Exec(context.Background(), `UPDATE purchase_executions SET scheduled_date=now()-interval '1 minute' WHERE execution_id=$1`, f.root.ExecutionID)
	require.NoError(t, err)
	result, err := f.manager.ProcessScheduledPurchases(context.Background())
	require.NoError(t, err)
	assert.Zero(t, result.Executed)
	assert.Empty(t, f.service.recorded())
	_, err = f.pool.Exec(context.Background(), `UPDATE purchase_executions SET scheduled_date=$1 WHERE execution_id=$2`, f.first, f.root.ExecutionID)
	require.NoError(t, err)
	response := f.approveAs(t, f.creator, 2400)
	assert.Equal(t, 403, response.StatusCode, response.Body)
	approver := uuid.NewString()
	_, err = f.pool.Exec(context.Background(), `INSERT INTO users (id,email,password_hash,salt,active,group_ids,created_at,updated_at) SELECT $1,'approver@example.invalid','x','y',true,ARRAY[g.id],now(),now() FROM groups g ORDER BY g.name LIMIT 1`, approver)
	require.NoError(t, err)
	response = f.approveAs(t, approver, 2399)
	assert.Equal(t, 403, response.StatusCode, response.Body)
	assert.Empty(t, f.service.recorded())
	response = f.approveAs(t, approver, 2400)
	require.Equal(t, 200, response.StatusCode, response.Body)
	f.assertCompleted(t)
	response = f.approveAs(t, approver, 2400)
	assert.Equal(t, 409, response.StatusCode, response.Body)
	assert.Len(t, f.service.recorded(), 1)
	f.assertCompleted(t)
}

func TestCombinedPlanPurchase_AutomaticOnScheduledExecution(t *testing.T) {
	capAmount := 2400.0
	f := newCombinedPlanFixture(t, true, &capAmount)
	f.assertAttached(t)
	result, err := f.manager.ProcessScheduledPurchases(context.Background())
	require.NoError(t, err)
	assert.Zero(t, result.Executed)
	assert.Empty(t, f.service.recorded())
	// Fixture-only clock advancement; requested plan dates and source policy remain unchanged.
	_, err = f.pool.Exec(context.Background(), `UPDATE purchase_executions SET scheduled_date=now()-interval '1 minute' WHERE execution_id=$1`, f.root.ExecutionID)
	require.NoError(t, err)
	result, err = f.manager.ProcessScheduledPurchases(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.Executed, "%+v", result)
	f.assertCompleted(t)
	result, err = f.manager.ProcessScheduledPurchases(context.Background())
	require.NoError(t, err)
	assert.Zero(t, result.Executed)
	assert.Len(t, f.service.recorded(), 1)
	f.assertCompleted(t)
}

func TestCombinedPlanPurchase_AutomaticAttachmentOverCapRefuses(t *testing.T) {
	capAmount := 2399.0
	f := newCombinedPlanFixture(t, true, &capAmount)
	assert.Zero(t, f.notified)
	assert.Equal(t, "failed", f.root.Status)
	assert.Contains(t, f.root.Error, "exceeds max_step_commitment")
	assert.Empty(t, f.root.Recommendations)
	assert.Empty(t, f.service.recorded())
	plan, err := f.store.GetPurchasePlan(context.Background(), f.planID)
	require.NoError(t, err)
	assert.Zero(t, plan.RampSchedule.CurrentStep)
	assert.True(t, plan.NextExecutionDate.Equal(f.first))
}

func TestCombinedPlanPurchase_PlannedProjectionReportsUnits(t *testing.T) {
	f := newCombinedPlanFixture(t, false, nil)
	f.assertAttached(t)
	response := combinedRoute(t, f.handler, "GET", "/api/purchases/planned", nil)
	require.Equal(t, 200, response.StatusCode, response.Body)
	var planned PlannedPurchasesResponse
	require.NoError(t, json.Unmarshal([]byte(response.Body), &planned))
	for _, row := range planned.Purchases {
		if row.ID != f.root.ExecutionID {
			continue
		}
		assert.Equal(t, 20, row.Count, response.Body)
		assert.Equal(t, "Standard_D2s_v3", row.ResourceType, response.Body)
		assert.Equal(t, "westeurope", row.Region, response.Body)
		assert.Empty(t, f.service.recorded())
		return
	}
	t.Fatal("attached root absent from real planned-purchases API")
}
