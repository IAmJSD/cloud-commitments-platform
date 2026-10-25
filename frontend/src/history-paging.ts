/**
 * "Load more" paging for the Purchase History view (issue #676).
 *
 * The History API pages completed purchases and terminal executions with a
 * keyset cursor (issue #674): a response with `has_more: true` carries a
 * `next_cursor`, and repeating the request with `cursor=<next_cursor>` returns
 * the rows after it. Pending and in-flight executions are not paged; the first
 * page lists all of them. Servers that predate paging omit `has_more`, so the
 * control below stays hidden and the old "results are capped" banner remains.
 *
 * history.ts owns the fetch and the rendering; this module holds the pure
 * helpers (cursor, row merge, banner text) and the button's DOM state.
 */

import type { HistoryPurchase, HistoryResponse } from './types';

/** The cursor for the next page, or null when no further page can be fetched. */
export function nextHistoryCursor(data: HistoryResponse): string | null {
  if (data.has_more !== true) return null;
  return typeof data.next_cursor === 'string' && data.next_cursor !== '' ? data.next_cursor : null;
}

/**
 * Append a newly fetched page to the rows already loaded.
 *
 * Rows are matched by purchase_id and the newest response wins: an approval
 * listed as pending on page 1 can be swept to expired before a later page is
 * read, and that later page then lists the same execution again in its new
 * state. Keeping both would show one execution twice with conflicting
 * statuses. Rows without a purchase_id cannot be matched and are kept as is.
 */
export function mergeHistoryPages(
  loaded: HistoryPurchase[],
  page: HistoryPurchase[],
): HistoryPurchase[] {
  const incoming = new Set<string>();
  for (const p of page) {
    if (p.purchase_id) incoming.add(p.purchase_id);
  }
  const kept = loaded.filter(p => !p.purchase_id || !incoming.has(p.purchase_id));
  return [...kept, ...page];
}

/**
 * Text of the "results capped" banner for a History response, or '' to hide
 * it.
 *
 * - No `has_more` field: the server cannot page, so keep the #630 wording that
 *   tells the user to narrow the window.
 * - `has_more` with a cursor: older rows are one click away, so the Load more
 *   button replaces the banner.
 * - Truncated without more pages: rows paging cannot reach, i.e. the
 *   actionable class hit its cap or a listing failed.
 */
export function historyTruncationMessage(data: HistoryResponse): string {
  if (!data.truncated) return '';
  if (nextHistoryCursor(data)) return '';
  if (data.has_more === false) {
    return `Some rows could not be listed: pending and in-progress executions are capped at ${data.actionable_executions_limit ?? '?'}, or part of the history failed to load. Pending approvals and totals may be incomplete. Narrow the date range or filters, or reload, to see them.`;
  }
  return `Results are capped at the most recent ${data.limit ?? '?'} completed purchases and ${data.executions_limit ?? '?'} pending, failed or other non-completed executions. Older rows are not shown, so pending approvals and totals may be incomplete. Narrow the date range or filters to see them.`;
}

/**
 * Show or hide the Load more control below the history table. `busy` disables
 * the button while a page is in flight so a double click cannot fetch the
 * same cursor twice.
 */
export function renderLoadMoreControl(visible: boolean, busy = false): void {
  const wrap = document.getElementById('history-load-more');
  if (wrap) wrap.hidden = !visible;
  const btn = document.getElementById('history-load-more-btn') as HTMLButtonElement | null;
  if (!btn) return;
  btn.disabled = busy;
  btn.textContent = busy ? 'Loading…' : 'Load more';
  btn.setAttribute('aria-busy', busy ? 'true' : 'false');
}
