/**
 * Issue #756: Home shows a first-run onboarding card when nothing has been
 * loaded and nothing is committed yet.
 */
jest.mock('../toast', () => ({ showToast: jest.fn(() => ({ dismiss: jest.fn() })) }));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn(() => Promise.resolve(true)) }));
jest.mock('chart.js', () => {
  const MockChart = jest.fn().mockImplementation(() => ({ destroy: jest.fn() }));
  (MockChart as unknown as { register: jest.Mock }).register = jest.fn();
  return { Chart: MockChart, registerables: [] };
});

const mockStartRefresh = jest.fn((_onReload: unknown) => Promise.resolve());
jest.mock('../recommendations', () => ({
  groupRecsByCell: (recs: unknown[]) => new Map(recs.length ? [['cell-1', recs]] : []),
  pageLevelRange: (groups: Map<string, unknown[]>) =>
    (groups.size === 0 ? { savingsMin: 0, savingsMax: 0, cellCount: 0 } : { savingsMin: 1, savingsMax: 2, cellCount: 1 }),
  formatSavingsRange: () => '$1 – $2',
  triggerAutoRefreshIfStale: jest.fn(() => Promise.resolve()),
  startRecommendationsRefresh: (onReload: unknown) => mockStartRefresh(onReload),
}));

jest.mock('../api/recommendations', () => ({
  getRecommendationsFreshness: jest.fn().mockResolvedValue({ last_collected_at: null, last_collection_error: null }),
}));

jest.mock('../api', () => ({
  getDashboardSummary: jest.fn(),
  getUpcomingPurchases: jest.fn().mockResolvedValue({ purchases: [] }),
  getRecommendations: jest.fn(),
  listAccountsMinimal: jest.fn(),
  getSavingsAnalytics: jest.fn().mockResolvedValue({ data_points: [] }),
}));

jest.mock('../state', () => ({
  getCurrentProvider: jest.fn().mockReturnValue(''),
  getCurrentAccountIDs: jest.fn().mockReturnValue([]),
  getSavingsChart: jest.fn().mockReturnValue(null),
  setSavingsChart: jest.fn(),
  subscribeProvider: jest.fn(),
  subscribeAccount: jest.fn(),
  getCurrentUser: jest.fn().mockReturnValue({
    id: 'admin-user', email: 'admin@example.com',
    groups: ['00000000-0000-5000-8000-000000000001'],
    effectivePermissions: [{ action: 'admin', resource: '*' }],
  }),
}));

import { loadDashboard } from '../dashboard';
import * as api from '../api';
import { setOpenAccountsHandler } from '../first-run-empty-state';

const host = (): HTMLElement => document.getElementById('home-onboarding')!;
const EMPTY_SUMMARY = { total_recommendations: 0, active_commitments: 0, committed_monthly: 0, ytd_savings: 0, by_service: {} };

beforeEach(() => {
  jest.clearAllMocks();
  document.body.innerHTML = `
    <section id="home-onboarding" class="hidden"></section>
    <div id="summary"></div>
    <div id="upcoming-list"></div>
  `;
  (api.getDashboardSummary as jest.Mock).mockResolvedValue(EMPTY_SUMMARY);
  (api.getRecommendations as jest.Mock).mockResolvedValue({ recommendations: [] });
  (api.listAccountsMinimal as jest.Mock).mockResolvedValue([]);
  setOpenAccountsHandler(jest.fn());
});

describe('Home onboarding card (issue #756)', () => {
  test('first run with no accounts: visible card with connect CTA', async () => {
    await loadDashboard();
    expect(host().classList.contains('hidden')).toBe(false);
    expect(host().querySelector('h3')!.textContent).toBe('No cloud accounts connected yet');
    expect(host().querySelector('button')!.textContent).toBe('Connect a cloud account');
    // The KPI tiles still render underneath.
    expect(document.querySelectorAll('#summary .kpi-tile').length).toBe(4);
  });

  test('accounts connected but no recs: refresh reloads the dashboard', async () => {
    (api.listAccountsMinimal as jest.Mock).mockResolvedValue([{ id: 'a', name: 'A' }]);
    await loadDashboard();
    expect(host().querySelector('h3')!.textContent).toBe('No opportunities yet');
    host().querySelector<HTMLButtonElement>('button')!.click();
    expect(mockStartRefresh).toHaveBeenCalledWith(loadDashboard);
  });

  test('hidden once recommendations exist', async () => {
    (api.getRecommendations as jest.Mock).mockResolvedValue({ recommendations: [{ id: 'r1' }] });
    await loadDashboard();
    expect(host().classList.contains('hidden')).toBe(true);
    expect(host().childElementCount).toBe(0);
  });

  test('hidden when commitments exist even without recommendations', async () => {
    (api.getDashboardSummary as jest.Mock).mockResolvedValue({ ...EMPTY_SUMMARY, active_commitments: 3 });
    await loadDashboard();
    expect(host().classList.contains('hidden')).toBe(true);
  });

  test('hidden when the recommendations fetch failed (unknown, not empty)', async () => {
    (api.getRecommendations as jest.Mock).mockRejectedValue(new Error('down'));
    await loadDashboard();
    expect(host().classList.contains('hidden')).toBe(true);
  });

  test('account list failure degrades to the generic copy', async () => {
    (api.listAccountsMinimal as jest.Mock).mockRejectedValue(new Error('403'));
    await loadDashboard();
    expect(host().querySelector('h3')!.textContent).toBe('No opportunities yet');
  });
});
