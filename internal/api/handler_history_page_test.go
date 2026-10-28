package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// noCursor is the after argument of a first-page ListPurchaseHistoryPage read.
var noCursor = (*config.HistoryPageKey)(nil)

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// pageUUID returns a distinct canonical UUID per (group, i).
func pageUUID(group, i int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", group, i)
}

// streamFixture is one paged-stream row of a test fixture: its key and the
// purchase_id the response carries for it.
type streamFixture struct {
	key config.HistoryPageKey
	id  string
}

func purchaseFixture(id, account string, ts time.Time, rowID string) (config.PurchaseHistoryPageRow, streamFixture) {
	row := config.PurchaseHistoryPageRow{
		PurchaseHistoryRecord: config.PurchaseHistoryRecord{PurchaseID: id, AccountID: account, Provider: "aws", Timestamp: ts},
		RowID:                 rowID,
	}
	return row, streamFixture{key: row.Key(), id: id}
}

func executionFixture(execID, status string, account *string, ts time.Time) (config.PurchaseExecution, streamFixture) {
	exec := config.PurchaseExecution{ExecutionID: execID, Status: status, ScheduledDate: ts, CloudAccountID: account, Error: "boom"}
	key := config.HistoryPageKey{Timestamp: ts, Source: config.HistorySourceExecution, ID: execID}
	return exec, streamFixture{key: key, id: execID}
}

func sortedStreamIDs(rows []streamFixture) []string {
	sorted := slices.Clone(rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].key.SortsBefore(sorted[j].key) })
	ids := make([]string, len(sorted))
	for i := range sorted {
		ids[i] = sorted[i].id
	}
	return ids
}

// walkHistoryPages requests pages until has_more is false and returns them.
func walkHistoryPages(t *testing.T, h *Handler, req *events.LambdaFunctionURLRequest, params map[string]string) []HistoryResponse {
	t.Helper()
	var pages []HistoryResponse
	cursor := ""
	for {
		p := map[string]string{}
		for k, v := range params {
			p[k] = v
		}
		if cursor != "" {
			p["cursor"] = cursor
		}
		out, err := h.getHistory(context.Background(), req, p)
		require.NoError(t, err)
		resp := out.(HistoryResponse)
		pages = append(pages, resp)
		assert.Equal(t, resp.HasMore, resp.NextCursor != "", "next_cursor is set exactly when has_more is")
		if !resp.HasMore {
			return pages
		}
		require.Less(t, len(pages), 100, "paging must terminate")
		cursor = resp.NextCursor
	}
}

// TestHandler_getHistory_PagesThroughEveryRowOnce pages a stream of
// completed purchases and terminal executions that share timestamps within
// and across the two sources. Every row must come back exactly once, in
// stream order, and the pending approval, older than every other row, must be
// on the first page (an offset over one merged list would put it last).
func TestHandler_getHistory_PagesThroughEveryRowOnce(t *testing.T) {
	ctx := context.Background()
	// Recent, so the pending row is not stale and no expiry sweep runs.
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	var purchases []config.PurchaseHistoryPageRow
	var execs []config.PurchaseExecution
	var stream []streamFixture
	for i := 0; i < 9; i++ {
		row, f := purchaseFixture(fmt.Sprintf("p-%d", i), "acct", base.Add(-time.Duration(i/3)*time.Hour), pageUUID(0xa, i))
		purchases = append(purchases, row)
		stream = append(stream, f)
	}
	for i := 0; i < 8; i++ {
		exec, f := executionFixture(pageUUID(0xe, i), "failed", nil, base.Add(-time.Duration(i/3)*time.Hour))
		execs = append(execs, exec)
		stream = append(stream, f)
	}
	pending := config.PurchaseExecution{ExecutionID: pageUUID(0x9, 0), Status: "pending", ScheduledDate: base.Add(-24 * time.Hour)}
	execs = append(execs, pending)

	store := new(MockConfigStore)
	store.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return(purchases, nil)
	store.On("ListHistoryExecutionsPage", ctx, mock.Anything).Return(execs, nil)
	store.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return(execs, nil)
	mockAuth, req := adminHistoryReq(ctx)
	h := &Handler{auth: mockAuth, config: store}

	pages := walkHistoryPages(t, h, req, map[string]string{"limit": "4"})

	require.Len(t, pages, 5, "17 stream rows at 4 per page")
	var got []string
	for n, page := range pages {
		pendingSeen := false
		var streamRows int
		for _, row := range page.Purchases {
			if row.PurchaseID == pending.ExecutionID {
				pendingSeen = true
				continue
			}
			got = append(got, row.PurchaseID)
			streamRows++
		}
		assert.Equal(t, n == 0, pendingSeen, "page %d: the pending approval is listed on the first page only", n)
		assert.Equal(t, page.HasMore, page.Truncated, "page %d: nothing failed, so truncated only means more pages", n)
		if page.HasMore {
			assert.Equal(t, 4, streamRows, "page %d is full", n)
		}
	}
	assert.Equal(t, sortedStreamIDs(stream), got, "every row exactly once, in stream order")
}

