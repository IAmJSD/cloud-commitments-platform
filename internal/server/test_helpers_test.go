package server

import (
	"context"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/mocks"
)

// databaseConfigStub is a type alias used in health tests to simulate pending DB config.
type databaseConfigStub = database.Config

// mockConfigStoreForHealth is the config.StoreInterface double for the server
// package's health, readiness and handler tests. It embeds the shared
// mocks.MockConfigStore instead of stubbing every store method by hand, so a
// new StoreInterface method needs no change here (issue #76). Override a
// method on this type only when a test here needs a default that differs
// from MockConfigStore's.
type mockConfigStoreForHealth struct {
	mocks.MockConfigStore
}

// CleanupOldExecutions reports nothing cleaned. MockConfigStore requires an
// expectation for it, and the scheduled-cleanup handler tests call it
// without one.
func (m *mockConfigStoreForHealth) CleanupOldExecutions(ctx context.Context, retentionDays int) (int64, error) {
	return 0, nil
}

// GetStaleProcessingExchanges reports no stuck exchanges. MockConfigStore
// requires an expectation for it, and the scheduled RI-exchange tests reach it
// through alarmStaleRIExchanges without one.
func (m *mockConfigStoreForHealth) GetStaleProcessingExchanges(ctx context.Context, olderThan time.Duration) ([]config.RIExchangeRecord, error) {
	return nil, nil
}
