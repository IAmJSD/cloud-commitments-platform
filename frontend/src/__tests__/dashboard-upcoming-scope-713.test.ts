/**
 * Issue #713 (second half): the Home upcoming-purchases widget ignored the
 * topbar chips. GET /api/dashboard/upcoming takes no filter parameters, so
 * the provider chip is applied client-side and an active account chip is
 * flagged as not applied rather than silently ignored.
 *
 * Drives loadDashboard() so the production render path is exercised.
 */

jest.mock('../toast', () => ({ showToast: jest.fn(() => ({ dismiss: jest.fn() })) }));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn(() => Promise.resolve(true)) }));
jest.mock('chart.js', () => {
  const MockChart = jest.fn().mockImplementation(() => ({ destroy: jest.fn() }));
  (MockChart as unknown as { register: jest.Mock }).register = jest.fn();
  return { Chart: MockChart, registerables: [] };
});
jest.mock('../recommendations', () => ({
  groupRecsByCell: jest.fn(() => new Map()),
  pageLevelRange: jest.fn(() => ({ savingsMin: 0, savingsMax: 0, cellCount: 0 })),
  formatSavingsRange: jest.fn((min: number, max: number) => `$${min}-$${max}`),
  triggerAutoRefreshIfStale: jest.fn(() => Promise.resolve()),
}));
jest.mock('../api', () => ({
  getDashboardSummary: jest.fn().mockResolvedValue({ potential_monthly_savings: 0, by_service: {} }),
  getUpcomingPurchases: jest.fn(),
  getSavingsAnalytics: jest.fn().mockResolvedValue({ data_points: [] }),
  getRecommendations: jest.fn().mockResolvedValue([]),
}));
jest.mock('../state', () => ({
  getCurrentProvider: jest.fn().mockReturnValue(''),
  getCurrentAccountIDs: jest.fn().mockReturnValue([]),
  getSavingsChart: jest.fn().mockReturnValue(null),
  setSavingsChart: jest.fn(),
  subscribeProvider: jest.fn().mockReturnValue(() => {}),
  subscribeAccount: jest.fn().mockReturnValue(() => {}),
  getCurrentUser: jest.fn().mockReturnValue(null),
}));

import { loadDashboard } from '../dashboard';
import * as api from '../api';
import * as state from '../state';
import { scopeUpcomingToProvider, upcomingEmptyMessage, UPCOMING_ACCOUNT_SCOPE_NOTE } from '../lib/upcoming-scope';
import type { UpcomingPurchase } from '../types';

const row = (execution_id: string, provider: UpcomingPurchase['provider'], plan_name: string): UpcomingPurchase => ({
  execution_id,
  plan_id: `plan-${execution_id}`,
  plan_name,
  scheduled_date: '2026-11-01',
  provider,
  service: 'compute',
  step_number: 1,
  total_steps: 2,
  estimated_savings: 50,
});

const ROWS = [row('e-aws', 'aws', 'AWS Plan'), row('e-az', 'azure', 'Azure Plan')];

const setScope = (provider: string, accountIDs: string[] = []) => {
  (state.getCurrentProvider as jest.Mock).mockReturnValue(provider);
  (state.getCurrentAccountIDs as jest.Mock).mockReturnValue(accountIDs);
};

const list = () => document.getElementById('upcoming-list') as HTMLElement;
const planNames = () => Array.from(list().querySelectorAll('.upcoming-details h4')).map(h => h.textContent);

describe('upcoming-scope helpers (#713)', () => {
  test('no provider filter keeps every row and does not alias the input', () => {
    for (const p of ['', 'all', 'ALL']) {
      const out = scopeUpcomingToProvider(ROWS, p);
      expect(out).toEqual(ROWS);
      expect(out).not.toBe(ROWS);
    }
  });

  test('provider filter keeps only matching rows, case-insensitively', () => {
    expect(scopeUpcomingToProvider(ROWS, 'azure').map(r => r.execution_id)).toEqual(['e-az']);
    expect(scopeUpcomingToProvider(ROWS, 'AWS').map(r => r.execution_id)).toEqual(['e-aws']);
    expect(scopeUpcomingToProvider(ROWS, 'gcp')).toEqual([]);
  });

  test('empty message names the provider only when one is filtered', () => {
    expect(upcomingEmptyMessage('')).toBe('No upcoming scheduled purchases');
    expect(upcomingEmptyMessage('all')).toBe('No upcoming scheduled purchases');
    expect(upcomingEmptyMessage('gcp')).toBe('No upcoming scheduled purchases for GCP');
  });
});

describe('Home upcoming widget follows the topbar chips (#713)', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    document.body.innerHTML = `
      <div id="summary"></div>
      <section id="savings-by-service-section">
        <canvas id="savings-by-service-chart"></canvas>
        <p id="savings-by-service-empty" class="empty hidden"></p>
      </section>
      <div id="upcoming-list"></div>
    `;
    (api.getUpcomingPurchases as jest.Mock).mockResolvedValue({ purchases: ROWS });
  });

  test('no chips: every row renders and no scope note is shown', async () => {
    setScope('');
    await loadDashboard();
    expect(planNames()).toEqual(['AWS Plan', 'Azure Plan']);
    expect(list().querySelector('.upcoming-scope-note')).toBeNull();
  });

  test('provider chip hides rows from other providers', async () => {
    setScope('azure');
    await loadDashboard();
    expect(planNames()).toEqual(['Azure Plan']);
    // View Details renders from the scoped index, so the hidden row is gone.
    expect(list().querySelectorAll('[data-action="view-purchase"]')).toHaveLength(1);
  });

  test('provider chip with no matching rows shows a filter-aware empty state', async () => {
    setScope('gcp');
    await loadDashboard();
    expect(planNames()).toEqual([]);
    expect(list().querySelector('.empty')?.textContent).toBe('No upcoming scheduled purchases for GCP');
  });

  test('account chip cannot be applied, so the widget says so above the rows', async () => {
    setScope('', ['acct-1']);
    await loadDashboard();
    expect(planNames()).toEqual(['AWS Plan', 'Azure Plan']);
    const note = list().querySelector('.upcoming-scope-note');
    expect(note?.textContent).toBe(UPCOMING_ACCOUNT_SCOPE_NOTE);
    // The note sits before the first card.
    expect(list().firstElementChild).toBe(note);
  });

  test('account chip with nothing to show keeps the plain empty state (no note)', async () => {
    setScope('', ['acct-1']);
    (api.getUpcomingPurchases as jest.Mock).mockResolvedValue({ purchases: [] });
    await loadDashboard();
    expect(list().querySelector('.upcoming-scope-note')).toBeNull();
    expect(list().querySelector('.empty')?.textContent).toBe('No upcoming scheduled purchases');
  });

  test('Retry after a failed fetch applies the same provider scope', async () => {
    setScope('aws');
    (api.getUpcomingPurchases as jest.Mock).mockRejectedValueOnce(new Error('API error: 503'));
    await loadDashboard();
    expect(list().querySelector('.error')).not.toBeNull();
    const retry = list().querySelector('button[data-action="retry-upcoming"]') as HTMLButtonElement;
    retry.click();
    await new Promise(resolve => setTimeout(resolve, 0));
    expect(planNames()).toEqual(['AWS Plan']);
  });
});
