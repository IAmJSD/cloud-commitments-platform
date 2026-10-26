/**
 * Issue #756: Opportunities first-run empty state vs filtered-to-zero hint.
 *
 * Zero loaded rows with no column filter render an onboarding card (and hide
 * the summary cards, filter bar and Purchase bar). Rows filtered away keep the
 * table hint and gain a "Clear filters" button.
 */
import { loadRecommendations, resetAutoRefreshInFlight } from '../recommendations';
import * as api from '../api';
import { refreshRecommendations as refreshRecsAPI } from '../api/recommendations';
import { ADMINISTRATORS_GROUP_ID } from '../permissions';
import { setOpenAccountsHandler } from '../first-run-empty-state';

jest.mock('../api', () => ({
  getRecommendations: jest.fn(),
  refreshRecommendations: jest.fn(),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  updateConfig: jest.fn().mockResolvedValue({}),
  listAccounts: jest.fn().mockResolvedValue([]),
  listAccountsMinimal: jest.fn().mockResolvedValue([]),
  listAccountServiceOverrides: jest.fn().mockResolvedValue([]),
}));

jest.mock('../api/recommendations', () => ({
  getRecommendationsFreshness: jest.fn().mockResolvedValue({
    last_collected_at: new Date().toISOString(),
    last_collection_error: null,
  }),
  refreshRecommendations: jest.fn().mockResolvedValue({}),
}));

jest.mock('../state', () => ({
  getCurrentProvider: jest.fn().mockReturnValue(''),
  setCurrentProvider: jest.fn(),
  getCurrentAccountIDs: jest.fn().mockReturnValue([]),
  setCurrentAccountIDs: jest.fn(),
  getRecommendations: jest.fn().mockReturnValue([]),
  getRecommendationByID: jest.fn().mockReturnValue(undefined),
  setRecommendations: jest.fn(),
  getSelectedRecommendationIDs: jest.fn().mockReturnValue(new Set()),
  clearSelectedRecommendations: jest.fn(),
  addSelectedRecommendation: jest.fn(),
  removeSelectedRecommendation: jest.fn(),
  getRecommendationsSort: jest.fn().mockReturnValue({ column: 'savings', direction: 'desc' }),
  setRecommendationsSort: jest.fn(),
  getRecommendationsColumnFilters: jest.fn().mockReturnValue({}),
  setRecommendationsColumnFilter: jest.fn(),
  clearAllRecommendationsColumnFilters: jest.fn(),
  getVisibleRecommendations: jest.fn().mockReturnValue([]),
  setVisibleRecommendations: jest.fn(),
  getCostPeriod: jest.fn().mockReturnValue('monthly'),
  setCostPeriod: jest.fn(),
  getHiddenColumns: jest.fn().mockReturnValue(new Set()),
  setHiddenColumns: jest.fn(),
  getCurrentUser: jest.fn(),
}));

jest.mock('../toast', () => ({
  showToast: jest.fn().mockReturnValue({ dismiss: jest.fn() }),
}));

import * as state from '../state';

const REC = {
  id: 'rec-1', provider: 'aws', cloud_account_id: 'acct-1', service: 'ec2', region: 'us-east-1',
  resource_type: 'm5.large', count: 1, term: 1, payment: 'all-upfront',
  savings: 100, upfront_cost: 500, monthly_cost: 0, on_demand_cost: 150,
};

const flush = (): Promise<void> => new Promise((r) => setTimeout(r, 0));
const list = (): HTMLElement => document.getElementById('recommendations-list')!;
const card = (): HTMLElement | null => list().querySelector('.first-run-empty-state');
const isHidden = (id: string): boolean => document.getElementById(id)?.classList.contains('hidden') ?? false;

const loadWith = async (recs: unknown[]): Promise<void> => {
  (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: recs, regions: [] });
  (state.getRecommendations as jest.Mock).mockReturnValue(recs);
  await loadRecommendations();
};

beforeEach(() => {
  jest.clearAllMocks();
  resetAutoRefreshInFlight();
  (state.getCurrentUser as jest.Mock).mockReturnValue({ id: 'u', email: 'u@example.com', groups: [ADMINISTRATORS_GROUP_ID] });
  (state.getCurrentProvider as jest.Mock).mockReturnValue('');
  (state.getRecommendationsColumnFilters as jest.Mock).mockReturnValue({});
  (api.listAccountsMinimal as jest.Mock).mockResolvedValue([]);
  setOpenAccountsHandler(jest.fn());
  const tab = document.createElement('div');
  tab.id = 'opportunities-tab';
  tab.className = 'tab-content active';
  const summary = document.createElement('section');
  summary.id = 'recommendations-summary';
  const toolbar = document.createElement('section');
  toolbar.id = 'recommendations-toolbar';
  const listEl = document.createElement('section');
  listEl.id = 'recommendations-list';
  tab.append(summary, toolbar, listEl);
  document.body.replaceChildren(tab);
});

