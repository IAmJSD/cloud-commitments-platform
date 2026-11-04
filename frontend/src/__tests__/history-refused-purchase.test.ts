/**
 * Refused-purchase presentation in Purchase History (issue #1694):
 * clamped status text, muted savings on rows that bought nothing, and the
 * Total Purchases card counting completed purchases only.
 */
import { loadHistory, purchaseHistoryNumericCellValue } from '../history';

jest.mock('../api', () => ({
  getHistory: jest.fn(),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
}));
jest.mock('../navigation', () => ({ switchTab: jest.fn() }));
jest.mock('../permissions', () => ({ canAccess: jest.fn().mockReturnValue(true) }));
jest.mock('../utils', () => ({
  NO_VALUE: '\u2014',
  formatCurrency: jest.fn((v) => (v === null || v === undefined || isNaN(v)) ? '—' : `$${v}`),
  formatDate: jest.fn((v) => (v ? new Date(v).toLocaleDateString() : '')),
  formatTerm: jest.fn((y) => (y == null ? '' : `${y} Year${y === 1 ? '' : 's'}`)),
  escapeHtml: jest.fn((s) => s || ''),
  escapeHtmlAttr: jest.fn((s: string | null | undefined) => (s || '').replace(/"/g, '&quot;')),
  amortizedMonthly: jest.requireActual('../utils').amortizedMonthly,
  populateAccountFilter: jest.fn(() => Promise.resolve()),
}));
jest.mock('../state', () => ({
  getCurrentUser: jest.fn(),
  getCurrentProvider: jest.fn().mockReturnValue('all'),
  setCurrentProvider: jest.fn(),
  getCurrentAccountIDs: jest.fn().mockReturnValue([]),
  setCurrentAccountIDs: jest.fn(),
  subscribeProvider: jest.fn().mockReturnValue(() => {}),
  subscribeAccount: jest.fn().mockReturnValue(() => {}),
  getAmortizeUpfront: jest.fn().mockReturnValue(false),
  setAmortizeUpfront: jest.fn(),
  subscribeAmortizeUpfront: jest.fn().mockReturnValue(() => {}),
  getPurchaseHistoryColumnFilters: jest.fn().mockReturnValue({}),
  setPurchaseHistoryColumnFilter: jest.fn(),
  clearAllPurchaseHistoryColumnFilters: jest.fn(),
  getApprovalQueueColumnFilters: jest.fn().mockReturnValue({}),
  setApprovalQueueColumnFilter: jest.fn(),
  clearAllApprovalQueueColumnFilters: jest.fn(),
}));

import * as api from '../api';

const row = (o: Record<string, unknown>) => ({
  purchase_id: 'p1', timestamp: '2026-11-04T00:00:00Z', provider: 'aws', service: 'ec2',
  resource_type: 'm5.large', region: 'us-east-1', count: 1, term: 1,
  upfront_cost: 0, monthly_cost: 50, estimated_savings: 10, status: 'completed', ...o,
});

beforeEach(() => {
  document.body.innerHTML = `
    <input type="date" id="history-start"><input type="date" id="history-end">
    <div id="history-summary"></div><div id="history-list"></div>
    <div id="purchases-approval-queue"></div>`;
  jest.clearAllMocks();
});

describe('status description (issue #1694)', () => {
  const long = 'AuthFailure: '.repeat(80);
  test('is clamped and the full text stays reachable via title', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [
      row({ status: 'failed', status_description: long }),
    ] });
    await loadHistory();
    const el = document.querySelector('#history-list td[data-label="Status"] .history-status-desc');
    expect(el).not.toBeNull();
    expect(el?.getAttribute('title')).toBe(long);
  });

  test('a long description is also reachable by keyboard and touch via a disclosure', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [
      row({ status: 'failed', status_description: long }),
    ] });
    await loadHistory();
    const details = document.querySelector('#history-list td[data-label="Status"] details.history-status-full');
    expect(details?.querySelector('summary')).not.toBeNull();
    expect(details?.textContent).toContain(long);
  });

  test('a description clamped only in the phone layout (~100 chars) also has a disclosure', async () => {
    const medium = 'purchase failed: '.repeat(6);
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [
      row({ status: 'failed', status_description: medium }),
    ] });
    await loadHistory();
    expect(document.querySelector('#history-list details.history-status-full')?.textContent).toContain(medium);
  });

  test('a short description has no disclosure', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [
      row({ status: 'failed', status_description: 'the provider returned an error' }),
    ] });
    await loadHistory();
    expect(document.querySelector('#history-list details.history-status-full')).toBeNull();
  });
});

describe('savings value used by sort and filter (issue #1694)', () => {
  test.each([
    ['failed', { status: 'failed' }],
    ['revoked', { status: 'completed', revoked_at: '2026-11-04T01:00:00Z' }],
  ])('%s row counts as no saving', (_name, o) => {
    expect(purchaseHistoryNumericCellValue(row(o) as never, 'savings')).toBe(0);
    expect(purchaseHistoryNumericCellValue(row({}) as never, 'savings')).toBe(10);
  });
});

describe('Monthly Savings on rows that bought nothing (issue #1694)', () => {
  test.each(['failed', 'cancelled', 'expired'])('%s row is not styled as a saving', async (status) => {
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [
      row({ purchase_id: 'x', status }),
      row({ purchase_id: 'ok', status: 'completed' }),
    ] });
    await loadHistory();
    const cell = (id: string) => document.querySelector(`#history-list tr[data-execution-id="${id}"] td[data-label="Monthly Savings"]`);
    expect(cell('x')?.classList.contains('savings')).toBe(false);
    expect(cell('x')?.textContent).toBe('—');
    expect(cell('ok')?.classList.contains('savings')).toBe(true);
    expect(cell('ok')?.textContent).toBe('$10');
  });

  test('a revoked completed row is not styled as a saving', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [
      row({ purchase_id: 'rv', status: 'completed', revoked_at: '2026-11-04T01:00:00Z' }),
    ] });
    await loadHistory();
    const cell = document.querySelector('#history-list tr[data-execution-id="rv"] td[data-label="Monthly Savings"]');
    expect(cell?.classList.contains('savings')).toBe(false);
  });
});

describe('Total Purchases card (issue #1694)', () => {
  test('counts completed purchases, not failed or cancelled rows', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: { total_purchases: 4, total_completed: 0, total_failed: 1, total_pending: 0 },
      purchases: [],
    });
    await loadHistory();
    const card = Array.from(document.querySelectorAll('#history-summary .card'))
      .find((c) => c.querySelector('h3')?.textContent === 'Total Purchases');
    expect(card?.querySelector('.value')?.textContent).toBe('0');
  });
});
