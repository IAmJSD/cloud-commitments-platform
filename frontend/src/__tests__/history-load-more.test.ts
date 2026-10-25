/**
 * Load more on the History view (issue #676): the API pages with has_more /
 * next_cursor (#674); the UI appends each page and rebuilds both tables, so
 * the Approval Queue covers every loaded page.
 */

import { loadHistory, loadMoreHistory, setupHistoryHandlers } from '../history';
import { historyTruncationMessage, mergeHistoryPages } from '../history-paging';

jest.mock('../api', () => ({
  getHistory: jest.fn(),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  listAccounts: jest.fn().mockResolvedValue([]),
  getDeploymentInfo: jest.fn().mockResolvedValue({}),
}));
jest.mock('../navigation', () => ({ switchTab: jest.fn() }));
jest.mock('../utils', () => ({
  formatCurrency: jest.fn((val) => `$${val || 0}`),
  formatDate: jest.fn((val) => (val ? new Date(val).toLocaleDateString() : '')),
  formatTerm: jest.fn((years) => (years == null ? '' : `${years} Year${years === 1 ? '' : 's'}`)),
  escapeHtml: jest.fn((str) => str || ''),
  escapeHtmlAttr: jest.fn((str: string | null | undefined) => str || ''),
  amortizedMonthly: jest.fn(() => 0),
}));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn() }));
jest.mock('../toast', () => ({ showToast: jest.fn() }));
jest.mock('../state', () => ({
  getCurrentUser: jest.fn(),
  getCurrentProvider: jest.fn().mockReturnValue('aws'),
  getCurrentAccountIDs: jest.fn().mockReturnValue([]),
  subscribeProvider: jest.fn().mockReturnValue(() => {}),
  subscribeAccount: jest.fn().mockReturnValue(() => {}),
  getAmortizeUpfront: jest.fn().mockReturnValue(false),
  setAmortizeUpfront: jest.fn(),
  subscribeAmortizeUpfront: jest.fn().mockReturnValue(() => {}),
  getPurchaseHistoryColumnFilters: jest.fn().mockReturnValue({}),
  setPurchaseHistoryColumnFilter: jest.fn(),
  getApprovalQueueColumnFilters: jest.fn().mockReturnValue({}),
  setApprovalQueueColumnFilter: jest.fn(),
}));
jest.mock('../recommendations', () => ({ getAccountName: jest.fn((id: string) => id) }));

import * as api from '../api';
import { showToast } from '../toast';
import type { HistoryPurchase, HistoryResponse } from '../types';

const getHistory = api.getHistory as jest.Mock;

function row(id: string, status = 'completed', ts = '2026-10-01T00:00:00Z'): HistoryPurchase {
  return {
    purchase_id: id, timestamp: ts, provider: 'aws', service: 'ec2', resource_type: 'm5.large',
    region: 'us-east-1', count: 1, term: 1, upfront_cost: 0, monthly_cost: 1, estimated_savings: 1, status,
  };
}

function page(purchases: HistoryPurchase[], extra: Partial<HistoryResponse> = {}): HistoryResponse {
  return { summary: { total_purchases: 99 }, purchases, ...extra };
}

function setupDOM(): void {
  document.body.innerHTML = `
    <input type="date" id="history-start" value="2026-09-01"><input type="date" id="history-end" value="2026-10-23">
    <p id="purchases-truncation-notice" hidden></p><p id="history-truncation-notice" hidden></p>
    <div id="purchases-approval-queue"></div><div id="history-summary"></div><div id="history-list"></div>
    <div id="history-load-more" hidden><button type="button" id="history-load-more-btn">Load more</button></div>`;
}

const loadMoreWrap = (): HTMLElement => document.getElementById('history-load-more')!;
const historyIds = (): string[] => Array.from(document.querySelectorAll<HTMLElement>('#history-list tr[data-execution-id]'))
  .map(tr => tr.dataset['executionId'] || '');
const queueText = (): string => document.getElementById('purchases-approval-queue')!.textContent || '';

