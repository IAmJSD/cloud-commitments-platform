package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/google/uuid"
)

// History paging (issue #674).
//
// GET /api/history lists two kinds of rows:
//
//   - the actionable executions (pending approvals and in-flight purchases),
//     listed in full on the first page only and never paged, so paging can
//     never push a pending approval off the first page or hide it on a later
//     one;
//   - the paged stream: completed purchase_history rows and terminal
//     executions merged newest first, `limit` rows per page.
//
// The stream is paged with a keyset cursor, not an offset. Its order is
// config.HistoryPageKey's (timestamp DESC, source DESC, row id DESC), which is
// unique, and next_cursor is the key of the last row on the page. A later page
// resumes strictly after it, so rows written between two page loads (new
// purchases land at the top) never shift, repeat or skip a row on the later
// page, and a page costs the same however deep it is. An offset would do
// neither: the account scope and the execution filters run in Go after the
// SQL read, so a SQL offset counts rows the caller never sees, and every
// insert ahead of the offset moves the page.
//
// The cursor names only a row the caller was shown, so it carries nothing
// about rows outside the caller's scope, and the scope is applied again on
// every page: a crafted cursor still only returns rows the caller may see.

const (
	historyCursorVersion = "v1"
	// maxHistoryCursorLen bounds the cursor before it is decoded. A valid one
	// is about 80 characters.
	maxHistoryCursorLen = 256
)

var errInvalidHistoryCursor = errors.New("invalid cursor")