// TestHandler_getHistory_ScopedPagingStaysInScope pages as a user scoped to
// account A through a stream where account B's rows are newest, interleaved
// and last. The user must get every account-A row and their own ambient row
// exactly once and nothing else; no cursor may name a hidden row; and the
// last page must say has_more=false although account-B rows follow it.
func TestHandler_getHistory_ScopedPagingStaysInScope(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	accA, accB := permsAccA, permsAccB
	own, other := permsScopedUserID, "dddddddd-dddd-4ddd-dddd-dddddddddddd"
	var purchases []config.PurchaseHistoryPageRow
	var execs []config.PurchaseExecution
	var visible []streamFixture
	hidden := map[string]bool{}
	addPurchase := func(id, account string, ts time.Time, n int) {
		row, f := purchaseFixture(id, account, ts, pageUUID(0xa, n))
		purchases = append(purchases, row)
		if account == accA {
			visible = append(visible, f)
		} else {
			hidden[f.key.ID] = true
		}
	}
	// 150 newer account-B rows: the first page needs two store batches.
	for i := 0; i < 150; i++ {
		addPurchase(fmt.Sprintf("b-new-%d", i), accB, base.Add(time.Duration(150-i)*time.Minute), i)
	}
	// 8 purchases + 3 executions + 1 ambient row = 12 visible rows: the last
	// page is full and only hidden rows follow it.
	for i := 0; i < 8; i++ {
		addPurchase(fmt.Sprintf("a-%d", i), accA, base.Add(-time.Duration(i)*time.Hour), 200+i)
		addPurchase(fmt.Sprintf("b-mid-%d", i), accB, base.Add(-time.Duration(i)*time.Hour), 300+i)
	}
	for i := 0; i < 30; i++ {
		addPurchase(fmt.Sprintf("b-old-%d", i), accB, base.Add(-time.Duration(100+i)*time.Hour), 400+i)
	}
	for i := 0; i < 3; i++ {
		exec, f := executionFixture(pageUUID(0xe, i), "failed", &accA, base.Add(-time.Duration(2*i)*time.Hour))
		execs = append(execs, exec)
		visible = append(visible, f)
		execB, fB := executionFixture(pageUUID(0xf, i), "failed", &accB, base.Add(-time.Duration(2*i)*time.Hour))
		execs = append(execs, execB)
		hidden[fB.key.ID] = true
	}
	ownAmbient, fOwn := executionFixture(pageUUID(0x1, 0), "expired", nil, base.Add(-90*time.Hour))
	ownAmbient.CreatedByUserID = &own
	otherAmbient, fOther := executionFixture(pageUUID(0x1, 1), "expired", nil, base.Add(-91*time.Hour))
	otherAmbient.CreatedByUserID = &other
	execs = append(execs, ownAmbient, otherAmbient)
	visible = append(visible, fOwn)
	hidden[fOther.key.ID] = true

	store := new(MockConfigStore)
	store.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return(purchases, nil)
	store.On("ListHistoryExecutionsPage", ctx, mock.Anything).Return(execs, nil)
	store.On("GetExecutionsByStatuses", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseExecution{}, nil)
	store.ListCloudAccountsFn = func(context.Context, config.CloudAccountFilter) ([]config.CloudAccount, error) {
		return permsAccountList(), nil
	}
	auth := scopedAuthMock(ctx)
	auth.On("GetUser", ctx, mock.Anything).Return(&User{Email: "creator@example.com"}, nil).Maybe()
	h := &Handler{auth: auth, config: store}

	pages := walkHistoryPages(t, h, scopedReq(), map[string]string{"limit": "3"})

	var got []string
	for n, page := range pages {
		for _, row := range page.Purchases {
			got = append(got, row.PurchaseID)
		}
		if page.HasMore {
			key, err := decodeHistoryCursor(page.NextCursor)
			require.NoError(t, err)
			assert.False(t, hidden[key.ID], "page %d: next_cursor names a row the caller cannot see", n)
		}
	}
	assert.Equal(t, sortedStreamIDs(visible), got, "every in-scope row exactly once, in order, and no other row")
	require.Len(t, visible, 12)
	assert.Len(t, pages, 4, "no trailing empty page")
	last := pages[len(pages)-1]
	assert.False(t, last.HasMore, "out-of-scope rows after the last visible row must not set has_more")
	assert.False(t, last.Truncated)
}

