/**
 * Reshape rows with no exchange target (issue #1410).
 *
 * The analyzer returns target_instance_type "" and target_count 0 when it
 * has no target for an RI (a completely unused RI, or a target capped at the
 * units the account owns). Those rows rendered "0x " in Suggested and kept a
 * live Exchange button that opened the modal with a 0 count.
 */

jest.mock('../api', () => ({
  listConvertibleRIs: jest.fn(),
  getRIUtilization: jest.fn().mockResolvedValue([]),
  getReshapeRecommendations: jest.fn(),
  getRIExchangeHistory: jest.fn().mockResolvedValue([]),
  listTargetOfferings: jest.fn().mockResolvedValue([]),
}));

jest.mock('../navigation', () => ({
  switchTab: jest.fn(),
  switchSettingsSubTab: jest.fn(),
}));

jest.mock('../state', () => ({
  getCurrentUser: jest.fn(() => ({ id: 'u', email: 'u@example.com', groups: ['00000000-0000-5000-8000-000000000001'] })),
  getCurrentProvider: jest.fn(() => 'aws'),
  getCurrentAccountIDs: jest.fn(() => []),
  getRiExchangeColumnFilters: jest.fn(() => ({})),
  setRiExchangeColumnFilter: jest.fn(),
  clearAllRiExchangeColumnFilters: jest.fn(),
  getActiveRiColumnFilters: jest.fn(() => ({})),
  setActiveRiColumnFilter: jest.fn(),
  clearAllActiveRiColumnFilters: jest.fn(),
}));

import * as api from '../api';
import { loadReshapeRecommendations } from '../riexchange';

const withTarget = {
  source_ri_id: 'ri-target',
  source_instance_type: 'm5.xlarge',
  source_count: 1,
  target_instance_type: 'm5.large',
  target_count: 2,
  utilization_percent: 50,
  normalized_used: 4,
  normalized_purchased: 8,
  reason: 'Suggest exchanging 1x m5.xlarge for 2x m5.large.',
};

const noTarget = {
  source_ri_id: 'ri-unused',
  source_instance_type: 'm5.xlarge',
  source_count: 1,
  target_instance_type: '',
  target_count: 0,
  utilization_percent: 0,
  normalized_used: 0,
  normalized_purchased: 8,
  reason: 'RI is completely unused (0% utilization, 8.0 normalized units wasted).',
};

const alternative = { instance_type: 'm7g.large', offering_id: 'off-m7g', effective_monthly_cost: 30 };

function setupDom(): HTMLDivElement {
  const recs = document.createElement('div');
  recs.id = 'ri-exchange-recommendations-list';
  const modal = document.createElement('div');
  modal.id = 'ri-exchange-modal';
  modal.className = 'modal hidden';
  const content = document.createElement('div');
  content.className = 'modal-content';
  modal.appendChild(content);
  document.body.replaceChildren(recs, modal);
  return recs;
}

async function renderRows(recommendations: unknown[]): Promise<HTMLTableRowElement[]> {
  const recs = setupDom();
  (api.getReshapeRecommendations as jest.Mock).mockResolvedValueOnce({
    recommendations,
    recs_staleness: '',
    recs_collected_at: null,
  });
  await loadReshapeRecommendations();
  return Array.from(recs.querySelectorAll<HTMLTableRowElement>('tbody tr'));
}

// Columns: Source RI, Current, Suggested, Alternatives, Utilization,
// Normalized Units, Reason, Actions.
const SUGGESTED = 2;
const REASON = 6;
const ACTIONS = 7;

function cell(row: HTMLTableRowElement, i: number): HTMLTableCellElement {
  return row.querySelectorAll<HTMLTableCellElement>('td')[i]!;
}

describe('reshape rows with no exchange target (#1410)', () => {
  afterEach(() => {
    document.body.replaceChildren();
    (api.getReshapeRecommendations as jest.Mock).mockReset();
  });

  it('says there is no target instead of "0x " and shows the reason', async () => {
    const [row] = await renderRows([noTarget]);

    expect(cell(row!, SUGGESTED).textContent!.trim()).toBe('No exchange target');
    expect(cell(row!, SUGGESTED).textContent).not.toContain('0x');
    expect(cell(row!, REASON).textContent).toBe(noTarget.reason);
  });

  it('disables the Exchange button and attaches no quote action', async () => {
    const [row] = await renderRows([noTarget]);

    const btn = cell(row!, ACTIONS).querySelector<HTMLButtonElement>('button')!;
    expect(btn.disabled).toBe(true);
    expect(btn.dataset['action']).toBeUndefined();
    expect(btn.title).toBe('No exchange target available for this RI');

    btn.click();
    expect(document.getElementById('ri-exchange-modal')!.classList.contains('hidden')).toBe(true);
  });

  it('keeps the Exchange button live for a row with a target', async () => {
    const [row] = await renderRows([withTarget]);

    expect(cell(row!, SUGGESTED).textContent!.trim()).toBe('2x m5.large');
    const btn = cell(row!, ACTIONS).querySelector<HTMLButtonElement>('button')!;
    expect(btn.disabled).toBe(false);
    expect(btn.dataset['action']).toBe('fill-quote');
  });

  it('keeps Exchange live when alternatives exist, opening the modal with a count of 1', async () => {
    const [row] = await renderRows([{ ...noTarget, alternative_targets: [alternative] }]);

    expect(cell(row!, SUGGESTED).textContent!.trim()).toBe('No exchange target');
    const btn = cell(row!, ACTIONS).querySelector<HTMLButtonElement>('button')!;
    expect(btn.disabled).toBe(false);
    btn.click();

    const modal = document.getElementById('ri-exchange-modal')!;
    expect(modal.classList.contains('hidden')).toBe(false);
    const count = modal.querySelector<HTMLInputElement>('.modal-exchange-count')!;
    expect(count.value).toBe('1');
  });
});
