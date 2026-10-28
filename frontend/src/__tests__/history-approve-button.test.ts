/**
 * History inline Approve button tests (issue #286).
 *
 * Mirror of the backend RBAC matrix in
 * internal/api/handler_purchases.go::authorizeSessionApprove — keep
 * both sides in sync. The backend remains authoritative; these tests
 * only verify the UX gate (don't render buttons users can't use).
 *
 * Tested matrix:
 *   1. admin sees Approve on every pending row, regardless of creator.
 *   2. regular user sees Approve only on rows they themselves created.
 *   3. Anonymous (no current user cached) sees no Approve buttons.
 *   4. Approve button absent for non-pending rows (completed, cancelled, ...).
 *   5. Click + decline confirmDialog → no API call.
 *   6. Click + accept → approvePurchase + reload + toast.
 *   7. approvePurchase rejects → toast.error + button re-enabled.
 *   8. Approve and Cancel render together for pending rows where the
 *      session qualifies for both verbs.
 */

import { loadHistory, setupHistoryHandlers, viewPlanHistory } from '../history';

jest.mock('../api', () => ({
  getHistory: jest.fn(),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  approvePurchase: jest.fn(),
  cancelPurchase: jest.fn(),
  getPurchaseDetails: jest.fn(),
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
  // Issue #344 T2: history.ts reads provider/account from the global
  // topbar filter via state. Default provider 'all' + no account scope
  // preserves the pre-topbar behaviour these tests assume.
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

import * as api from '../api';
import { confirmDialog } from '../confirmDialog';
import { showToast } from '../toast';
import { getCurrentUser, subscribeAmortizeUpfront } from '../state';
import { ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID } from '../permissions';
import { flushPromises } from './flush-promises';

// Admin user includes Purchaser membership (mirrors the auto-migration for
// existing admins on first deploy of issue #923). approve-any:purchases is
// carved out of admin:* and requires Purchaser group membership.
const ADMIN_USER = { id: 'admin-uuid', email: 'admin@example.com', groups: [ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID] };
// Admin without Purchaser membership and without effectivePermissions
// for any carved-out spending verb. Issue #923 explicitly carves
// approve-any:purchases / retry-any:purchases / execute:purchases OUT
// of admin:*, so this user MUST NOT see Approve / Retry buttons on
// rows they did not create. Regression guard for CR #924 F5 — if a
// future refactor reintroduces isAdmin() as the gate, this test
// catches it.
const ADMIN_NO_PURCH = { id: 'admin-no-purch-uuid', email: 'admin-no-purch@example.com', groups: [ADMINISTRATORS_GROUP_ID] };
// REG_USER carries the default-user effective permission set (approve-own
// + cancel-own + retry-own on purchases) so canAccess returns true for
// own-row actions without needing the bootstrap fetch. The previous
// `groups: []` shape relied on the pre-#917 role-string gate that no
// longer exists; the post-rebase canAccess only honors the loaded
// effectivePermissions, so the test fixture must populate it.
const REG_USER = {
  id: 'user-uuid',
  email: 'user@example.com',
  groups: [],
  effectivePermissions: [
    { action: 'approve-own', resource: 'purchases' },
    { action: 'cancel-own', resource: 'purchases' },
    { action: 'retry-own', resource: 'purchases' },
    { action: 'view', resource: 'history' },
  ],
};
const OTHER_UUID = 'other-uuid';
const DETAILS = {
  total_upfront_cost: 100,
  estimated_savings: 10,
  recommendations: [{ provider: 'aws', service: 'ec2', region: 'us-east-1', count: 1, term: 1, payment: 'all-upfront', upfront_cost: 100, savings: 10 }],
};

function deferred<T>(): { promise: Promise<T>; resolve(value: T): void; reject(error: Error): void } {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

const settleActions = (): Promise<void> => new Promise(resolve => setTimeout(resolve, 0));

describe('History action ownership and refresh recovery (#249)', () => {
  const data = { summary: {}, purchases: [makeRow({ purchase_id: 'guarded' })] };
  beforeEach(() => {
    setupDOM();
    jest.clearAllMocks();
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockReset().mockResolvedValue(data);
    (api.getPurchaseDetails as jest.Mock).mockReset().mockResolvedValue(DETAILS);
    (api.approvePurchase as jest.Mock).mockReset().mockResolvedValue(undefined);
    (confirmDialog as jest.Mock).mockReset().mockResolvedValue(true);
    (showToast as jest.Mock).mockReset();
  });

  test('details fetch owns both actions and same-ID projections while other IDs remain independent', async () => {
    const details = deferred<typeof DETAILS>();
    (api.getPurchaseDetails as jest.Mock).mockReturnValue(details.promise);
    (api.getHistory as jest.Mock).mockResolvedValue({ ...data, purchases: [...data.purchases, makeRow({ purchase_id: 'other' })] });
    (confirmDialog as jest.Mock).mockResolvedValue(false);
    await loadHistory();
    const btn = document.querySelector<HTMLButtonElement>('#purchases-approval-queue [data-approve-id="guarded"]')!;
    btn.click();
    btn.dispatchEvent(new MouseEvent('click'));
    document.querySelector<HTMLButtonElement>('#purchases-approval-queue [data-cancel-id="guarded"]')!.click();
    document.querySelector<HTMLButtonElement>('#purchases-approval-queue [data-approve-id="guarded"]')!.click();
    document.querySelector<HTMLButtonElement>('#purchases-approval-queue [data-approve-id="other"]')!.click();
    const disabled = btn.disabled;
    details.resolve(DETAILS);
    await settleActions();
    expect(disabled).toBe(true);
    expect(api.getPurchaseDetails).toHaveBeenCalledTimes(2);
    expect(confirmDialog).toHaveBeenCalledTimes(2);
    expect(api.approvePurchase).not.toHaveBeenCalled();
    expect(api.cancelPurchase).not.toHaveBeenCalled();
    expect(btn.disabled).toBe(false);
  });

  test.each([
    ['the details fetch fails', () => (api.getPurchaseDetails as jest.Mock).mockRejectedValue(new Error('403 Forbidden'))],
    ['the details carry no recommendations', () => (api.getPurchaseDetails as jest.Mock).mockResolvedValue({ ...DETAILS, recommendations: [] })],
  ])('Approve is not offered when %s (issue #247)', async (_label, arrange) => {
    arrange();
    console.error = jest.fn();
    await loadHistory();
    const btn = document.querySelector<HTMLButtonElement>('#purchases-approval-queue .history-approve-btn')!;
    btn.click();
    await settleActions();
    expect(confirmDialog).not.toHaveBeenCalled();
    expect(api.approvePurchase).not.toHaveBeenCalled();
    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({
      kind: 'error',
      message: expect.stringContaining('cannot be approved without showing the amount'),
    }));
    expect(btn.disabled).toBe(false);
  });

  test.each(['cancel', 'reject', 'throw', 'api-error'] as const)('%s releases ownership for a later explicit attempt', async outcome => {
    await loadHistory();
    const btn = document.querySelector<HTMLButtonElement>('#purchases-approval-queue .history-approve-btn')!;
    if (outcome === 'cancel') (confirmDialog as jest.Mock).mockResolvedValueOnce(false);
    if (outcome === 'reject') (confirmDialog as jest.Mock).mockRejectedValueOnce(new Error('dialog failed'));
    if (outcome === 'throw') (confirmDialog as jest.Mock).mockImplementationOnce(() => { throw new Error('dialog failed'); });
    if (outcome === 'api-error') (api.approvePurchase as jest.Mock).mockRejectedValueOnce(new Error('mutation failed'));
    btn.click();
    await settleActions();
    expect(btn.disabled).toBe(false);
    const count = outcome === 'api-error' ? 1 : 0;
    expect(api.approvePurchase).toHaveBeenCalledTimes(count);
    btn.click();
    await settleActions();
    expect(api.approvePurchase).toHaveBeenCalledTimes(count + 1);
  });

  test('API remains guarded across a cached re-render and direct disabled dispatch', async () => {
    const mutation = deferred<void>();
    (api.approvePurchase as jest.Mock).mockReturnValue(mutation.promise);
    setupHistoryHandlers();
    await loadHistory();
    const btn = document.querySelector<HTMLButtonElement>('#purchases-approval-queue .history-approve-btn')!;
    btn.click();
    await settleActions();
    const redraw = (subscribeAmortizeUpfront as jest.Mock).mock.calls[0]![0] as () => void;
    redraw();
    document.querySelector<HTMLButtonElement>('#purchases-approval-queue .history-approve-btn')!.click();
    document.querySelector<HTMLButtonElement>('#purchases-approval-queue .history-cancel-btn')!.click();
    btn.dispatchEvent(new MouseEvent('click'));
    mutation.resolve();
    await settleActions();
    expect(api.approvePurchase).toHaveBeenCalledTimes(1);
    expect(api.cancelPurchase).not.toHaveBeenCalled();
  });

  test.each([false, true])('success survives failed refresh and cached redraw (toast throws=%s)', async toastThrows => {
    setupHistoryHandlers();
    await loadHistory();
    (api.getHistory as jest.Mock).mockRejectedValueOnce(new Error('refresh failed'));
    if (toastThrows) (showToast as jest.Mock).mockImplementationOnce(() => { throw new Error('toast failed'); });
    document.querySelector<HTMLButtonElement>('#purchases-approval-queue .history-approve-btn')!.click();
    await settleActions();
    const redraw = (subscribeAmortizeUpfront as jest.Mock).mock.calls[0]![0] as () => void;
    redraw();
    expect(document.querySelectorAll('[data-approve-id="guarded"]')).toHaveLength(0);
    expect(api.approvePurchase).toHaveBeenCalledTimes(1);
    expect(api.getHistory).toHaveBeenCalledTimes(2);
    expect((showToast as jest.Mock).mock.calls.some(([opts]) => opts.message?.startsWith('Failed to approve'))).toBe(false);
    await loadHistory();
    expect(document.querySelectorAll('[data-approve-id="guarded"]')).toHaveLength(1);
  });

  test.each(['history', 'plan'] as const)('%s loader cannot publish responses started before a successful mutation', async loader => {
    const old = deferred<typeof data>();
    setupHistoryHandlers();
    await loadHistory();
    (api.getHistory as jest.Mock).mockReturnValueOnce(old.promise);
    const oldLoad = loader === 'history' ? loadHistory() : viewPlanHistory('plan-1');
    const redraw = (subscribeAmortizeUpfront as jest.Mock).mock.calls[0]![0] as () => void;
    redraw();
    (api.getHistory as jest.Mock).mockRejectedValueOnce(new Error('refresh failed'));
    document.querySelector<HTMLButtonElement>('#purchases-approval-queue .history-approve-btn')!.click();
    await settleActions();
    old.resolve(data);
    await oldLoad;
    redraw();
    expect(document.querySelectorAll('[data-approve-id="guarded"]')).toHaveLength(0);
    await loadHistory();
    expect(document.querySelectorAll('[data-approve-id="guarded"]')).toHaveLength(1);
  });

  test.each(['history', 'plan'] as const)('%s loader ignores old errors after a successful mutation refresh', async loader => {
    const old = deferred<typeof data>();
    setupHistoryHandlers();
    await loadHistory();
    (api.getHistory as jest.Mock).mockReturnValueOnce(old.promise);
    const oldLoad = loader === 'history' ? loadHistory() : viewPlanHistory('plan-1');
    ((subscribeAmortizeUpfront as jest.Mock).mock.calls[0]![0] as () => void)();
    document.querySelector<HTMLButtonElement>('#purchases-approval-queue .history-approve-btn')!.click();
    await settleActions();
    old.reject(new Error('old failure'));
    await oldLoad;
    expect(document.querySelectorAll('[data-approve-id="guarded"]')).toHaveLength(1);
    expect(document.getElementById('history-list')!.textContent).not.toContain('old failure');
  });
});

function setupDOM(): void {
  while (document.body.firstChild) document.body.removeChild(document.body.firstChild);

  const mkInput = (id: string): HTMLInputElement => {
    const el = document.createElement('input');
    el.type = 'date';
    el.id = id;
    return el;
  };
  const mkSelect = (id: string): HTMLSelectElement => {
    const el = document.createElement('select');
    el.id = id;
    const opt = document.createElement('option');
    opt.value = '';
    opt.textContent = 'All';
    el.appendChild(opt);
    return el;
  };
  const mkDiv = (id: string): HTMLDivElement => {
    const el = document.createElement('div');
    el.id = id;
    return el;
  };

  document.body.appendChild(mkInput('history-start'));
  document.body.appendChild(mkInput('history-end'));
  document.body.appendChild(mkSelect('history-provider-filter'));
  document.body.appendChild(mkSelect('history-account-filter'));
  document.body.appendChild(mkDiv('history-summary'));
  document.body.appendChild(mkDiv('history-list'));
  // Issue #340 sub-task: loadHistory now also paints the approval-queue
  // card. The container must exist so renderApprovalQueue's
  // getElementById lookup succeeds.
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

describe('History inline Approve button (issue #286)', () => {
  beforeEach(() => {
    setupDOM();
    jest.clearAllMocks();
    (api.getPurchaseDetails as jest.Mock).mockResolvedValue(DETAILS);
  });

  test('admin sees Approve on every pending row, regardless of creator', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({ purchase_id: 'exec-mine', created_by_user_id: ADMIN_USER.id }),
        makeRow({ purchase_id: 'exec-other', created_by_user_id: OTHER_UUID }),
        makeRow({ purchase_id: 'exec-legacy', created_by_user_id: undefined }),
      ],
    });

    await loadHistory();

    // The Approval queue card is the only approval surface (issue #183).
    const list = document.getElementById('purchases-approval-queue')!;
    const buttons = list.querySelectorAll<HTMLButtonElement>('.history-approve-btn');
    const ids = Array.from(buttons).map((b) => b.dataset['approveId']);
    expect(ids).toEqual(expect.arrayContaining(['exec-mine', 'exec-other', 'exec-legacy']));
    expect(ids).toHaveLength(3);
  });

  test('regular user sees Approve only on rows they created', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(REG_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({ purchase_id: 'exec-mine', created_by_user_id: REG_USER.id }),
        makeRow({ purchase_id: 'exec-other', created_by_user_id: OTHER_UUID }),
        makeRow({ purchase_id: 'exec-legacy', created_by_user_id: undefined }),
      ],
    });

    await loadHistory();

    // The Approval queue card is the only approval surface (issue #183).
    const list = document.getElementById('purchases-approval-queue')!;
    const buttons = list.querySelectorAll<HTMLButtonElement>('.history-approve-btn');
    const ids = Array.from(buttons).map((b) => b.dataset['approveId']);
    expect(ids).toEqual(['exec-mine']);
  });

  test('renders no Approve buttons when no current user is cached', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(null);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ created_by_user_id: 'anything' })],
    });

    await loadHistory();

    expect(document.querySelectorAll('.history-approve-btn')).toHaveLength(0);
  });

  test('Approve button absent on non-pending rows even for admin', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({ purchase_id: 'exec-completed', status: 'completed', created_by_user_id: ADMIN_USER.id }),
        makeRow({ purchase_id: 'exec-cancelled', status: 'cancelled', created_by_user_id: ADMIN_USER.id }),
        makeRow({ purchase_id: 'exec-failed', status: 'failed', created_by_user_id: ADMIN_USER.id }),
        // Sanity: notified is treated like pending and SHOULD render the button.
        makeRow({ purchase_id: 'exec-notified', status: 'notified', created_by_user_id: ADMIN_USER.id }),
      ],
    });

    await loadHistory();

    // The Approval queue card is the only approval surface (issue #183).
    const list = document.getElementById('purchases-approval-queue')!;
    const ids = Array.from(list.querySelectorAll<HTMLButtonElement>('.history-approve-btn'))
      .map((b) => b.dataset['approveId']);
    expect(ids).toEqual(['exec-notified']);
  });

  test('Approve and Cancel render side-by-side for pending rows the session qualifies for', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(REG_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'exec-1', created_by_user_id: REG_USER.id })],
    });

    await loadHistory();

    // The Approval queue card is the only approval surface (issue #183).
    const list = document.getElementById('purchases-approval-queue')!;
    const approveBtns = list.querySelectorAll<HTMLButtonElement>('.history-approve-btn[data-approve-id]');
    const cancelBtns = list.querySelectorAll<HTMLButtonElement>('.history-cancel-btn[data-cancel-id]');
    expect(approveBtns).toHaveLength(1);
    expect(cancelBtns).toHaveLength(1);
    expect(approveBtns[0]?.dataset['approveId']).toBe('exec-1');
    expect(cancelBtns[0]?.dataset['cancelId']).toBe('exec-1');
  });

  test('declined confirm dialog skips API call', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (confirmDialog as jest.Mock).mockResolvedValue(false);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'exec-1', created_by_user_id: ADMIN_USER.id })],
    });

    await loadHistory();
    const btn = document.querySelector<HTMLButtonElement>('.history-approve-btn');
    btn?.click();
    await new Promise((r) => setTimeout(r, 0));

    expect(confirmDialog).toHaveBeenCalled();
    expect(api.approvePurchase).not.toHaveBeenCalled();
  });

  test('accepted confirm posts approve + reloads + toasts', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (confirmDialog as jest.Mock).mockResolvedValue(true);
    (api.approvePurchase as jest.Mock).mockResolvedValue(undefined);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'exec-1', created_by_user_id: ADMIN_USER.id })],
    });

    await loadHistory();
    expect(api.getHistory).toHaveBeenCalledTimes(1);

    const btn = document.querySelector<HTMLButtonElement>('.history-approve-btn');
    btn?.click();
    await flushPromises();

    expect(api.approvePurchase).toHaveBeenCalledWith('exec-1');
    // Reload was triggered → second getHistory call.
    expect(api.getHistory).toHaveBeenCalledTimes(2);
    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'success' }));
  });

  test('approve API failure surfaces toast and re-enables the button', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (confirmDialog as jest.Mock).mockResolvedValue(true);
    (api.approvePurchase as jest.Mock).mockRejectedValue(new Error('boom'));
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'exec-1', created_by_user_id: ADMIN_USER.id })],
    });
    console.error = jest.fn();

    await loadHistory();
    const btn = document.querySelector<HTMLButtonElement>('.history-approve-btn');
    btn?.click();
    await flushPromises();

    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'error' }));
    expect(btn?.disabled).toBe(false);
  });

  test('approve click disables BOTH Approve and Cancel for the row while in flight (CR pass)', async () => {
    // Issue #286 + CR pass on PR #299: Approve and Cancel can render
    // together. Clicking Approve should disable BOTH buttons until the
    // request completes — otherwise a quick double-click could fire
    // a conflicting Cancel on the same row before the reload runs.
    (getCurrentUser as jest.Mock).mockReturnValue(REG_USER);
    (confirmDialog as jest.Mock).mockResolvedValue(true);
    let resolveApprove: (() => void) | undefined;
    (api.approvePurchase as jest.Mock).mockReturnValue(
      new Promise<void>((r) => { resolveApprove = r; }),
    );
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'exec-1', created_by_user_id: REG_USER.id })],
    });

    await loadHistory();
    // Scope to the queue for the disable assertion. The click
    // handler scopes its disable to the clicked button's containing
    // <td> (sameRowActions), so we need both buttons from the SAME row
    // of the SAME view to verify the pair-disable behaviour.
    const list = document.getElementById('purchases-approval-queue')!;
    const approveBtn = list.querySelector<HTMLButtonElement>('.history-approve-btn');
    const cancelBtn = list.querySelector<HTMLButtonElement>('.history-cancel-btn');
    expect(approveBtn).not.toBeNull();
    expect(cancelBtn).not.toBeNull();
    expect(approveBtn?.disabled).toBe(false);
    expect(cancelBtn?.disabled).toBe(false);

    approveBtn?.click();
    // Let the click handler reach the disable step (microtasks for the
    // confirmDialog await chain).
    await new Promise((r) => setTimeout(r, 0));
    await new Promise((r) => setTimeout(r, 0));

    // While the API call is unresolved, BOTH buttons are disabled.
    expect(approveBtn?.disabled).toBe(true);
    expect(cancelBtn?.disabled).toBe(true);

    // Resolve the in-flight promise + let the success path complete.
    resolveApprove?.();
    await flushPromises();
  });

  test('approve API failure re-enables BOTH Approve and Cancel for the row (CR pass)', async () => {
    (getCurrentUser as jest.Mock).mockReturnValue(REG_USER);
    (confirmDialog as jest.Mock).mockResolvedValue(true);
    (api.approvePurchase as jest.Mock).mockRejectedValue(new Error('boom'));
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [makeRow({ purchase_id: 'exec-1', created_by_user_id: REG_USER.id })],
    });
    console.error = jest.fn();

    await loadHistory();
    const approveBtn = document.querySelector<HTMLButtonElement>('.history-approve-btn');
    const cancelBtn = document.querySelector<HTMLButtonElement>('.history-cancel-btn');
    approveBtn?.click();
    await flushPromises();

    // After the API rejects, BOTH buttons must be re-enabled.
    expect(approveBtn?.disabled).toBe(false);
    expect(cancelBtn?.disabled).toBe(false);
    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'error' }));
  });

  test('user without any approve permission sees NO Approve button even on their own rows (issue #1407 four-eyes)', async () => {
    // Regression guard for issue #1407. Before the fix, canApprovePendingRow
    // returned true based on ownership alone (created_by_user_id === user.id)
    // without verifying that the session holds approve-own or approve-any.
    // This caused Viewer-role users (no approve-* in effectivePermissions) to
    // see the Approve button on purchases they submitted.
    //
    // After the fix, approve-own must be checked explicitly before ownership is
    // evaluated; a session without it sees no Approve buttons at all.
    const VIEWER_USER = {
      id: 'viewer-uuid',
      email: 'viewer@example.com',
      groups: [],
      effectivePermissions: [
        { action: 'view', resource: 'recommendations' },
        { action: 'view', resource: 'plans' },
        { action: 'view', resource: 'history' },
        // cancel-own / retry-own / revoke-own present; approve-own intentionally absent.
        { action: 'cancel-own', resource: 'purchases' },
      ],
    };

    (getCurrentUser as jest.Mock).mockReturnValue(VIEWER_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        // Own row: before fix this showed Approve; after fix it must NOT.
        makeRow({ purchase_id: 'exec-mine', created_by_user_id: VIEWER_USER.id }),
        makeRow({ purchase_id: 'exec-other', created_by_user_id: OTHER_UUID }),
      ],
    });

    await loadHistory();

    // The Approval queue card is the only approval surface (issue #183).
    const list = document.getElementById('purchases-approval-queue')!;
    const buttons = list.querySelectorAll<HTMLButtonElement>('.history-approve-btn');
    // No Approve buttons must appear for any row when the session has no
    // approve-own or approve-any permission.
    expect(buttons).toHaveLength(0);
  });

  test('admin WITHOUT Purchaser membership does not see Approve on rows they did not create (CR #924 F5)', async () => {
    // Issue #923 + CR #924 F5: approve-any:purchases is carved out of
    // admin:*. canApprovePendingRow must gate on
    // canAccess('approve-any', 'purchases'), NOT on isAdmin() or
    // isPurchaser() group membership alone. A bare admin (no Purchaser
    // group, no effectivePermissions yet) is exactly the case where
    // the carve-out matters: the legacy implementation would have
    // shown Approve on every pending row.
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_NO_PURCH);
    (api.getHistory as jest.Mock).mockResolvedValue({
      summary: {},
      purchases: [
        makeRow({ purchase_id: 'exec-mine', created_by_user_id: ADMIN_NO_PURCH.id }),
        makeRow({ purchase_id: 'exec-other', created_by_user_id: OTHER_UUID }),
        makeRow({ purchase_id: 'exec-legacy', created_by_user_id: undefined }),
      ],
    });

    await loadHistory();

    // The Approval queue card is the only approval surface (issue #183).
    const list = document.getElementById('purchases-approval-queue')!;
    const buttons = list.querySelectorAll<HTMLButtonElement>('.history-approve-btn');
    const ids = Array.from(buttons).map((b) => b.dataset['approveId']);
    // Approve renders only via the approve-own fallback (matching
    // created_by_user_id), NOT approve-any.
    expect(ids).toEqual(['exec-mine']);
  });
});