// TestHandler_getHistory_CursorPageOmitsActionableRows checks that a page
// fetched with a cursor neither lists nor sweeps actionable executions: they
// were all on the first page.
func TestHandler_getHistory_CursorPageOmitsActionableRows(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var purchases []config.PurchaseHistoryPageRow
	for i := 0; i < 3; i++ {
		row, _ := purchaseFixture(fmt.Sprintf("p-%d", i), "acct", base.Add(-time.Duration(i)*time.Hour), pageUUID(0xa, i))
		purchases = append(purchases, row)
	}
	store := new(MockConfigStore)
	store.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return(purchases, nil)
	mockAuth, req := adminHistoryReq(ctx)
	h := &Handler{auth: mockAuth, config: store, isLambda: true}

	cursor := encodeHistoryCursor(purchases[0].Key())
	out, err := h.getHistory(ctx, req, map[string]string{"cursor": cursor, "limit": "1"})
	require.NoError(t, err)
	resp := out.(HistoryResponse)

	require.Len(t, resp.Purchases, 1)
	assert.Equal(t, "p-1", resp.Purchases[0].PurchaseID)
	assert.True(t, resp.HasMore)
	store.AssertNotCalled(t, "GetExecutionsByStatuses", mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "ExpireExecutionAtomic", mock.Anything, mock.Anything, mock.Anything)
	store.AssertCalled(t, "ListPurchaseHistoryPage", ctx, mock.Anything, mock.MatchedBy(func(after *config.HistoryPageKey) bool {
		return after != nil && *after == purchases[0].Key()
	}))
}

// TestHandler_getHistory_TerminalExecutionsFailureFlagsTruncated keeps the
// issue #686 signal on paged reads: when terminal executions cannot be read
// the purchases still page, and the response says rows are missing.
func TestHandler_getHistory_TerminalExecutionsFailureFlagsTruncated(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	row, _ := purchaseFixture("p-0", "acct", base, pageUUID(0xa, 0))
	store := new(MockConfigStore)
	store.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return([]config.PurchaseHistoryPageRow{row}, nil)
	store.On("ListHistoryExecutionsPage", ctx, mock.Anything).Return(nil, errors.New("db down"))
	mockAuth, req := adminHistoryReq(ctx)
	h := &Handler{auth: mockAuth, config: store}

	cursor := encodeHistoryCursor(config.HistoryPageKey{Timestamp: base.Add(time.Hour), Source: config.HistorySourcePurchase, ID: pageUUID(0xa, 9)})
	out, err := h.getHistory(ctx, req, map[string]string{"cursor": cursor})
	require.NoError(t, err)
	resp := out.(HistoryResponse)
	require.Len(t, resp.Purchases, 1)
	assert.False(t, resp.HasMore)
	assert.True(t, resp.Truncated)
}

// TestHandler_getHistory_PurchaseHistoryFailureFailsRequest keeps the
// pre-paging contract: a purchase_history read error is a server error.
func TestHandler_getHistory_PurchaseHistoryFailureFailsRequest(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("ListPurchaseHistoryPage", ctx, mock.Anything, mock.Anything).Return(nil, errors.New("db down"))
	mockAuth, req := adminHistoryReq(ctx)
	h := &Handler{auth: mockAuth, config: store}

	_, err := h.getHistory(ctx, req, map[string]string{})
	require.Error(t, err)
	_, isClient := IsClientError(err)
	assert.False(t, isClient)
}

func TestHistoryStreamSource_RefusesAKeysetReadThatDoesNotAdvance(t *testing.T) {
	cursor := config.HistoryPageKey{Timestamp: time.Unix(100, 0), Source: config.HistorySourcePurchase, ID: pageUUID(1, 1)}
	src := historyStreamSource{
		after: &cursor,
		batch: 2,
		fetch: func(context.Context, *config.HistoryPageKey) ([]historyStreamRow, error) {
			return []historyStreamRow{{key: cursor}, {key: cursor}}, nil
		},
	}
	_, err := src.head(context.Background())
	require.Error(t, err, "a store that repeats rows must not loop the request forever")
}

