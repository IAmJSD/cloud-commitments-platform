package testutil

import (
	"context"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/purchase"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/scheduler"
)

// MockScheduler is a mock implementation of server.SchedulerInterface.
type MockScheduler struct {
	CollectRecommendationsFunc func(ctx context.Context, ownerToken string) (*scheduler.CollectResult, error)
	ListRecommendationsFunc    func(ctx context.Context, filter config.RecommendationFilter) ([]config.RecommendationRecord, error)
	GetRecommendationByIDFunc  func(ctx context.Context, id string) (*config.RecommendationRecord, []string, error)
	ScanRecommendationsFunc    func(ctx context.Context, filter config.RecommendationFilter, chunkSize int, fn func([]config.RecommendationRecord) error) error
}

func (m *MockScheduler) CollectRecommendations(ctx context.Context, ownerToken string) (*scheduler.CollectResult, error) {
	if m.CollectRecommendationsFunc != nil {
		return m.CollectRecommendationsFunc(ctx, ownerToken)
	}
	return &scheduler.CollectResult{}, nil
}

func (m *MockScheduler) ListRecommendations(ctx context.Context, filter config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	if m.ListRecommendationsFunc != nil {
		return m.ListRecommendationsFunc(ctx, filter)
	}
	return []config.RecommendationRecord{}, nil
}

// ScanRecommendations defaults to an empty cache: fn is never called.
func (m *MockScheduler) ScanRecommendations(ctx context.Context, filter config.RecommendationFilter, chunkSize int, fn func([]config.RecommendationRecord) error) error {
	if m.ScanRecommendationsFunc != nil {
		return m.ScanRecommendationsFunc(ctx, filter, chunkSize, fn)
	}
	return nil
}

func (m *MockScheduler) GetRecommendationByID(ctx context.Context, id string) (*config.RecommendationRecord, []string, error) {
	if m.GetRecommendationByIDFunc != nil {
		return m.GetRecommendationByIDFunc(ctx, id)
	}
	return nil, nil, nil
}

// MockPurchaseManager is a mock implementation of server.PurchaseManagerInterface.
type MockPurchaseManager struct {
	ProcessScheduledPurchasesFunc         func(ctx context.Context) (*purchase.ProcessResult, error)
	SendUpcomingPurchaseNotificationsFunc func(ctx context.Context) (*purchase.NotificationResult, error)
	ProcessMessageFunc                    func(ctx context.Context, body string) error
	ApproveExecutionFunc                  func(ctx context.Context, execID, token, actor string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error)
	ApproveAndExecuteFunc                 func(ctx context.Context, execID, actor string, transitionedBy *string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error)
	DirectExecuteFunc                     func(ctx context.Context, execID, actor string, transitionedBy *string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error)
	RunPlannedPurchaseNowFunc             func(ctx context.Context, execID, actor string, transitionedBy *string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error)
	CancelExecutionFunc                   func(ctx context.Context, execID, token, actor string) error
	ReapStuckExecutionsFunc               func(ctx context.Context, reapAfter time.Duration) (*purchase.ReapResult, error)
	FireScheduledDelayedPurchasesFunc     func(ctx context.Context) (*purchase.FireResult, error)
	FinalizeInFlightRevocationsFunc       func(ctx context.Context) (*purchase.FinalizeResult, error)
}

func (m *MockPurchaseManager) ProcessScheduledPurchases(ctx context.Context) (*purchase.ProcessResult, error) {
	if m.ProcessScheduledPurchasesFunc != nil {
		return m.ProcessScheduledPurchasesFunc(ctx)
	}
	return &purchase.ProcessResult{}, nil
}

func (m *MockPurchaseManager) SendUpcomingPurchaseNotifications(ctx context.Context) (*purchase.NotificationResult, error) {
	if m.SendUpcomingPurchaseNotificationsFunc != nil {
		return m.SendUpcomingPurchaseNotificationsFunc(ctx)
	}
	return &purchase.NotificationResult{}, nil
}

func (m *MockPurchaseManager) ProcessMessage(ctx context.Context, body string) error {
	if m.ProcessMessageFunc != nil {
		return m.ProcessMessageFunc(ctx, body)
	}
	return nil
}

func (m *MockPurchaseManager) ApproveExecution(ctx context.Context, execID, token, actor string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error) {
	if m.ApproveExecutionFunc != nil {
		return m.ApproveExecutionFunc(ctx, execID, token, actor, check)
	}
	return nil, "", nil
}

func (m *MockPurchaseManager) ApproveAndExecute(ctx context.Context, execID, actor string, transitionedBy *string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error) {
	if m.ApproveAndExecuteFunc != nil {
		return m.ApproveAndExecuteFunc(ctx, execID, actor, transitionedBy, check)
	}
	return nil, "", nil
}

func (m *MockPurchaseManager) DirectExecute(ctx context.Context, execID, actor string, transitionedBy *string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error) {
	if m.DirectExecuteFunc != nil {
		return m.DirectExecuteFunc(ctx, execID, actor, transitionedBy, check)
	}
	return nil, "", nil
}

func (m *MockPurchaseManager) RunPlannedPurchaseNow(ctx context.Context, execID, actor string, transitionedBy *string, check purchase.ClaimedRowCheck) (*config.PurchaseExecution, string, error) {
	if m.RunPlannedPurchaseNowFunc != nil {
		return m.RunPlannedPurchaseNowFunc(ctx, execID, actor, transitionedBy, check)
	}
	return nil, "", nil
}

func (m *MockPurchaseManager) CancelExecution(ctx context.Context, execID, token, actor string) error {
	if m.CancelExecutionFunc != nil {
		return m.CancelExecutionFunc(ctx, execID, token, actor)
	}
	return nil
}

func (m *MockPurchaseManager) ReapStuckExecutions(ctx context.Context, reapAfter time.Duration) (*purchase.ReapResult, error) {
	if m.ReapStuckExecutionsFunc != nil {
		return m.ReapStuckExecutionsFunc(ctx, reapAfter)
	}
	return &purchase.ReapResult{}, nil
}

func (m *MockPurchaseManager) FireScheduledDelayedPurchases(ctx context.Context) (*purchase.FireResult, error) {
	if m.FireScheduledDelayedPurchasesFunc != nil {
		return m.FireScheduledDelayedPurchasesFunc(ctx)
	}
	return &purchase.FireResult{}, nil
}

func (m *MockPurchaseManager) FinalizeInFlightRevocations(ctx context.Context) (*purchase.FinalizeResult, error) {
	if m.FinalizeInFlightRevocationsFunc != nil {
		return m.FinalizeInFlightRevocationsFunc(ctx)
	}
	return &purchase.FinalizeResult{}, nil
}
