/**
 * Issue #743: the Opportunities summary renders the same .kpi-tile component
 * as Home (it used to write plain `.card` innerHTML, so the same kind of KPI
 * looked different on the two pages).
 */
import { loadRecommendations } from '../recommendations';
import type { LocalRecommendation } from '../types';

jest.mock('../api', () => ({
  getRecommendations: jest.fn(),
  refreshRecommendations: jest.fn(),
  listAccounts: jest.fn().mockResolvedValue([]),
  listAccountsMinimal: jest.fn().mockResolvedValue([]),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  listAccountServiceOverrides: jest.fn().mockResolvedValue([]),
}));

jest.mock('../api/recommendations', () => ({
  getRecommendationsFreshness: jest.fn().mockResolvedValue({
    last_collected_at: new Date().toISOString(),
    last_collection_error: null,
  }),
  refreshRecommendations: jest.fn().mockResolvedValue({}),
}));

jest.mock('../toast', () => ({
  showToast: jest.fn().mockReturnValue({ dismiss: jest.fn() }),
}));

const RECS: LocalRecommendation[] = [
  { id: 'a', provider: 'aws', cloud_account_id: 'acct', service: 'ec2', resource_type: 'm5.large', region: 'us-east-1', count: 1, term: 1, payment: 'all-upfront', savings: 100, upfront_cost: 1200 } as LocalRecommendation,
  { id: 'b', provider: 'aws', cloud_account_id: 'acct', service: 'rds', resource_type: 'db.t3', region: 'us-east-1', count: 1, term: 1, payment: 'all-upfront', savings: 200, upfront_cost: 2400 } as LocalRecommendation,
];

jest.mock('../state', () => ({
  getCurrentProvider: jest.fn().mockReturnValue('all'),
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
  // Administrators group id (see recommendations-enabled-providers.test.ts).
  getCurrentUser: jest.fn().mockReturnValue({ id: 'u', email: 'u@example.com', groups: ['00000000-0000-5000-8000-000000000001'] }),
}));

import * as api from '../api';

function seedDOM(): void {
  const tab = document.createElement('div');
  tab.id = 'opportunities-tab';
  tab.className = 'tab-content active';
  for (const id of ['recommendations-summary', 'recommendations-list']) {
    const el = document.createElement('div');
    el.id = id;
    tab.appendChild(el);
  }
  document.body.replaceChildren(tab);
}

beforeEach(() => {
  jest.clearAllMocks();
  seedDOM();
  (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: RECS, regions: [] });
});

test('summary renders four .kpi-tile cards in the Home tile markup', async () => {
  await loadRecommendations();
  const tiles = Array.from(document.querySelectorAll<HTMLElement>('#recommendations-summary > .card'));
  expect(tiles.map((t) => t.dataset['kpi'])).toEqual(['count', 'savings', 'upfront', 'payback']);
  for (const tile of tiles) {
    expect(tile.classList.contains('kpi-tile')).toBe(true);
    expect(tile.querySelector(':scope > h3')).not.toBeNull();
    expect(tile.querySelector(':scope > p.value.kpi-tile-value')).not.toBeNull();
  }
  expect(tiles.map((t) => t.querySelector('h3')!.textContent)).toEqual([
    'Total Recommendations', 'Potential Monthly Savings', 'Total Upfront Cost', 'Payback Period',
  ]);
});

test('only the savings tile uses the savings color, and values are filled in', async () => {
  await loadRecommendations();
  const value = (kpi: string) =>
    document.querySelector<HTMLElement>(`#recommendations-summary [data-kpi="${kpi}"] .kpi-tile-value`)!;
  expect(value('count').textContent).toBe('2');
  expect(value('savings').classList.contains('savings')).toBe(true);
  for (const kpi of ['count', 'upfront', 'payback']) {
    expect(value(kpi).classList.contains('savings')).toBe(false);
  }
  expect(value('savings').textContent).toContain('$');
  expect(value('upfront').textContent).toContain('$');
});
