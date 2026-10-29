package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// retryDedupeFixture is a failed execution owned by the retrying caller plus
// a handler whose store expects the successor insert and the link CAS when
// persists is true. A retry that must fail before any write passes false, so
// an attempted write hits an unregistered method and fails the test.
func retryDedupeFixture(t *testing.T, persists bool) (*config.PurchaseExecution, *Handler, *MockConfigStore) {
	t.Helper()
	creator := retryCallerID
	failed := &config.PurchaseExecution{
		ExecutionID: retryExecID, Status: "failed", CreatedByUserID: &creator, CapacityPercent: 100,
		Recommendations: []config.RecommendationRecord{{Provider: "aws", Service: "rds", Term: 1, Count: 1, UpfrontCost: 100}},
	}
	handler, mockConfig, _ := buildSessionRetryHandler(t, failed, &Session{UserID: retryCallerID}, false, true)
	if persists {
		mockConfig.On("SavePurchaseExecution", mock.Anything, mock.AnythingOfType("*config.PurchaseExecution")).Return(nil)
		mockConfig.On("LinkRetryExecutionAtomic", mock.Anything, mock.Anything, failed.ExecutionID, mock.AnythingOfType("string")).Return(true, nil)
	}
	return failed, handler, mockConfig
}

// liveSubmit is a web-source row the retrying caller submitted seconds ago
// with the given recommendations.
func liveSubmit(id string, recs []config.RecommendationRecord) config.PurchaseExecution {
	creator := retryCallerID
	return config.PurchaseExecution{
		ExecutionID: id, Status: "pending", Source: common.PurchaseSourceWeb, CreatedByUserID: &creator,
		ScheduledDate: time.Now().Add(-10 * time.Second), CapacityPercent: 100,
		Recommendations: append([]config.RecommendationRecord(nil), recs...),
	}
}

// TestHandler_retryPurchase_RefusesIdenticalLiveSubmit (issue #760): the retry
// reads the caller's recent submits under the per-creator submit lock and
// refuses with a 409 naming the existing row when one has the successor's
// fingerprint, writing no suppressions and sending no approval email. The
// real lock and race are covered by TestRetrySuccessorRacesFreshSubmit.
func TestHandler_retryPurchase_RefusesIdenticalLiveSubmit(t *testing.T) {
	failed, handler, mockConfig := retryDedupeFixture(t, true)
	notifier := &recordingEmailNotifier{}
	handler.emailNotifier = notifier
	mockConfig.On("ListRecentSubmitsTx", mock.Anything, mock.Anything, retryCallerID, mock.AnythingOfType("time.Time")).
		Return([]config.PurchaseExecution{liveSubmit("fresh-submit", failed.Recommendations)}, nil)

	result, err := handler.retryPurchase(context.Background(), sessionRetryReq(), retryExecID)

	require.Error(t, err)
	assert.Nil(t, result)
	ce, ok := IsClientError(err)
	require.True(t, ok, "a duplicate must surface as a client error, not a 500: %v", err)
	assert.Equal(t, 409, ce.code)
	assert.Contains(t, ce.message, "identical purchase")
	assert.Equal(t, "fresh-submit", ce.Details()["duplicate_execution_id"])
	mockConfig.AssertNotCalled(t, "CreateSuppressionTx")
	assert.Empty(t, notifier.captured.ApprovalToken, "no approval email for a refused retry")
}

// A recent submit with a different fingerprint does not block the retry.
func TestHandler_retryPurchase_AllowsDifferentLiveSubmit(t *testing.T) {
	failed, handler, mockConfig := retryDedupeFixture(t, true)
	other := append([]config.RecommendationRecord(nil), failed.Recommendations...)
	other[0].Count = 7
	mockConfig.On("ListRecentSubmitsTx", mock.Anything, mock.Anything, retryCallerID, mock.AnythingOfType("time.Time")).
		Return([]config.PurchaseExecution{liveSubmit("other-submit", other)}, nil)

	_, err := handler.retryPurchase(context.Background(), sessionRetryReq(), retryExecID)
	require.NoError(t, err)
}

// A failure to read the recent submits fails the retry before any write.
func TestHandler_retryPurchase_RecentSubmitsErrorFailsRetry(t *testing.T) {
	_, handler, mockConfig := retryDedupeFixture(t, false)
	mockConfig.On("ListRecentSubmitsTx", mock.Anything, mock.Anything, retryCallerID, mock.AnythingOfType("time.Time")).
		Return(nil, errors.New("db down"))

	_, err := handler.retryPurchase(context.Background(), sessionRetryReq(), retryExecID)
	require.ErrorContains(t, err, "db down")
	_, isClient := IsClientError(err)
	assert.False(t, isClient)
	mockConfig.AssertNotCalled(t, "SavePurchaseExecution")
	mockConfig.AssertNotCalled(t, "LinkRetryExecutionAtomic")
}

// TestPersistExecution_RecentSubmitsErrorIsSurfaced moves the "lookup error is
// surfaced, not swallowed" case of the removed findDuplicatePendingExecution
// test onto the live submit path (#760): a failed dedupe read must fail the
// submit, not fall through to an unchecked insert.
func TestPersistExecution_RecentSubmitsErrorIsSurfaced(t *testing.T) {
	mockConfig := new(MockConfigStore)
	t.Cleanup(func() { mockConfig.AssertExpectations(t) })
	mockConfig.On("ListRecentSubmitsTx", mock.Anything, mock.Anything, "", mock.AnythingOfType("time.Time")).
		Return(nil, errors.New("db down"))
	h := &Handler{config: mockConfig}

	dup, err := h.persistExecutionAndSuppressions(context.Background(), &config.PurchaseExecution{ExecutionID: "new"}, nil, "", "key")
	require.ErrorContains(t, err, "db down")
	assert.Nil(t, dup)
	mockConfig.AssertNotCalled(t, "SavePurchaseExecution")
}