describe('History Load more (issue #676)', () => {
  beforeEach(() => {
    setupDOM();
    jest.clearAllMocks();
  });

  test('stays hidden when the server does not page', async () => {
    getHistory.mockResolvedValueOnce(page([row('a')]));
    await loadHistory();
    expect(loadMoreWrap().hidden).toBe(true);
  });

  test('appends the second page with the same filters plus the cursor, then hides', async () => {
    getHistory.mockResolvedValueOnce(page([row('a'), row('b')], { truncated: true, has_more: true, next_cursor: 'c1' }));
    await loadHistory();
    expect(loadMoreWrap().hidden).toBe(false);
    expect(document.getElementById('history-truncation-notice')!.hidden).toBe(true);

    getHistory.mockResolvedValueOnce(page([row('c', 'failed')], { summary: { total_purchases: 1 }, has_more: false }));
    await loadMoreHistory();

    const firstFilters = getHistory.mock.calls[0][0];
    expect(getHistory.mock.calls[1][0]).toEqual({ ...firstFilters, cursor: 'c1' });
    expect(firstFilters.provider).toBe('aws');
    expect(historyIds()).toEqual(['a', 'b', 'c']);
    expect(loadMoreWrap().hidden).toBe(true);
    // Summary cards keep the first page's totals.
    expect(document.getElementById('history-summary')!.textContent).toContain('99');
  });

  test('builds the Approval Queue from every loaded page', async () => {
    getHistory.mockResolvedValueOnce(page([row('p1', 'pending')], { has_more: true, next_cursor: 'c1' }));
    await loadHistory();
    getHistory.mockResolvedValueOnce(page([row('p2', 'notified')], { has_more: false }));
    await loadMoreHistory();
    const queueRows = document.querySelectorAll('#purchases-approval-queue tbody tr');
    expect(queueRows).toHaveLength(2);
  });

  test('a row repeated on a later page replaces the stale copy', async () => {
    getHistory.mockResolvedValueOnce(page([row('x', 'pending'), row('a')], { has_more: true, next_cursor: 'c1' }));
    await loadHistory();
    expect(queueText()).not.toContain('No pending approvals');
    getHistory.mockResolvedValueOnce(page([row('x', 'expired')], { has_more: false }));
    await loadMoreHistory();
    expect(historyIds()).toEqual(['a', 'x']);
    expect(queueText()).toContain('No pending approvals');
  });

  test('a reload drops the cursor and discards a Load more response still in flight', async () => {
    getHistory.mockResolvedValueOnce(page([row('a')], { has_more: true, next_cursor: 'c1' }));
    await loadHistory();
    let resolveMore!: (v: HistoryResponse) => void;
    getHistory.mockReturnValueOnce(new Promise<HistoryResponse>(r => { resolveMore = r; }));
    const more = loadMoreHistory();
    const btn = document.getElementById('history-load-more-btn') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    // A second click while the page is in flight does not fetch again.
    await loadMoreHistory();
    expect(getHistory).toHaveBeenCalledTimes(2);

    getHistory.mockResolvedValueOnce(page([row('n')]));
    await loadHistory();
    resolveMore(page([row('stale')], { has_more: false }));
    await more;
    expect(historyIds()).toEqual(['n']);
    expect(loadMoreWrap().hidden).toBe(true);
  });

  test('a failed page keeps the rows and the button for a retry', async () => {
    getHistory.mockResolvedValueOnce(page([row('a')], { has_more: true, next_cursor: 'c1' }));
    await loadHistory();
    jest.spyOn(console, 'error').mockImplementation(() => {});
    getHistory.mockRejectedValueOnce(new Error('boom'));
    await loadMoreHistory();
    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'error', message: expect.stringContaining('boom') }));
    expect(historyIds()).toEqual(['a']);
    const btn = document.getElementById('history-load-more-btn') as HTMLButtonElement;
    expect(loadMoreWrap().hidden).toBe(false);
    expect(btn.disabled).toBe(false);

    getHistory.mockResolvedValueOnce(page([row('b')], { has_more: false }));
    await loadMoreHistory();
    expect(getHistory.mock.calls[2][0].cursor).toBe('c1');
    expect(historyIds()).toEqual(['a', 'b']);
  });

  test('the button is wired by setupHistoryHandlers', async () => {
    setupHistoryHandlers();
    getHistory.mockResolvedValueOnce(page([row('a')], { has_more: true, next_cursor: 'c1' }));
    await loadHistory();
    getHistory.mockResolvedValueOnce(page([row('b')], { has_more: false }));
    document.getElementById('history-load-more-btn')!.click();
    await new Promise(r => setTimeout(r, 0));
    expect(getHistory.mock.calls[1][0].cursor).toBe('c1');
  });
});

describe('history-paging helpers', () => {
  test('mergeHistoryPages keeps rows without a purchase_id', () => {
    const anon = { ...row('z'), purchase_id: undefined };
    expect(mergeHistoryPages([anon, row('a')], [anon, row('a', 'failed')]).map(p => p.status))
      .toEqual(['completed', 'completed', 'failed']);
  });

  test('banner text follows has_more', () => {
    expect(historyTruncationMessage({ truncated: false })).toBe('');
    expect(historyTruncationMessage({ truncated: true, limit: 100, executions_limit: 100 })).toContain('Narrow the date range');
    expect(historyTruncationMessage({ truncated: true, has_more: true, next_cursor: 'c' })).toBe('');
    expect(historyTruncationMessage({ truncated: true, has_more: false, actionable_executions_limit: 1000 }))
      .toContain('capped at 1000');
    // has_more without a usable cursor cannot page: fall back to the old text.
    expect(historyTruncationMessage({ truncated: true, has_more: true, limit: 5 })).toContain('most recent 5');
  });
});
