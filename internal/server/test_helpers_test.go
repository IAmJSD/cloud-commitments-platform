package server

import (
	"context"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/mocks"
	"github.com/jackc/pgx/v5"
)

// databaseConfigStub is a type alias used in health tests to simulate pending DB config.
type databaseConfigStub = database.Config

// mockConfigStoreForHealth implements config.StoreInterface for health check
// tests. It embeds mocks.MockConfigStore; the hand-written stubs below are
// being removed in favour of the embedded defaults (issue #76).
type mockConfigStoreForHealth struct {
	mocks.MockConfigStore
}

func (m *mockConfigStoreForHealth) CleanupOldExecutions(ctx context.Context, retentionDays int) (int64, error) {
	return 0, nil
}

func (m *mockConfigStoreForHealth) GetStaleProcessingExchanges(ctx context.Context, olderThan time.Duration) ([]config.RIExchangeRecord, error) {
	return nil, nil
}

func (m *mockConfigStoreForHealth) CreateCloudAccount(ctx context.Context, account *config.CloudAccount) error {
	return nil
}
func (m *mockConfigStoreForHealth) GetCloudAccount(ctx context.Context, id string) (*config.CloudAccount, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) GetCloudAccountByExternalID(ctx context.Context, provider, externalID string) (*config.CloudAccount, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) UpdateCloudAccount(ctx context.Context, account *config.CloudAccount) error {
	return nil
}
func (m *mockConfigStoreForHealth) DeleteCloudAccount(ctx context.Context, id string) error {
	return nil
}
func (m *mockConfigStoreForHealth) ListCloudAccounts(ctx context.Context, filter config.CloudAccountFilter) ([]config.CloudAccount, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) SaveAccountCredential(ctx context.Context, accountID, credentialType, encryptedBlob string) error {
	return nil
}
func (m *mockConfigStoreForHealth) GetAccountCredential(ctx context.Context, accountID, credentialType string) (string, error) {
	return "", nil
}
func (m *mockConfigStoreForHealth) DeleteAccountCredentials(ctx context.Context, accountID string) error {
	return nil
}
func (m *mockConfigStoreForHealth) HasAccountCredentials(ctx context.Context, accountID string) (bool, error) {
	return false, nil
}
func (m *mockConfigStoreForHealth) GetAccountServiceOverride(ctx context.Context, accountID, provider, service string) (*config.AccountServiceOverride, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) SaveAccountServiceOverride(ctx context.Context, override *config.AccountServiceOverride) error {
	return nil
}
func (m *mockConfigStoreForHealth) DeleteAccountServiceOverride(ctx context.Context, accountID, provider, service string) error {
	return nil
}
func (m *mockConfigStoreForHealth) ListAccountServiceOverrides(ctx context.Context, accountID string) ([]config.AccountServiceOverride, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) SetPlanAccounts(ctx context.Context, planID string, accountIDs []string) error {
	return nil
}
func (m *mockConfigStoreForHealth) GetPlanAccounts(ctx context.Context, planID string) ([]config.CloudAccount, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) CreateAccountRegistration(_ context.Context, _ *config.AccountRegistration) error {
	return nil
}
func (m *mockConfigStoreForHealth) GetAccountRegistration(_ context.Context, _ string) (*config.AccountRegistration, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) GetAccountRegistrationByToken(_ context.Context, _ string) (*config.AccountRegistration, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) ListAccountRegistrations(_ context.Context, _ config.AccountRegistrationFilter) ([]config.AccountRegistration, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) UpdateAccountRegistration(_ context.Context, _ *config.AccountRegistration) error {
	return nil
}
func (m *mockConfigStoreForHealth) TransitionRegistrationStatus(_ context.Context, _ *config.AccountRegistration, _ string, _ *string) error {
	return nil
}
func (m *mockConfigStoreForHealth) DeleteAccountRegistration(_ context.Context, _ string) error {
	return nil
}
func (m *mockConfigStoreForHealth) UpsertRecommendations(_ context.Context, _ time.Time, _ []config.RecommendationRecord, _ []config.SuccessfulCollect) error {
	return nil
}
func (m *mockConfigStoreForHealth) ListStoredRecommendations(_ context.Context, _ config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) GetRecommendationsFreshness(_ context.Context) (*config.RecommendationsFreshness, error) {
	return &config.RecommendationsFreshness{}, nil
}
func (m *mockConfigStoreForHealth) SetRecommendationsCollectionError(_ context.Context, _ string) error {
	return nil
}
func (m *mockConfigStoreForHealth) SetRecommendationsCollectionPartial(_ context.Context, _ *config.RecommendationsCollectionPartial) error {
	return nil
}
func (m *mockConfigStoreForHealth) MarkCollectionStarted(_ context.Context) (string, bool, error) {
	return "mock-owner-token", true, nil
}
func (m *mockConfigStoreForHealth) ClearCollectionStarted(_ context.Context, _ string) error {
	return nil
}
func (m *mockConfigStoreForHealth) ClaimCollectionInvocation(_ context.Context, _ string) (string, bool, error) {
	return mocks.MockInvocationToken, true, nil
}
func (m *mockConfigStoreForHealth) GetRIUtilizationCache(_ context.Context, _ string, _ int) (*config.RIUtilizationCacheEntry, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) UpsertRIUtilizationCache(_ context.Context, _ string, _ int, _ []byte, _ time.Time) error {
	return nil
}

