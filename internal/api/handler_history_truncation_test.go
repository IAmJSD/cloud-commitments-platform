package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func historyRows(n int) []config.PurchaseHistoryRecord {
	rows := make([]config.PurchaseHistoryRecord, n)
	for i := range rows {
		rows[i] = config.PurchaseHistoryRecord{PurchaseID: fmt.Sprintf("p-%d", i), Timestamp: time.Now()}
	}
	return rows
}

func pendingExecutions(n int) []config.PurchaseExecution {
	execs := make([]config.PurchaseExecution, n)
	for i := range execs {
		execs[i] = config.PurchaseExecution{
			ExecutionID:   fmt.Sprintf("e-%d", i),
			Status:        "pending",
			ScheduledDate: time.Now(),
		}
	}
	return execs
}

// historyJSON round-trips the response through JSON so the assertion sees the
// wire shape a client sees, not the Go struct.
func historyJSON(t *testing.T, resp any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func runAdminHistory(t *testing.T, completed []config.PurchaseHistoryRecord, execs []config.PurchaseExecution, params map[string]string) map[string]any {
	t.Helper()
	ctx := context.Background()
	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return(completed, nil)
	mockStore.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return(execs, nil)
	mockStore.On("ListHistoryExecutionsPage", ctx, mock.Anything).Return(execs, nil)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil).Maybe()
	mockAuth, req := adminHistoryReq(t, ctx)
	handler := &Handler{auth: mockAuth, config: mockStore}

	result, err := handler.getHistory(ctx, req, params)
	require.NoError(t, err)
	return historyJSON(t, result)
}

func TestHandler_getHistory_TruncatedWhenCompletedPastLimit(t *testing.T) {
	body := runAdminHistory(t, historyRows(config.DefaultListLimit+1), nil, map[string]string{})
	assert.Equal(t, true, body["truncated"], "an older completed row is not on this page")
	assert.Equal(t, true, body["has_more"])
	assert.Len(t, body["purchases"], config.DefaultListLimit)
	assert.EqualValues(t, config.DefaultListLimit, body["limit"])
}

// Paging reads one row past the page (issue #674), so exactly a page's worth
// of rows is no longer a false positive.
func TestHandler_getHistory_NotTruncatedWhenCompletedExactlyAtLimit(t *testing.T) {
	body := runAdminHistory(t, historyRows(config.DefaultListLimit), nil, map[string]string{})
	assert.Equal(t, false, body["truncated"])
	assert.Equal(t, false, body["has_more"])
	assert.Len(t, body["purchases"], config.DefaultListLimit)
}

func TestHandler_getHistory_TruncatedWhenExecutionsPastLimit(t *testing.T) {
	// Terminal executions share the page with completed purchases.
	body := runAdminHistory(t, nil, failedExecutionsPastLimit(), map[string]string{})
	assert.Equal(t, true, body["truncated"])
	assert.Equal(t, true, body["has_more"])
}

func failedExecutionsPastLimit() []config.PurchaseExecution {
	execs := pendingExecutions(config.DefaultListLimit + 1)
	for i := range execs {
		execs[i].Status = "failed"
	}
	return execs
}

func cleanCompletedExecutions(n int) []config.PurchaseExecution {
	execs := pendingExecutions(n)
	for i := range execs {
		execs[i].Status = "completed"
	}
	return execs
}

// Clean completed executions are skipped by the handler (and excluded by the
// store query), so a mature tenant must not see a permanent banner.
func TestHandler_getHistory_NotTruncatedByCleanCompletedExecutions(t *testing.T) {
	body := runAdminHistory(t, historyRows(3), cleanCompletedExecutions(config.DefaultListLimit), map[string]string{})
	assert.Equal(t, false, body["truncated"])
	assert.Len(t, body["purchases"], 3)
}

func TestHandler_getHistory_AuditGapCompletedExecutionsCountTowardCap(t *testing.T) {
	execs := cleanCompletedExecutions(config.DefaultListLimit + 1)
	for i := range execs {
		execs[i].Error = "history write failed"
	}
	body := runAdminHistory(t, nil, execs, map[string]string{})
	assert.Equal(t, true, body["truncated"])
	assert.Len(t, body["purchases"], config.DefaultListLimit)
}

