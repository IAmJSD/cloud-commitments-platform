/**
 * History approval queue card tests (issue #340 sub-task).
 *
 * The queue card sits above the Purchase History table inside
 * #purchases-tab and surfaces pending|notified purchases in a dedicated
 * action-focused view. Approve / Cancel buttons reuse the existing
 * history.ts helpers (renderPendingActionButtons + wireRowActionHandlers)
 * so the click flow is the SAME as approving from the history table.
 *
 * Tested matrix:
 *   1. Queue renders only pending|notified rows; skips completed,
 *      cancelled, failed, expired.
 *   2. Queue empty-state copy when no pending rows exist.
 *   3. Approve click on a queue row goes through confirmDialog →
 *      api.approvePurchase → showToast → reload.
 *   4. Cancel click on a queue row goes through confirmDialog →
 *      api.cancelPurchase → showToast → reload.
 *   5. The queue is the only approval surface (issue #183): the same
 *      pending row stays in the history table, which links to the queue
 *      row instead of repeating Approve / Cancel.
 */

import { loadHistory } from '../history';

jest.mock('../api', () => ({
  getHistory: jest.fn(),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  approvePurchase: jest.fn(),
  cancelPurchase: jest.fn(),
  getPurchaseDetails: jest.fn().mockResolvedValue({
    total_upfront_cost: 100,
    estimated_savings: 10,
    recommendations: [{ provider: 'aws', service: 'ec2', region: 'us-east-1', count: 1, term: 1, upfront_cost: 100, savings: 10 }],
  }),
  listAccounts: jest.fn().mockResolvedValue([]),
  getDeploymentInfo: jest.fn().mockResolvedValue({}),
}));

jest.mock('../navigation', () => ({
  switchTab: jest.fn(),
}));

jest.mock('../utils', () => ({
  NO_VALUE: '\u2014',
  formatCurrency: jest.fn((val) => `$${val || 0}`),
  formatDate: jest.fn((val) => (val ? new Date(val).toLocaleDateString() : '')),
  formatTerm: jest.fn((years) => (years == null ? '' : `${years} Year${years === 1 ? '' : 's'}`)),
  escapeHtml: jest.fn((str) => str || ''),
  escapeHtmlAttr: jest.fn((str: string | null | undefined) => {
    if (!str) return '';
    return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }),
  populateAccountFilter: jest.fn(() => Promise.resolve()),
}));

jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn(),
}));

jest.mock('../toast', () => ({
  showToast: jest.fn(),
}));

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

jest.mock('../recommendations', () => ({
  getAccountName: jest.fn((id: string) => id),
}));

import * as api from '../api';
import { confirmDialog } from '../confirmDialog';
import { showToast } from '../toast';
import * as state from '../state';
import { getCurrentUser } from '../state';
import { getAccountName } from '../recommendations';
import { ADMINISTRATORS_GROUP_ID } from '../permissions';
import { flushPromises } from './flush-promises';

const ADMIN_USER = { id: 'admin-uuid', email: 'admin@example.com', groups: [ADMINISTRATORS_GROUP_ID] };

function setupDOM(): void {
  while (document.body.firstChild) document.body.removeChild(document.body.firstChild);

  const mkInput = (id: string): HTMLInputElement => {
    const el = document.createElement('input');
    el.type = 'date';
    el.id = id;
    return el;
  };
  const mkDiv = (id: string): HTMLDivElement => {
    const el = document.createElement('div');
    el.id = id;
    return el;
  };

  document.body.appendChild(mkInput('history-start'));
  document.body.appendChild(mkInput('history-end'));
  document.body.appendChild(mkDiv('history-summary'));
  document.body.appendChild(mkDiv('history-list'));
  document.body.appendChild(mkDiv('purchases-approval-queue'));
}

function makeRow(overrides: Record<string, unknown>) {
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
    status: 'pending',
    ...overrides,
  };
}