describe('Opportunities first-run empty state (issue #756)', () => {
  test('no accounts: onboarding card with connect CTA, no empty table, chrome hidden', async () => {
    await loadWith([]);
    expect(card()).not.toBeNull();
    expect(card()!.querySelector('h3')!.textContent).toBe('No cloud accounts connected yet');
    expect([...card()!.querySelectorAll('button')].map((b) => b.textContent)).toEqual(['Connect a cloud account']);
    expect(list().querySelector('table')).toBeNull();
    expect(list().textContent).not.toMatch(/No rows match/);
    expect(isHidden('recommendations-summary')).toBe(true);
    // The Purchase bar is mounted but hidden; the filter bar is never built.
    expect(isHidden('recommendations-action-box')).toBe(true);
    expect(document.getElementById('recommendations-filter-status')).toBeNull();
  });

  test('accounts connected: "No opportunities yet" and Refresh triggers a collection', async () => {
    (api.listAccountsMinimal as jest.Mock).mockResolvedValue([{ id: 'acct-1', name: 'Prod' }]);
    await loadWith([]);
    expect(card()!.querySelector('h3')!.textContent).toBe('No opportunities yet');
    const refresh = [...card()!.querySelectorAll('button')].find((b) => b.textContent === 'Refresh recommendations');
    expect(refresh).toBeDefined();
    refresh!.click();
    expect(refreshRecsAPI).toHaveBeenCalledTimes(1);
    await flush();
  });

  test('account fetch failure does not claim that no accounts are connected', async () => {
    (api.listAccountsMinimal as jest.Mock).mockRejectedValue(new Error('boom'));
    await loadWith([]);
    expect(card()!.querySelector('h3')!.textContent).toBe('No opportunities yet');
  });

  test('topbar provider filter: scope copy instead of onboarding', async () => {
    (state.getCurrentProvider as jest.Mock).mockReturnValue('azure');
    await loadWith([]);
    expect(card()!.querySelector('h3')!.textContent).toBe('No opportunities for the selected provider or account');
  });

  test('chrome comes back once recommendations load', async () => {
    await loadWith([]);
    expect(isHidden('recommendations-summary')).toBe(true);
    await loadWith([REC]);
    expect(card()).toBeNull();
    expect(list().querySelector('table')).not.toBeNull();
    expect(isHidden('recommendations-summary')).toBe(false);
    expect(isHidden('recommendations-filter-status')).toBe(false);
    expect(isHidden('recommendations-action-box')).toBe(false);
  });
});

describe('Filtered-to-zero hint (issue #756)', () => {
  test('client-side filter: hint keeps the table and Clear filters rerenders without refetch', async () => {
    (state.getRecommendationsColumnFilters as jest.Mock).mockReturnValue({ provider: { kind: 'set', values: ['gcp'] } });
    await loadWith([REC]);
    expect(card()).toBeNull();
    const cell = list().querySelector('tbody td.empty')!;
    expect(cell.textContent).toMatch(/No rows match these filters\./);
    const btn = cell.querySelector<HTMLButtonElement>('button.empty-clear-filters')!;
    expect(btn.textContent).toBe('Clear filters');
    (api.getRecommendations as jest.Mock).mockClear();
    btn.click();
    expect(state.clearAllRecommendationsColumnFilters).toHaveBeenCalledTimes(1);
    expect(api.getRecommendations).not.toHaveBeenCalled();
  });

  test('server-pushed-down filter with zero loaded: hint (not onboarding) and Clear filters refetches', async () => {
    (state.getRecommendationsColumnFilters as jest.Mock).mockReturnValue({ service: { kind: 'set', values: ['rds'] } });
    await loadWith([]);
    expect(card()).toBeNull();
    const btn = list().querySelector<HTMLButtonElement>('tbody td.empty button.empty-clear-filters')!;
    (api.getRecommendations as jest.Mock).mockClear();
    btn.click();
    expect(state.clearAllRecommendationsColumnFilters).toHaveBeenCalledTimes(1);
    expect(api.getRecommendations).toHaveBeenCalledTimes(1);
    await flush();
  });
});