// encodeHistoryCursor renders a stream position as an opaque URL-safe token.
// The timestamp is kept to the microsecond, PostgreSQL's precision, so the
// key read back from the database round-trips exactly.
func encodeHistoryCursor(k config.HistoryPageKey) string {
	raw := fmt.Sprintf("%s.%d.%d.%s", historyCursorVersion, k.Source, k.Timestamp.UnixMicro(), k.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeHistoryCursor parses a token from encodeHistoryCursor. It rejects
// anything else, including an ID that is not a canonical lowercase UUID: the
// store compares it with PostgreSQL's uuid ordering.
func decodeHistoryCursor(s string) (*config.HistoryPageKey, error) {
	if len(s) > maxHistoryCursorLen {
		return nil, errInvalidHistoryCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errInvalidHistoryCursor
	}
	parts := strings.Split(string(raw), ".")
	if len(parts) != 4 || parts[0] != historyCursorVersion {
		return nil, errInvalidHistoryCursor
	}
	src, err := parseHistoryCursorSource(parts[1])
	if err != nil {
		return nil, err
	}
	ts, err := parseHistoryCursorTime(parts[2])
	if err != nil {
		return nil, err
	}
	if id, err := uuid.Parse(parts[3]); err != nil || id.String() != parts[3] {
		return nil, errInvalidHistoryCursor
	}
	return &config.HistoryPageKey{Timestamp: ts, Source: src, ID: parts[3]}, nil
}

func parseHistoryCursorSource(s string) (config.HistorySource, error) {
	n, err := strconv.Atoi(s)
	src := config.HistorySource(n)
	if err != nil || (src != config.HistorySourceExecution && src != config.HistorySourcePurchase) {
		return 0, errInvalidHistoryCursor
	}
	return src, nil
}

// parseHistoryCursorTime reads the Unix-microsecond timestamp, limited to
// years PostgreSQL's timestamptz can compare against.
func parseHistoryCursorTime(s string) (time.Time, error) {
	micros, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, errInvalidHistoryCursor
	}
	ts := time.UnixMicro(micros).UTC()
	if ts.Year() < 1 || ts.Year() > 9999 {
		return time.Time{}, errInvalidHistoryCursor
	}
	return ts, nil
}

// parseHistoryPaging reads the paging parameters. An offset is refused rather
// than ignored: a client written against an offset API would otherwise get
// the first page back for every offset and never stop asking for more.
func parseHistoryPaging(params map[string]string) (*config.HistoryPageKey, error) {
	if params["offset"] != "" {
		return nil, NewClientError(400, "offset is not supported: page with cursor, passing the next_cursor value from the previous response")
	}
	s := params["cursor"]
	if s == "" {
		return nil, nil
	}
	cursor, err := decodeHistoryCursor(s)
	if err != nil {
		return nil, NewClientError(400, "invalid cursor: pass the next_cursor value from a previous /api/history response unchanged")
	}
	return cursor, nil
}

// historyStreamBatch is how many rows each stream source reads per query: at
// least one more than the page so has_more needs no extra query when nothing
// is filtered out, and at least DefaultListLimit so a scoped caller whose
// rows are sparse needs fewer round trips.
func historyStreamBatch(limit int) int {
	return min(max(limit+1, config.DefaultListLimit), config.MaxListLimit)
}

// historyStreamRow is one row of the paged stream: its stream position, its
// History projection, and whether the request's filters and the caller's
// account scope let the caller see it. Hidden rows still advance the stream.
type historyStreamRow struct {
	key     config.HistoryPageKey
	row     config.PurchaseHistoryRecord
	visible bool
}

// historyStreamSource reads one table's half of the stream in keyset batches.
type historyStreamSource struct {
	fetch func(ctx context.Context, after *config.HistoryPageKey) ([]historyStreamRow, error)
	buf   []historyStreamRow
	after *config.HistoryPageKey
	batch int
	done  bool
}

// head returns the source's next row without consuming it, reading the next
// batch when the buffer is empty. nil means the source is exhausted.
func (s *historyStreamSource) head(ctx context.Context) (*historyStreamRow, error) {
	if len(s.buf) == 0 && !s.done {
		rows, err := s.fetch(ctx, s.after)
		if err != nil {
			s.done = true
			return nil, err
		}
		if len(rows) > 0 {
			last := rows[len(rows)-1].key
			if s.after != nil && !s.after.SortsBefore(rows[0].key) {
				s.done = true
				return nil, fmt.Errorf("history stream: keyset read did not advance past %v", *s.after)
			}
			s.after = &last
		}
		s.buf = rows
		s.done = len(rows) < s.batch
	}
	if len(s.buf) == 0 {
		return nil, nil
	}
	return &s.buf[0], nil
}

// historyStream merges the purchase_history and terminal-execution sources in
// stream order.
type historyStream struct {
	h                *Handler
	session          *Session
	filters          *historyFilters
	purchases        historyStreamSource
	executions       historyStreamSource
	approver         string
	approverResolved bool
	// executionsFailed reports that a terminal-execution read failed. The
	// stream then continues with purchase_history rows only, as History did
	// when a class listing failed (issue #686), and the response is flagged
	// truncated.
	executionsFailed bool
}

func newHistoryStream(h *Handler, session *Session, filters *historyFilters) *historyStream {
	batch := historyStreamBatch(filters.Limit)
	st := &historyStream{h: h, session: session, filters: filters}
	st.purchases = historyStreamSource{fetch: st.fetchPurchases, after: filters.Cursor, batch: batch}
	st.executions = historyStreamSource{fetch: st.fetchTerminalExecutions, after: filters.Cursor, batch: batch}
	return st
}

// next consumes and returns the next row of the merged stream, or nil at the
// end. A purchase_history read error fails the request, as it did before
// paging; a terminal-execution read error is logged and that source dropped.
func (st *historyStream) next(ctx context.Context) (*historyStreamRow, error) {
	p, err := st.purchases.head(ctx)
	if err != nil {
		return nil, err
	}
	e, err := st.executions.head(ctx)
	if err != nil {
		logging.Warnf("history: failed to load terminal executions: %v", err)
		st.executionsFailed = true
	}
	var src *historyStreamSource
	switch {
	case p != nil && (e == nil || p.key.SortsBefore(e.key)):
		src = &st.purchases
	case e != nil:
		src = &st.executions
	default:
		return nil, nil
	}
	row := src.buf[0]
	src.buf = src.buf[1:]
	return &row, nil
}

func (st *historyStream) fetchPurchases(ctx context.Context, after *config.HistoryPageKey) ([]historyStreamRow, error) {
	recs, err := st.h.fetchPurchaseHistory(ctx, st.filters, after, st.purchases.batch)
	if err != nil {
		return nil, err
	}
	rows := make([]historyStreamRow, len(recs))
	for i := range recs {
		rows[i] = historyStreamRow{key: recs[i].Key(), row: recs[i].PurchaseHistoryRecord, visible: true}
		rows[i].row.Status = "completed"
	}
	return rows, st.h.markOutOfScope(ctx, st.session, rows)
}

// fetchTerminalExecutions reads a batch of terminal executions and projects
// the ones matching the request's filters into History rows. The date bound
// is pushed into SQL as well as checked by matchesExecution; provider and
// account are checked in Go only, like the actionable class.
func (st *historyStream) fetchTerminalExecutions(ctx context.Context, after *config.HistoryPageKey) ([]historyStreamRow, error) {
	q := config.HistoryExecutionsPageQuery{Statuses: terminalExecutionStatuses, After: after, Limit: st.executions.batch}
	if st.filters.HasDate {
		start, end := st.filters.Start, st.filters.End
		q.Start, q.End = &start, &end
	}
	execs, err := st.h.config.ListHistoryExecutionsPage(ctx, q)
	if err != nil {
		return nil, err
	}
	rows := make([]historyStreamRow, len(execs))
	matching := make([]config.PurchaseExecution, 0, len(execs))
	for i := range execs {
		rows[i].key = config.HistoryPageKey{Timestamp: execs[i].ScheduledDate, Source: config.HistorySourceExecution, ID: execs[i].ExecutionID}
		// Clean completed executions are excluded by the store; checked here
		// too so one can never show twice (it renders via purchase_history).
		rows[i].visible = (execs[i].Status != "completed" || execs[i].Error != "") && st.filters.matchesExecution(execs[i])
		if rows[i].visible {
			matching = append(matching, execs[i])
		}
	}
	if len(matching) == 0 {
		return rows, nil
	}
	approver := st.approverEmail(ctx)
	emails := st.h.resolveUserEmails(ctx, matching)
	for i := range execs {
		if rows[i].visible {
			rows[i].row = executionToHistoryRow(execs[i], approver, createdByEmail(emails, &execs[i]))
		}
	}
	return rows, st.h.markOutOfScope(ctx, st.session, rows)
}

// approverEmail resolves the approver address once per request.
func (st *historyStream) approverEmail(ctx context.Context) string {
	if !st.approverResolved {
		st.approver = st.h.resolvePendingApproverEmail(ctx)
		st.approverResolved = true
	}
	return st.approver
}

func createdByEmail(emails map[string]string, exec *config.PurchaseExecution) string {
	if exec.CreatedByUserID == nil {
		return ""
	}
	return emails[*exec.CreatedByUserID]
}

// markOutOfScope hides every visible row the caller's account scope does not
// allow. The scope rule has one home, filterPurchaseHistoryByAllowedAccounts;
// this runs the batch's visible rows through it and matches the rows it keeps
// back to the batch in order. The filter keeps an order-preserving subset and
// decides each row from that row alone, and sameHistoryRow compares every
// field it reads plus the row's identity, so the match is exact.
func (h *Handler) markOutOfScope(ctx context.Context, session *Session, rows []historyStreamRow) error {
	candidates := make([]config.PurchaseHistoryRecord, 0, len(rows))
	index := make([]int, 0, len(rows))
	for i := range rows {
		if rows[i].visible {
			candidates = append(candidates, rows[i].row)
			index = append(index, i)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	kept, err := h.filterPurchaseHistoryByAllowedAccounts(ctx, session, candidates)
	if err != nil {
		return err
	}
	j := 0
	for n, i := range index {
		if j < len(kept) && sameHistoryRow(&kept[j], &candidates[n]) {
			j++
			continue
		}
		rows[i].visible = false
	}
	return nil
}

// sameHistoryRow compares the fields the account-scope check reads (account,
// cloud account, provider, creator) and the row's identity.
func sameHistoryRow(a, b *config.PurchaseHistoryRecord) bool {
	return a.PurchaseID == b.PurchaseID && a.Timestamp.Equal(b.Timestamp) &&
		a.AccountID == b.AccountID && a.Provider == b.Provider &&
		a.CreatedByUserID == b.CreatedByUserID && sameOptionalString(a.CloudAccountID, b.CloudAccountID)
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// historyPage is one page of the stream.
type historyPage struct {
	rows []config.PurchaseHistoryRecord
	// nextCursor is set when more visible rows follow the page.
	nextCursor string
	// incomplete reports that terminal executions could not be read.
	incomplete bool
}

// fetchHistoryPage returns the next filters.Limit rows of the stream the
// caller may see, starting after filters.Cursor. It reads one row past the
// page so has_more is exact: has_more is never set when the rows after the
// page are all outside the caller's scope or filters.
func (h *Handler) fetchHistoryPage(ctx context.Context, session *Session, filters *historyFilters) (historyPage, error) {
	st := newHistoryStream(h, session, filters)
	var page historyPage
	var last config.HistoryPageKey
	for {
		r, err := st.next(ctx)
		if err != nil {
			return historyPage{}, err
		}
		if r == nil {
			break
		}
		if !r.visible {
			continue
		}
		if len(page.rows) == filters.Limit {
			page.nextCursor = encodeHistoryCursor(last)
			break
		}
		page.rows = append(page.rows, r.row)
		last = r.key
	}
	page.incomplete = st.executionsFailed
	return page, nil
}
