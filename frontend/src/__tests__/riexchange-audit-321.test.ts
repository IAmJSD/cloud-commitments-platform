/**
 * Issue #321 (2026-09-02 audit, A12-067 and A12-069):
 *   - target-picker option labels are set via textContent, so they must not
 *     be pre-escaped (that rendered "&amp;" literally);
 *   - a failed exchange's error reason is shown in the history Status cell.
 */

jest.mock('../api', () => ({
  listConvertibleRIs: jest.fn().mockResolvedValue([]),
  getRIUtilization: jest.fn().mockResolvedValue([]),
  getReshapeRecommendations: jest.fn().mockResolvedValue({ recommendations: [], recs_staleness: '', recs_collected_at: null }),
  getRIExchangeHistory: jest.fn(),
  listTargetOfferings: jest.fn(),
}));

jest.mock('../navigation', () => ({
  switchTab: jest.fn(),
  switchSettingsSubTab: jest.fn(),
}));

jest.mock('../state', () => ({
  getCurrentUser: jest.fn(() => null),
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
import { loadRIExchange, openExchangeModal } from '../riexchange';

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

function historyRecord(overrides: Record<string, unknown>) {
  return {
    id: 'h1', account_id: 'a', exchange_id: '', region: 'us-east-1', source_ri_ids: ['ri-1'],
    source_instance_type: 'm5.large', source_count: 2, target_offering_id: 'o', target_instance_type: 'm5.xlarge',
    target_count: 1, payment_due: '0.00', status: 'failed', mode: 'manual',
    created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

describe('RI exchange target picker labels (A12-067)', () => {
  let modal: HTMLDivElement;

  beforeEach(() => {
    modal = document.createElement('div');
    modal.id = 'ri-exchange-modal';
    modal.className = 'modal hidden';
    const content = document.createElement('div');
    content.className = 'modal-content';
    modal.appendChild(content);
    document.body.replaceChildren(modal);
  });

  function optionTexts(): string[] {
    const picker = modal.querySelector<HTMLSelectElement>('.modal-exchange-target-select');
    return picker ? Array.from(picker.options).map((o) => o.textContent ?? '') : [];
  }

  test('AWS offering labels keep &, < and quotes as typed', async () => {
    (api.listTargetOfferings as jest.Mock).mockResolvedValueOnce([
      { offering_id: 'off-1', instance_type: 'm5.large', offering_type: 'Partial & "Light" <Upfront>' },
    ]);
    openExchangeModal('ri-abc', 2);
    await flush();

    expect(optionTexts()).toContain('m5.large -- Partial & "Light" <Upfront>');
    expect(optionTexts().join('\n')).not.toContain('&amp;');
  });

  test('CE alternative labels are not pre-escaped', async () => {
    (api.listTargetOfferings as jest.Mock).mockResolvedValueOnce([]);
    openExchangeModal('ri-abc', 2, undefined, [
      { instance_type: 'x2&<gd>.large', offering_id: 'off-alt', effective_monthly_cost: 12 },
    ]);
    await flush();

    const label = optionTexts().find((t) => t.startsWith('x2'));
    expect(label).toMatch(/^x2&<gd>\.large -- /);
    expect(label).not.toContain('&amp;');
  });
});

describe('RI exchange history failure reason (A12-069)', () => {
  beforeEach(() => {
    const ids = ['ri-exchange-instances-list', 'ri-exchange-recommendations-list', 'ri-exchange-history-list'];
    document.body.replaceChildren(...ids.map((id) => Object.assign(document.createElement('div'), { id })));
  });

  function statusCell(): HTMLTableCellElement {
    const row = document.querySelector('#ri-exchange-history-list tbody tr')!;
    return row.querySelectorAll<HTMLTableCellElement>('td')[4]!;
  }

  test('shows the error reason, as text, under a failed badge', async () => {
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([
      historyRecord({ error: 'AcceptReservedInstancesExchangeQuote: <b>InvalidParameter</b>' }),
    ]);
    await loadRIExchange();

    const cell = statusCell();
    expect(cell.querySelector('.status-badge')?.textContent).toBe('failed');
    const detail = cell.querySelector('.status-detail');
    expect(detail?.textContent).toBe('AcceptReservedInstancesExchangeQuote: <b>InvalidParameter</b>');
    expect(detail?.querySelector('b')).toBeNull();
  });

  test('renders no detail line when the record has no error', async () => {
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([historyRecord({ status: 'completed' })]);
    await loadRIExchange();

    expect(statusCell().querySelector('.status-detail')).toBeNull();
  });
});