// ── Purchase suppressions (Commit 2 of bulk-purchase-with-grace).
func (m *mockConfigStoreForHealth) CreateSuppression(_ context.Context, _ *config.PurchaseSuppression) error {
	return nil
}
func (m *mockConfigStoreForHealth) CreateSuppressionTx(_ context.Context, _ pgx.Tx, _ *config.PurchaseSuppression) error {
	return nil
}
func (m *mockConfigStoreForHealth) DeleteSuppressionsByExecution(_ context.Context, _ string) error {
	return nil
}
func (m *mockConfigStoreForHealth) DeleteSuppressionsByExecutionTx(_ context.Context, _ pgx.Tx, _ string) error {
	return nil
}
func (m *mockConfigStoreForHealth) ListActiveSuppressions(_ context.Context) ([]config.PurchaseSuppression, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) SavePurchaseExecutionTx(ctx context.Context, _ pgx.Tx, e *config.PurchaseExecution) error {
	return m.SavePurchaseExecution(ctx, e)
}
func (m *mockConfigStoreForHealth) SaveExecutionIfStatus(ctx context.Context, e *config.PurchaseExecution, _ []string) error {
	return m.SavePurchaseExecution(ctx, e)
}
func (m *mockConfigStoreForHealth) SaveExecutionIfStatusTx(ctx context.Context, _ pgx.Tx, e *config.PurchaseExecution, _ []string) error {
	return m.SavePurchaseExecution(ctx, e)
}
func (m *mockConfigStoreForHealth) GetExecutionFanOutShape(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (m *mockConfigStoreForHealth) PinExecutionFanOutShape(_ context.Context, _ string, shape string) (string, error) {
	return shape, nil
}
func (m *mockConfigStoreForHealth) RecoverReapedExecution(ctx context.Context, e *config.PurchaseExecution) error {
	return m.SavePurchaseExecution(ctx, e)
}
func (m *mockConfigStoreForHealth) RecoverReapedExecutionTx(ctx context.Context, _ pgx.Tx, e *config.PurchaseExecution) error {
	return m.SavePurchaseExecution(ctx, e)
}
func (m *mockConfigStoreForHealth) ListRecentSubmitsTx(ctx context.Context, _ pgx.Tx, _ string, _ time.Time) ([]config.PurchaseExecution, error) {
	return m.GetPendingExecutions(ctx)
}
func (m *mockConfigStoreForHealth) WithTx(_ context.Context, fn func(tx pgx.Tx) error) error {
	return fn(nil)
}

func (m *mockConfigStoreForHealth) GetPurchaseHistoryByPurchaseID(_ context.Context, _ string) (*config.PurchaseHistoryRecord, error) {
	return nil, nil
}

func (m *mockConfigStoreForHealth) MarkPurchaseRevoked(_ context.Context, _ string, _ time.Time, _ string, _ string, _ *float64, _ string) error {
	return nil
}

func (m *mockConfigStoreForHealth) FlipPurchaseRevocationInFlight(_ context.Context, _ string) error {
	return nil
}

func (m *mockConfigStoreForHealth) ClearRevocationInFlight(_ context.Context, _ string) error {
	return nil
}

func (m *mockConfigStoreForHealth) GetPurchaseHistoryInFlight(_ context.Context) ([]*config.PurchaseHistoryRecord, error) {
	return nil, nil
}

func (m *mockConfigStoreForHealth) GetScheduledExecutionsDue(_ context.Context) ([]config.PurchaseExecution, error) {
	return nil, nil
}

func (m *mockConfigStoreForHealth) GetLadderConfigs(_ context.Context) ([]config.LadderConfigDB, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) GetLadderConfig(_ context.Context, _, _ string) (*config.LadderConfigDB, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) UpsertLadderConfig(_ context.Context, cfg *config.LadderConfigDB) (*config.LadderConfigDB, error) {
	return cfg, nil
}
func (m *mockConfigStoreForHealth) SaveLadderRun(_ context.Context, run *config.LadderRunDB) (*config.LadderRunDB, error) {
	return run, nil
}
func (m *mockConfigStoreForHealth) PlanAndSaveLadderRun(_ context.Context, _ string, _ time.Time, build config.LadderPlanBuilder) (*config.LadderRunDB, error) {
	run, _, err := build(nil)
	return run, err
}
func (m *mockConfigStoreForHealth) GetLadderRun(_ context.Context, _ string) (*config.LadderRunDB, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) SaveLadderTranches(_ context.Context, _ []config.LadderTrancheDB) error {
	return nil
}
func (m *mockConfigStoreForHealth) LatestLadderRunStartedAt(_ context.Context, _ string) (*time.Time, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) TransitionLadderRunStatus(_ context.Context, _ string, _ []ladder.RunStatus, _ ladder.RunStatus) (*config.LadderRunDB, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) CancelLadderRun(_ context.Context, _ string, _ []ladder.RunStatus, _ string) (*config.LadderRunDB, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) UpdateGlobalConfigAtomic(_ context.Context, apply func(*config.GlobalConfig) error) (*config.GlobalConfig, error) {
	cfg := &config.GlobalConfig{}
	if err := apply(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
func (m *mockConfigStoreForHealth) UpdatePurchaseHistoryListing(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *mockConfigStoreForHealth) GetMarketplaceListingAttempt(_ context.Context, _ string) (*config.MarketplaceListingAttempt, error) {
	return nil, nil
}
func (m *mockConfigStoreForHealth) ResolveMarketplaceListingAttempt(_ context.Context, _ string, _ config.MarketplaceListingAttempt, _, _ string) (bool, error) {
	return false, nil
}
func (m *mockConfigStoreForHealth) ClaimMarketplaceListingSlot(_ context.Context, _, _ string, _ []byte) (*config.MarketplaceListingClaim, error) {
	return &config.MarketplaceListingClaim{}, nil
}

func (m *mockConfigStoreForHealth) ReleaseMarketplaceListingClaim(_ context.Context, _, _, _ string, _ bool) error {
	return nil
}

func (m *mockConfigStoreForHealth) ClaimRIExchangeIdempotencyKey(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

func (m *mockConfigStoreForHealth) StampOfferingClass(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockConfigStoreForHealth) UpsertNotificationMute(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockConfigStoreForHealth) IsNotificationMuted(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (m *mockConfigStoreForHealth) ListExecutionsForPlanStepTx(_ context.Context, _ pgx.Tx, _ string, _ int) ([]config.PurchaseExecution, error) {
	return nil, nil
}

func (m *mockConfigStoreForHealth) SetPlanRampBaselineTx(_ context.Context, _ pgx.Tx, _ string, _ config.PlanRampBaseline) (bool, error) {
	return false, nil
}

func (m *mockConfigStoreForHealth) GetPlanRampBaselineTx(_ context.Context, _ pgx.Tx, _ string) (config.PlanRampBaseline, error) {
	return nil, nil
}

func (m *mockConfigStoreForHealth) ListPlanCommittedTx(_ context.Context, _ pgx.Tx, _ string) (map[string]config.PlanRampQuantity, error) {
	return nil, nil
}

func (m *mockConfigStoreForHealth) CompleteTargetMetStepTx(_ context.Context, _ pgx.Tx, _ string, _ int, _, _ string) error {
	return nil
}

func (m *mockConfigStoreForHealth) SetExecutionRecommendationsIfEmptyTx(_ context.Context, _ pgx.Tx, _ string, _ []config.RecommendationRecord, _, _ float64) (bool, error) {
	return false, nil
}

func (m *mockConfigStoreForHealth) ExpireExecutionAtomic(_ context.Context, _ pgx.Tx, _ string) (bool, error) {
	return false, nil
}
