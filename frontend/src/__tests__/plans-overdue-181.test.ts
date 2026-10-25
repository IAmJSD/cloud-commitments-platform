/**
 * Issue #181: a scheduled purchase that is past its date yet still pending
 * read as an on-schedule "pending" row. The Planned Purchases table now adds
 * an Overdue badge and names the next action the user can take.
 *
 * Drives the real loadPlans() render path. Dates are far in the past /
 * future so the test does not depend on today's date.
 */
import { loadPlans } from '../plans';

jest.mock('../api', () => ({
  getPlans: jest.fn(),
  getPlannedPurchases: jest.fn(),
  listPlanAccounts: jest.fn().mockResolvedValue([]),
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
  getCurrentUser: jest.fn(),
  getPlansColumnFilters: jest.fn().mockReturnValue({}),
  setPlansColumnFilter: jest.fn(),
  clearAllPlansColumnFilters: jest.fn(),
}));

jest.mock('../history', () => ({ viewPlanHistory: jest.fn() }));

import * as api from '../api';
import * as state from '../state';

const USER_ID = 'user-1';

const plan = {
  id: 'plan-1',
  name: 'Sample Plan',
  enabled: true,
  auto_purchase: true,
  services: { ec2: { provider: 'aws', service: 'ec2', enabled: true, term: 1, payment: 'all-upfront', coverage: 80 } },
  ramp_schedule: { type: 'immediate', percent_per_step: 100, step_interval_days: 0, current_step: 1, total_steps: 1 },
};

const purchase = (id: string, scheduled_date: string, status: string) => ({
  id,
  plan_id: 'plan-1',
  plan_name: 'Sample Plan',
  scheduled_date,
  provider: 'aws',
  service: 'ec2',
  resource_type: 't3.medium',
  region: 'us-east-1',
  count: 2,
  term: 1,
  payment: 'all-upfront',
  upfront_cost: 100,
  estimated_savings: 50,
  step_number: 1,
  total_steps: 1,
  status,
  created_by_user_id: USER_ID,
  has_recommendations: true,
});

const setUser = (canRun: boolean) => {
  const effectivePermissions = [{ action: 'update', resource: 'plans' }];
  if (canRun) effectivePermissions.push({ action: 'execute', resource: 'purchases' }, { action: 'update', resource: 'purchases' });
  (state.getCurrentUser as jest.Mock).mockReturnValue({ id: USER_ID, email: 'u@example.com', groups: [], effectivePermissions });
};

// Rows are located through their action buttons' data-id (run perms needed).
const row = (id: string): HTMLTableRowElement => {
  const tr = document.querySelector(`#planned-purchases-list [data-id="${id}"]`)?.closest('tr');
  if (!tr) throw new Error(`row ${id} not found`);
  return tr;
};

describe('Planned Purchases: overdue pending rows (#181)', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    const list = document.createElement('div');
    list.id = 'plans-list';
    const planned = document.createElement('div');
    planned.id = 'planned-purchases-list';
    document.body.replaceChildren(list, planned);
    (api.getPlans as jest.Mock).mockResolvedValue({ plans: [plan] });
  });

  test('a pending row past its date gets an Overdue badge and a next action', async () => {
    setUser(true);
    (api.getPlannedPurchases as jest.Mock).mockResolvedValue({
      purchases: [purchase('pp-late', '2000-01-03', 'pending'), purchase('pp-future', '2999-01-03', 'pending')],
    });
    await loadPlans();
    const late = row('pp-late');
    expect(late.querySelector('.badge-danger')?.textContent).toBe('Overdue');
    expect(late.querySelector('.overdue-hint')?.textContent).toBe('Not run on its date. Run now or pause.');
    const future = row('pp-future');
    expect(future.querySelector('.badge-danger')).toBeNull();
    expect(future.querySelector('.overdue-hint')).toBeNull();
  });

  test('without run permission the hint does not offer an action the user cannot take', async () => {
    setUser(false);
    (api.getPlannedPurchases as jest.Mock).mockResolvedValue({ purchases: [purchase('pp-late', '2000-01-03', 'notified')] });
    await loadPlans();
    const hint = document.querySelector('#planned-purchases-list .overdue-hint');
    expect(hint?.textContent).toBe('Not run on its date.');
  });

  test('a paused row past its date is not flagged (pausing is the user\'s choice)', async () => {
    setUser(true);
    (api.getPlannedPurchases as jest.Mock).mockResolvedValue({ purchases: [purchase('pp-paused', '2000-01-03', 'paused')] });
    await loadPlans();
    expect(document.querySelector('#planned-purchases-list .badge-danger')).toBeNull();
  });
});
