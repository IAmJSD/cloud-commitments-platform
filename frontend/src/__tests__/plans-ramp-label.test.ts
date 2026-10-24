/**
 * Plan list ramp label agrees with Edit (#1185).
 *
 * A preset name ("Weekly 25%") is shown only when type, percent AND interval
 * match the preset, the same rule Edit uses to pick the ramp radio; anything
 * else reads "Custom N% every D days". The generated plan name counts steps
 * like the backend (floor), so 30% is 3 steps. Uses the real index.html.
 */
import * as fs from 'fs';
import * as path from 'path';
import { loadPlans, openCreatePlanModal, setupPlanHandlers, _resetRampHandlersForTest } from '../plans';

jest.mock('../api', () => ({
  getPlans: jest.fn(),
  getPlan: jest.fn(),
  getPlannedPurchases: jest.fn().mockResolvedValue({ purchases: [] }),
  listPlanAccounts: jest.fn().mockResolvedValue([]),
  listAccounts: jest.fn().mockResolvedValue([]),
  listAccountsMinimal: jest.fn().mockResolvedValue([]),
  getAccount: jest.fn().mockResolvedValue(null),
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
  // Literal ids (jest.mock is hoisted): administrators + purchasers groups.
  getCurrentUser: jest.fn().mockReturnValue({ id: 'u-admin', email: 'admin@example.com', groups: ['00000000-0000-5000-8000-000000000001', '00000000-0000-5000-8000-000000000007'] }),
  getPlansColumnFilters: jest.fn().mockReturnValue({}),
  setPlansColumnFilter: jest.fn(),
  clearAllPlansColumnFilters: jest.fn(),
}));

jest.mock('../history', () => ({ viewPlanHistory: jest.fn() }));
jest.mock('../archera', () => ({ openArcheraOfferModal: jest.fn() }));
jest.mock('../toast', () => ({ showToast: jest.fn(() => ({ dismiss: jest.fn() })) }));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn(() => Promise.resolve(true)) }));

import * as api from '../api';

function loadRealIndexBody(): string {
  const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
  return new DOMParser().parseFromString(html, 'text/html').body.innerHTML;
}

function plan(type: string, percent: number, interval: number): Record<string, unknown> {
  return {
    id: 'plan-1', name: 'Ramp Plan', enabled: true, auto_purchase: false, notification_days_before: 3,
    services: { ec2: { provider: 'aws', service: 'ec2', enabled: true, term: 1, payment: 'all-upfront', coverage: 80 } },
    ramp_schedule: { type, percent_per_step: percent, step_interval_days: interval, current_step: 0, total_steps: Math.floor(100 / percent) },
  };
}

function listedRampLabel(): string {
  const detail = Array.from(document.querySelectorAll('.plan-detail'))
    .find(el => el.querySelector('.plan-detail-label')?.textContent === 'Ramp Schedule');
  return detail?.querySelector('.plan-detail-value')?.textContent ?? '';
}

describe('plan list ramp label (#1185)', () => {
  beforeEach(() => {
    document.body.innerHTML = loadRealIndexBody();
    jest.clearAllMocks();
    _resetRampHandlersForTest();
  });

  test.each([
    ['weekly', 25, 7, 'Weekly 25%', 'weekly-25pct'],
    ['monthly', 10, 30, 'Monthly 10%', 'monthly-10pct'],
    ['weekly', 20, 7, 'Custom 20% every 7 days', 'custom'],
    ['weekly', 25, 14, 'Custom 25% every 14 days', 'custom'],
    ['custom', 25, 7, 'Custom 25% every 7 days', 'custom'],
    ['custom', 50, 1, 'Custom 50% every 1 day', 'custom'],
    ['immediate', 100, 0, 'Immediate', 'immediate'],
  ])('stored %s %d%%/%dd lists as %p and Edit opens %s', async (type, percent, interval, label, radio) => {
    const stored = plan(type, percent, interval);
    (api.getPlans as jest.Mock).mockResolvedValue({ plans: [stored] });
    (api.getPlan as jest.Mock).mockResolvedValue(stored);
    setupPlanHandlers();

    await loadPlans();
    expect(listedRampLabel()).toBe(label);

    (document.querySelector('[data-action="edit-plan"]') as HTMLButtonElement).click();
    await new Promise(resolve => setTimeout(resolve, 50));
    expect((document.querySelector('input[name="ramp-schedule"]:checked') as HTMLInputElement | null)?.value).toBe(radio);
  });
});

describe('generated plan name step count (#1185)', () => {
  beforeEach(() => {
    document.body.innerHTML = loadRealIndexBody();
    jest.clearAllMocks();
    _resetRampHandlersForTest();
  });

  test.each([
    ['30', '3 steps'],
    ['25', '4 steps'],
    ['60', '1 step'],
  ])('custom %s%% names the plan with %s, matching the backend', (percent, steps) => {
    setupPlanHandlers();
    openCreatePlanModal();
    const custom = document.querySelector('input[value="custom"]') as HTMLInputElement;
    custom.checked = true;
    custom.dispatchEvent(new Event('change', { bubbles: true }));
    const step = document.getElementById('ramp-step-percent') as HTMLInputElement;
    step.value = percent;
    step.dispatchEvent(new Event('input'));

    expect((document.getElementById('plan-name') as HTMLInputElement).value).toContain(`(${steps})`);
  });
});