describe('Approval queue card (issue #340 sub-task)', () => {
  beforeEach(() => {
    setupDOM();
    jest.clearAllMocks();
  });

  test('renders only pending|notified rows; skips completed / cancelled / failed / expired', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({ purchase_id: 'p-pending', status: 'pending', created_by_user_id: ADMIN_USER.id }),
        makeRow({ purchase_id: 'p-notified', status: 'notified', created_by_user_id: ADMIN_USER.id }),
        makeRow({ purchase_id: 'p-completed', status: 'completed' }),
        makeRow({ purchase_id: 'p-cancelled', status: 'cancelled' }),
        makeRow({ purchase_id: 'p-failed', status: 'failed' }),
        makeRow({ purchase_id: 'p-expired', status: 'expired' }),
      ],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue');
    expect(queue).toBeTruthy();
    const queueRows = queue!.querySelectorAll('tr[data-execution-id]');
    const ids = Array.from(queueRows).map((r) => r.getAttribute('data-execution-id'));
    expect(ids).toEqual(expect.arrayContaining(['p-pending', 'p-notified']));
    expect(ids).toHaveLength(2);
    // Negative assertions on the queue.
    for (const skipped of ['p-completed', 'p-cancelled', 'p-failed', 'p-expired']) {
      expect(ids).not.toContain(skipped);
    }
  });

  test('shows empty-state copy when no pending rows', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({ purchase_id: 'p-completed', status: 'completed' }),
        makeRow({ purchase_id: 'p-cancelled', status: 'cancelled' }),
      ],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue');
    expect(queue?.textContent || '').toMatch(/no pending approvals/i);
    expect(queue?.querySelectorAll('tr[data-execution-id]').length).toBe(0);
  });

  test('shows empty-state copy when the full history is empty', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [] });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue');
    expect(queue?.textContent || '').toMatch(/no pending approvals/i);
  });

  test('Approve click on a queue row runs confirm + api.approvePurchase + toast + reload', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (confirmDialog as jest.Mock).mockResolvedValue(true);
    (api.approvePurchase as jest.Mock).mockResolvedValue(undefined);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'p-pending', created_by_user_id: ADMIN_USER.id })],
    });

    await loadHistory();
    expect(api.getHistory).toHaveBeenCalledTimes(1);

    // Pick the Approve button INSIDE the queue card specifically.
    const queue = document.getElementById('purchases-approval-queue')!;
    const btn = queue.querySelector<HTMLButtonElement>('.history-approve-btn[data-approve-id="p-pending"]');
    expect(btn).toBeTruthy();
    btn?.click();
    await flushPromises();

    expect(confirmDialog).toHaveBeenCalled();
    expect(api.approvePurchase).toHaveBeenCalledWith('p-pending');
    // Reload triggers a second getHistory call.
    expect(api.getHistory).toHaveBeenCalledTimes(2);
    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'success' }));
  });

  test('Cancel click on a queue row runs confirm + api.cancelPurchase + toast + reload', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (confirmDialog as jest.Mock).mockResolvedValue(true);
    (api.cancelPurchase as jest.Mock).mockResolvedValue(undefined);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'p-pending', created_by_user_id: ADMIN_USER.id })],
    });

    await loadHistory();
    expect(api.getHistory).toHaveBeenCalledTimes(1);

    const queue = document.getElementById('purchases-approval-queue')!;
    const btn = queue.querySelector<HTMLButtonElement>('.history-cancel-btn[data-cancel-id="p-pending"]');
    expect(btn).toBeTruthy();
    btn?.click();
    await flushPromises();

    expect(confirmDialog).toHaveBeenCalled();
    expect(api.cancelPurchase).toHaveBeenCalledWith('p-pending');
    expect(api.getHistory).toHaveBeenCalledTimes(2);
    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'success' }));
  });

  test('approve API failure on a queue row surfaces error toast (no reload)', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (confirmDialog as jest.Mock).mockResolvedValue(true);
    (api.approvePurchase as jest.Mock).mockRejectedValue(new Error('boom'));
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'p-pending', created_by_user_id: ADMIN_USER.id })],
    });
    // CR-feedback (PR #387): stub console.error via jest.spyOn so the
    // mock auto-restores after the test, instead of leaking a permanent
    // override into sibling tests.
    const consoleErrorSpy = jest.spyOn(console, 'error').mockImplementation(() => {});

    try {
      await loadHistory();
      const queue = document.getElementById('purchases-approval-queue')!;
      const btn = queue.querySelector<HTMLButtonElement>('.history-approve-btn');
      btn?.click();
      await flushPromises();

      expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'error' }));
      // No reload after failure.
      expect(api.getHistory).toHaveBeenCalledTimes(1);
    } finally {
      consoleErrorSpy.mockRestore();
    }
  });

  test('pending row stays in the history table but its actions live only in the queue (issue #183)', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({ purchase_id: 'p-pending', status: 'pending', created_by_user_id: ADMIN_USER.id }),
        makeRow({ purchase_id: 'p-done', status: 'completed', created_by_user_id: ADMIN_USER.id }),
      ],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue')!;
    const list = document.getElementById('history-list')!;
    expect(queue.querySelector('tr[data-execution-id="p-pending"]')).toBeTruthy();
    const listRow = list.querySelector('tr[data-execution-id="p-pending"]')!;
    expect(listRow).toBeTruthy();
    expect(queue.querySelectorAll('.history-approve-btn[data-approve-id="p-pending"]').length).toBe(1);
    expect(queue.querySelectorAll('.history-cancel-btn[data-cancel-id="p-pending"]').length).toBe(1);
    expect(list.querySelectorAll('.history-approve-btn, .history-cancel-btn')).toHaveLength(0);
    expect(listRow.querySelector('.history-queue-link[data-queue-id="p-pending"]')).toBeTruthy();
    expect(list.querySelector('tr[data-execution-id="p-done"] .history-queue-link')).toBeNull();
  });

  test('a pending row the session cannot act on gets no queue link', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(null);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'p-pending', status: 'pending', plan_name: 'Weekly EC2' })],
    });

    await loadHistory();

    const list = document.getElementById('history-list')!;
    expect(list.querySelector('.history-queue-link')).toBeNull();
    expect(list.textContent).toContain('Weekly EC2');
  });

  describe('history queue link', () => {
    let scrollIntoView: jest.Mock;
    beforeEach(() => {
      scrollIntoView = jest.fn();
      Element.prototype.scrollIntoView = scrollIntoView as unknown as typeof Element.prototype.scrollIntoView;
      const section = document.createElement('section');
      section.id = 'purchases-approval-queue-section';
      document.body.appendChild(section);
      (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
      (api.getHistory as jest.Mock).mockResolvedValue({
        summary: {},
        purchases: [makeRow({ purchase_id: 'p-pending', status: 'pending', created_by_user_id: ADMIN_USER.id })],
      });
    });

    test('jumps to the queue row, highlights it and focuses its Approve button', async () => {
      await loadHistory();
      document.querySelector<HTMLButtonElement>('#history-list .history-queue-link')!.click();
      const queueRow = document.querySelector<HTMLElement>('#purchases-approval-queue tr[data-execution-id="p-pending"]')!;
      expect(scrollIntoView).toHaveBeenCalledTimes(1);
      expect(scrollIntoView.mock.contexts[0]).toBe(queueRow);
      expect(queueRow.classList.contains('history-row-highlight')).toBe(true);
      expect(document.activeElement).toBe(queueRow.querySelector('.history-approve-btn'));
    });

    test('falls back to the queue card when a queue filter hides the row', async () => {
      (state.getApprovalQueueColumnFilters as jest.Mock).mockReturnValueOnce({ service: { kind: 'set', values: ['rds'] } });
      await loadHistory();
      expect(document.querySelector('#purchases-approval-queue tr[data-execution-id="p-pending"]')).toBeNull();
      document.querySelector<HTMLButtonElement>('#history-list .history-queue-link')!.click();
      expect(scrollIntoView.mock.contexts[0]).toBe(document.getElementById('purchases-approval-queue-section'));
    });
  });

  // Issue #704 — new column tests

  test('renders email in Created-by column when created_by_user_email is set', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({
          purchase_id: 'p-email',
          status: 'pending',
          created_by_user_id: 'some-uuid',
          created_by_user_email: 'alice@example.com',
        }),
      ],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue')!;
    expect(queue.textContent).toContain('alice@example.com');
    // Raw UUID must NOT appear when email is available.
    expect(queue.textContent).not.toContain('some-uuid');
  });

  test('falls back to UUID in Created-by column when email is absent', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({
          purchase_id: 'p-uuid',
          status: 'pending',
          created_by_user_id: 'fallback-uuid',
          // created_by_user_email intentionally absent
        }),
      ],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue')!;
    expect(queue.textContent).toContain('fallback-uuid');
  });

  test('renders Account column via getAccountName when account_id is set', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (getAccountName as jest.Mock).mockReturnValue('Production Account');
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({
          purchase_id: 'p-acct',
          status: 'pending',
          account_id: '123456789012',
        }),
      ],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue')!;
    expect(getAccountName).toHaveBeenCalledWith('123456789012');
    expect(queue.textContent).toContain('Production Account');
  });

  test('renders Term and Payment columns', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({
          purchase_id: 'p-term',
          status: 'pending',
          term: 3,
          payment: 'no-upfront',
        }),
      ],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue')!;
    // Table cells use the compact term and the human payment label.
    expect(queue.textContent).toContain('3 yr');
    expect(queue.textContent).toContain('No Upfront');
  });

  test('renders Monthly Cost column when monthly_cost is present', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({
          purchase_id: 'p-monthly',
          status: 'pending',
          monthly_cost: 42.5,
        }),
      ],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue')!;
    // formatCurrency mock renders ".5"
    expect(queue.textContent).toContain('.5');
  });

  // Issue #733 — regression guard. PR #713 shipped the columns but the
  // backend never populated account_id / payment / monthly_cost on synthesised
  // execution rows, so every Approval-queue cell rendered as "-". The fix is
  // backend-side (handler_history.go now sources Account/Payment/MonthlyCost
  // from the rec when exec.CloudAccountID is nil and copies Payment off the
  // rec). This test pins the frontend contract end-to-end: when the API
  // returns the populated shape we expect after the fix, the cells must show
  // the real values, not the dash fallback.
  test('Approval queue cells show real values, not "-", when backend returns populated row (#733)', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (getAccountName as jest.Mock).mockReturnValue('Production Account');
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({
          purchase_id: 'p-733',
          status: 'pending',
          account_id: '123456789012',
          term: 1,
          payment: 'all-upfront',
          monthly_cost: 7.5,
          created_by_user_id: ADMIN_USER.id,
        }),
      ],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue')!;
    const queueRow = queue.querySelector('tr[data-execution-id="p-733"]')!;
    expect(queueRow).toBeTruthy();

    const cellTexts = Array.from(queueRow.querySelectorAll('td')).map((c) => c.textContent || '');
    // Find by content rather than column index so a reorder doesn't break this.
    expect(cellTexts).toEqual(expect.arrayContaining([
      expect.stringContaining('Production Account'),
      expect.stringContaining('1 yr'),
      expect.stringContaining('All Upfront'),
      expect.stringContaining('7.5'),
    ]));
    // No dash fallback may appear on the populated cells.
    expect(cellTexts).not.toContain('-');
  });

  test('renders Account/Term/Payment/Monthly column headers', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ status: 'pending', created_by_user_id: ADMIN_USER.id })],
    });

    await loadHistory();

    const queue = document.getElementById('purchases-approval-queue')!;
    const headerText = queue.querySelector('thead')?.textContent || '';
    expect(headerText).toContain('Account');
    expect(headerText).toContain('Term');
    expect(headerText).toContain('Payment');
    expect(headerText).toContain('Monthly Cost');
  });
});