// Terminal executions share the page with completed purchases (issue #674),
// so their cap follows the limit parameter.
func TestHandler_getHistory_ExecutionsLimitFollowsLimit(t *testing.T) {
	body := runAdminHistory(t, historyRows(2), failedExecutionsPastLimit(), map[string]string{"limit": "500"})
	assert.EqualValues(t, 500, body["limit"])
	assert.EqualValues(t, 500, body["executions_limit"])
	assert.Len(t, body["purchases"], 2+config.DefaultListLimit+1)
	assert.Equal(t, false, body["truncated"])
}

// The actionable class (pending, approved, ...) has its own, larger cap. The
// response must expose it so the truncation banner can quote the cap that was
// actually hit instead of the terminal-class cap (issue #686).
func TestHandler_getHistory_ActionableExecutionsLimitExposed(t *testing.T) {
	body := runAdminHistory(t, nil, pendingExecutions(config.MaxListLimit), map[string]string{})
	assert.Equal(t, true, body["truncated"])
	assert.EqualValues(t, config.MaxListLimit, body["actionable_executions_limit"])
	assert.EqualValues(t, config.DefaultListLimit, body["executions_limit"])
}

func TestHandler_getHistory_TruncatedHonorsCustomLimit(t *testing.T) {
	body := runAdminHistory(t, historyRows(6), nil, map[string]string{"limit": "5"})
	assert.Equal(t, true, body["truncated"])
	assert.Len(t, body["purchases"], 5)
	assert.EqualValues(t, 5, body["limit"])
}

func TestHandler_getHistory_NotTruncatedUnderLimit(t *testing.T) {
	body := runAdminHistory(t, historyRows(config.DefaultListLimit-1), pendingExecutions(config.DefaultListLimit-1), map[string]string{})
	assert.Equal(t, false, body["truncated"])
	assert.EqualValues(t, config.DefaultListLimit, body["limit"])
}

// A scoped user's visible rows can be few while other tenants' rows fill the
// page many times over. The page is filled with rows the caller may see and
// has_more is judged on those too (issue #674), so other tenants' volume
// changes neither flag, and no count is exposed.
func TestHandler_getHistory_ScopedUserTruncationLeaksNoCounts(t *testing.T) {
	ctx := context.Background()
	const scopedUserID = "scoped-user-id"
	own := scopedUserID
	execs := failedExecutionsPastLimit()
	execs[len(execs)-1].CreatedByUserID = &own

	mockStore := new(MockConfigStore)
	t.Cleanup(func() { mockStore.AssertExpectations(t) })
	mockStore.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseHistoryRecord{}, nil)
	mockStore.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return(execs, nil)
	mockStore.On("ListHistoryExecutionsPage", ctx, mock.Anything).Return(execs, nil)
	mockStore.On("GetGlobalConfig", ctx).Return(&config.GlobalConfig{}, nil).Maybe()
	mockStore.ListCloudAccountsFn = func(_ context.Context, _ config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return nil, nil
	}
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "scoped-token").Return(&Session{UserID: scopedUserID, Email: "s@example.com"}, nil)
	mockAuth.On("HasPermissionAPI", ctx, scopedUserID, "view", "purchases").Return(true, nil)
	mockAuth.On("GetAllowedAccountsAPI", ctx, scopedUserID).Return([]string{"aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"}, nil)
	mockAuth.On("GetUser", ctx, mock.Anything).Return(&User{Email: "s@example.com"}, nil).Maybe()
	handler := &Handler{auth: mockAuth, config: mockStore}
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer scoped-token"}}

	result, err := handler.getHistory(ctx, req, map[string]string{})
	require.NoError(t, err)
	body := historyJSON(t, result)

	assert.Len(t, body["purchases"], 1, "scoped user sees only their own row")
	assert.Equal(t, false, body["truncated"], "other tenants' rows are not missing rows")
	assert.Equal(t, false, body["has_more"])
	assert.NotContains(t, body, "next_cursor")
	assert.NotContains(t, body, "total", "no count of rows outside the caller's scope may be exposed")
	summary, ok := body["summary"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 1, summary["total_purchases"])
}
