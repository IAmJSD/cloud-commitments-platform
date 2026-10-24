/**
 * Escaping-convention regressions in plans.ts innerHTML templates (issue #37).
 *
 * Uses the REAL utils escapers (plans.test.ts mocks escapeHtml as a
 * passthrough), so a value that would break out of its attribute or inject
 * markup is caught here.
 *
 *   1. #add-purchases-plan-id: the plan id reaches openAddPurchasesModal via
 *      btn.dataset['id'], which the DOM has already decoded, so the escaping
 *      at the data-id render does not protect this value="..." sink. A raw
 *      `"` would also truncate the id that handleAddPurchases POSTs.
 *   2. The planned-purchases Count cell: purchase.count is typed number but
 *      the API response is not validated at runtime.
 */
import { loadPlans, openAddPurchasesModal } from '../plans';
import * as api from '../api';

jest.mock('../api', () => ({
  getPlans: jest.fn(),
  getPlannedPurchases: jest.fn(),
  createPlannedPurchases: jest.fn(),
  listPlanAccounts: jest.fn().mockResolvedValue([]),
  listAccounts: jest.fn().mockResolvedValue([]),
  listAccountsMinimal: jest.fn().mockResolvedValue([]),
}));

jest.mock('../state', () => ({
  getRecommendations: jest.fn().mockReturnValue([]),
  getSelectedRecommendationIDs: jest.fn().mockReturnValue(new Set()),
  getVisibleRecommendations: jest.fn().mockReturnValue([]),
  setVisibleRecommendations: jest.fn(),
  getCurrentProvider: jest.fn().mockReturnValue(''),
  setCurrentProvider: jest.fn(),
  getCurrentAccountIDs: jest.fn().mockReturnValue([]),
  setCurrentAccountIDs: jest.fn(),
  subscribeProvider: jest.fn().mockReturnValue(() => {}),
  subscribeAccount: jest.fn().mockReturnValue(() => {}),
  // Administrators + Purchaser group literals (see plans.test.ts for why
  // these are not imported from permissions.ts).
  getCurrentUser: jest.fn().mockReturnValue({ id: 'u-admin', email: 'admin@example.com', groups: ['00000000-0000-5000-8000-000000000001', '00000000-0000-5000-8000-000000000007'] }),
  getPlansColumnFilters: jest.fn().mockReturnValue({}),
  setPlansColumnFilter: jest.fn(),
  clearAllPlansColumnFilters: jest.fn(),
}));

jest.mock('../history', () => ({ viewPlanHistory: jest.fn() }));

jest.mock('../toast', () => ({
  showToast: jest.fn(() => ({ dismiss: jest.fn() })),
}));

jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn(() => Promise.resolve(true)),
}));

const HOSTILE_ID = 'plan-1" autofocus onfocus="alert(1)';

beforeEach(() => {
  jest.clearAllMocks();
  document.body.innerHTML = '<div id="plans-list"></div><div id="planned-purchases-list"></div>';
});

describe('openAddPurchasesModal plan id attribute (issue #37)', () => {
  test('a quote in the plan id cannot break out of the hidden input value', async () => {
    await openAddPurchasesModal(HOSTILE_ID, 'Plan', 3);

    const input = document.getElementById('add-purchases-plan-id') as HTMLInputElement;
    expect(input).not.toBeNull();
    // The whole id round-trips as the value: nothing was truncated at the
    // quote, so the id read back for createPlannedPurchases is intact.
    expect(input.value).toBe(HOSTILE_ID);
    expect(input.hasAttribute('onfocus')).toBe(false);
    expect(input.hasAttribute('autofocus')).toBe(false);
  });

  test('the submitted plan id is the original, untruncated value', async () => {
    (api.createPlannedPurchases as jest.Mock).mockResolvedValue({});
    (api.getPlans as jest.Mock).mockResolvedValue({ plans: [] });
    (api.getPlannedPurchases as jest.Mock).mockResolvedValue({ purchases: [] });
    await openAddPurchasesModal(HOSTILE_ID, 'Plan', 0);

    const date = document.getElementById('add-purchases-start-date') as HTMLInputElement;
    const future = new Date(Date.now() + 10 * 86400000).toISOString().slice(0, 10);
    date.value = future;
    document.getElementById('add-purchases-form')?.dispatchEvent(new Event('submit', { cancelable: true }));
    await new Promise((r) => setTimeout(r, 0));

    expect(api.createPlannedPurchases).toHaveBeenCalledTimes(1);
    expect((api.createPlannedPurchases as jest.Mock).mock.calls[0][0]).toBe(HOSTILE_ID);
  });
});

describe('planned purchases Count cell (issue #37)', () => {
  function purchase(count: unknown) {
    return {
      id: 'pp-1', plan_id: 'plan-1', plan_name: 'Plan', scheduled_date: '2026-11-01',
      provider: 'aws', service: 'ec2', resource_type: 't3.medium', region: 'us-east-1',
      count, term: 1, payment: 'no-upfront', upfront_cost: 0, estimated_savings: 10,
      status: 'pending', step_number: 1, total_steps: 1,
    };
  }

  test('a non-number count from the API is rendered as text, not markup', async () => {
    (api.getPlans as jest.Mock).mockResolvedValue({ plans: [] });
    (api.getPlannedPurchases as jest.Mock).mockResolvedValue({
      purchases: [purchase('<img src=x onerror="alert(1)">')],
    });

    await loadPlans();

    const list = document.getElementById('planned-purchases-list') as HTMLElement;
    expect(list.querySelector('img')).toBeNull();
    expect(list.textContent).toContain('<img src=x onerror="alert(1)">');
  });

  test('a normal numeric count still renders', async () => {
    (api.getPlans as jest.Mock).mockResolvedValue({ plans: [] });
    (api.getPlannedPurchases as jest.Mock).mockResolvedValue({ purchases: [purchase(5)] });

    await loadPlans();

    const cells = Array.from(document.querySelectorAll('#planned-purchases-list td')).map((td) => td.textContent?.trim());
    expect(cells).toContain('5');
  });
});