// #320: _fourEyesMode starts ON, matching the fail-closed default that
// refreshFourEyesMode applies when the config is unreadable. A render that
// runs before the first config load must not offer the creator Approve.
describe('dual-control default before the first config load (#320)', () => {
  test('renderApprovalQueue hides Approve on the creator\'s own row until the config says dual control is off', async () => {
    setupDOM();
    let history!: typeof import('../history');
    let freshState!: { getCurrentUser: jest.Mock };
    let freshApi!: { getConfig: jest.Mock; getHistory: jest.Mock };
    jest.isolateModules(() => {
      freshState = jest.requireMock('../state');
      freshApi = jest.requireMock('../api');
      history = jest.requireActual('../history');
    });
    freshState.getCurrentUser.mockReturnValue(REG_USER);
    const row = makeRow({ purchase_id: 'exec-own', created_by_user_id: REG_USER.id });
    const approveIds = (): (string | undefined)[] => Array.from(
      document.querySelectorAll<HTMLButtonElement>('#purchases-approval-queue .history-approve-btn'),
      b => b.dataset['approveId'],
    );

    history.renderApprovalQueue([row] as never);
    expect(approveIds()).toEqual([]);

    freshApi.getConfig.mockResolvedValue({ global: { require_different_approver: false } });
    freshApi.getHistory.mockResolvedValue({ summary: {}, purchases: [row] });
    await history.loadHistory();
    expect(approveIds()).toEqual(['exec-own']);
  });
});
