/**
 * Escaping-convention regressions in history.ts innerHTML templates (issue #37).
 *
 * api.getHistory is cast to HistoryResponse with no runtime validation, so
 * the `number` types on count / retry_attempt_n are not a guarantee. These
 * tests use the REAL utils escapers (history-retry-button.test.ts mocks
 * escapeHtml as a passthrough) and feed non-number values through:
 *
 *   1. retry_attempt_n inside the "Retry #n" badge's title="..." attribute
 *      (the one site positioned to break out of an attribute). Today the
 *      `> 0` guard in front of it already drops non-numeric strings; this
 *      test pins that the badge stays inert if that guard ever changes.
 *   2. count in the Purchase History table cell.
 *   3. count in the approval-queue table cell.
 */
import { loadHistory, renderApprovalQueue } from '../history';
import type { HistoryPurchase } from '../types';
import * as api from '../api';
import { getCurrentUser } from '../state';
import { ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID } from '../permissions';

jest.mock('../api', () => ({
  getHistory: jest.fn(),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  retryPurchase: jest.fn(),
  cancelPurchase: jest.fn(),
}));

jest.mock('../navigation', () => ({ switchTab: jest.fn() }));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn() }));
jest.mock('../toast', () => ({ showToast: jest.fn() }));

jest.mock('../state', () => ({
  getCurrentUser: jest.fn(),
  getCurrentProvider: jest.fn().mockReturnValue(''),
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

const ADMIN_USER = { id: 'admin-uuid', email: 'admin@example.com', groups: [ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID] };
const HOSTILE = '1" onmouseover="alert(1)"><img src=x onerror="alert(2)">';

function setupDOM(): void {
  document.body.replaceChildren();
  for (const id of ['history-start', 'history-end']) {
    const el = document.createElement('input');
    el.type = 'date';
    el.id = id;
    document.body.appendChild(el);
  }
  for (const id of ['history-provider-filter', 'history-account-filter']) {
    const el = document.createElement('select');
    el.id = id;
    document.body.appendChild(el);
  }
  for (const id of ['history-summary', 'history-list', 'purchases-approval-queue']) {
    const el = document.createElement('div');
    el.id = id;
    document.body.appendChild(el);
  }
}

function makeRow(overrides: Record<string, unknown>): Record<string, unknown> {
  return {
    purchase_id: 'exec-1',
    timestamp: '2024-01-15T00:00:00Z',
    provider: 'aws',
    service: 'ec2',
    resource_type: 't3.medium',
    region: 'us-east-1',
    count: 1,
    term: 1,
    upfront_cost: 100,
    estimated_savings: 50,
    plan_name: '',
    status: 'completed',
    created_by_user_id: ADMIN_USER.id,
    ...overrides,
  };
}

describe('history.ts escaping of API-typed numbers (issue #37)', () => {
  beforeEach(() => {
    setupDOM();
    jest.clearAllMocks();
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
  });

  test('a non-number retry_attempt_n cannot break out of the badge title attribute', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ retry_attempt_n: HOSTILE })],
    });

    await loadHistory();

    const list = document.getElementById('history-list') as HTMLElement;
    expect(list.querySelector('[onmouseover]')).toBeNull();
    expect(list.querySelector('img')).toBeNull();
  });

  test('a numeric retry_attempt_n still renders its "Retry #n" badge', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ retry_attempt_n: 3 })],
    });

    await loadHistory();

    const badge = document.querySelector('.history-retry-of');
    expect(badge?.textContent).toContain('Retry #3');
    expect(badge?.getAttribute('title')).toBe('This row is retry #3 in its chain');
  });

  test('a non-number count is rendered as text in the history table', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ count: HOSTILE })],
    });

    await loadHistory();

    const list = document.getElementById('history-list') as HTMLElement;
    expect(list.querySelector('img')).toBeNull();
    expect(list.textContent).toContain(HOSTILE);
  });

  test('a non-number count is rendered as text in the approval queue', () => {
    renderApprovalQueue([makeRow({ status: 'pending', count: HOSTILE }) as unknown as HistoryPurchase]);

    const queue = document.getElementById('purchases-approval-queue') as HTMLElement;
    expect(queue.querySelector('img')).toBeNull();
    expect(queue.textContent).toContain(HOSTILE);
  });
});