func TestHistoryCursor_RoundTrip(t *testing.T) {
	for _, key := range []config.HistoryPageKey{
		{Timestamp: time.Date(2026, 9, 1, 12, 0, 0, 123456000, time.UTC), Source: config.HistorySourcePurchase, ID: pageUUID(0xa, 1)},
		{Timestamp: time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), Source: config.HistorySourceExecution, ID: pageUUID(0xe, 2)},
	} {
		got, err := decodeHistoryCursor(encodeHistoryCursor(key))
		require.NoError(t, err)
		assert.True(t, key.Timestamp.Equal(got.Timestamp))
		assert.Equal(t, key.Source, got.Source)
		assert.Equal(t, key.ID, got.ID)
	}
}

// TestParseHistoryFilters_PagingParams covers the paging parameters'
// validation: anything but an unchanged next_cursor value is a 400, and so is
// an offset.
func TestParseHistoryFilters_PagingParams(t *testing.T) {
	enc := func(raw string) string { return b64url(raw) }
	id := pageUUID(0xa, 1)
	valid := encodeHistoryCursor(config.HistoryPageKey{Timestamp: time.Unix(1700000000, 0), Source: config.HistorySourcePurchase, ID: id})
	tests := []struct {
		name    string
		params  map[string]string
		wantErr string
	}{
		{"no paging params", map[string]string{}, ""},
		{"valid cursor", map[string]string{"cursor": valid}, ""},
		{"offset", map[string]string{"offset": "100"}, "offset is not supported"},
		{"offset zero", map[string]string{"offset": "0"}, "offset is not supported"},
		{"not base64", map[string]string{"cursor": "!!!"}, "invalid cursor"},
		{"wrong version", map[string]string{"cursor": enc("v2.2.1700000000000000." + id)}, "invalid cursor"},
		{"too few fields", map[string]string{"cursor": enc("v1.2.1700000000000000")}, "invalid cursor"},
		{"too many fields", map[string]string{"cursor": enc("v1.2.1700000000000000." + id + ".x")}, "invalid cursor"},
		{"unknown source", map[string]string{"cursor": enc("v1.3.1700000000000000." + id)}, "invalid cursor"},
		{"zero source", map[string]string{"cursor": enc("v1.0.1700000000000000." + id)}, "invalid cursor"},
		{"non-numeric timestamp", map[string]string{"cursor": enc("v1.2.soon." + id)}, "invalid cursor"},
		{"timestamp out of range", map[string]string{"cursor": enc("v1.2.9223372036854775807." + id)}, "invalid cursor"},
		{"id not a uuid", map[string]string{"cursor": enc("v1.2.1700000000000000.p-1")}, "invalid cursor"},
		{"id not canonical", map[string]string{"cursor": enc("v1.2.1700000000000000." + strings.ToUpper(id))}, "invalid cursor"},
		{"id injection", map[string]string{"cursor": enc("v1.2.1700000000000000.' OR 1=1 --")}, "invalid cursor"},
		{"overlong", map[string]string{"cursor": strings.Repeat("A", maxHistoryCursorLen+1)}, "invalid cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := parseHistoryFilters(tt.params)
			if tt.wantErr == "" {
				require.NoError(t, err)
				if tt.params["cursor"] != "" {
					require.NotNil(t, f.Cursor)
					assert.Equal(t, id, f.Cursor.ID)
				} else {
					assert.Nil(t, f.Cursor)
				}
				return
			}
			require.Error(t, err)
			ce, ok := IsClientError(err)
			require.True(t, ok, "got %T: %v", err, err)
			assert.Equal(t, 400, ce.code)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestHandler_getHistory_InvalidCursorReadsNothing checks the 400 happens
// before any store read.
func TestHandler_getHistory_InvalidCursorReadsNothing(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	mockAuth, req := adminHistoryReq(ctx)
	h := &Handler{auth: mockAuth, config: store}

	_, err := h.getHistory(ctx, req, map[string]string{"cursor": "garbage"})
	require.Error(t, err)
	ce, ok := IsClientError(err)
	require.True(t, ok)
	assert.Equal(t, 400, ce.code)
	store.AssertNotCalled(t, "ListPurchaseHistoryPage", mock.Anything, mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "ListHistoryExecutionsPage", mock.Anything, mock.Anything)
	store.AssertNotCalled(t, "GetExecutionsByStatuses", mock.Anything, mock.Anything, mock.Anything)
}

func TestHistoryStreamBatch(t *testing.T) {
	assert.Equal(t, config.DefaultListLimit, historyStreamBatch(1))
	assert.Equal(t, config.DefaultListLimit+1, historyStreamBatch(config.DefaultListLimit))
	assert.Equal(t, 501, historyStreamBatch(500))
	assert.Equal(t, config.MaxListLimit, historyStreamBatch(config.MaxListLimit))
}
