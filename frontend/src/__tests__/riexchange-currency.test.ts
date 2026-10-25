/**
 * RI exchange money display honors currency_code (issue #284): cost chips,
 * picker labels and the running total use the offering's own currency, and
 * an empty history payment renders a dash instead of a lone "$".
 */
jest.mock('../api', () => ({
  listConvertibleRIs: jest.fn(),
  listAccountsMinimal: jest.fn(),
  listExchangeableAzureRIs: jest.fn(),
  getRIUtilization: jest.fn(),
  getReshapeRecommendations: jest.fn(),
  getExchangeQuote: jest.fn(),
  executeExchange: jest.fn(),
  getRIExchangeHistory: jest.fn(),
  getRIExchangeConfig: jest.fn(),
  updateRIExchangeConfig: jest.fn(),
  listTargetOfferings: jest.fn(),
}));
jest.mock('../navigation', () => ({ switchTab: jest.fn(), switchSettingsSubTab: jest.fn() }));
jest.mock('../state', () => ({
  subscribeProvider: jest.fn(() => () => undefined),
  subscribeAccount: jest.fn(() => () => undefined),
  getCurrentProvider: jest.fn(() => 'aws'),
  getCurrentAccountIDs: jest.fn(() => []),
  getCurrentUser: jest.fn(() => ({ id: 'u', email: 'u@example.com', groups: ['00000000-0000-5000-8000-000000000001'] })),
  getRiExchangeColumnFilters: jest.fn(() => ({})),
  setRiExchangeColumnFilter: jest.fn(),
  clearAllRiExchangeColumnFilters: jest.fn(),
  getActiveRiColumnFilters: jest.fn(() => ({})),
  setActiveRiColumnFilter: jest.fn(),
  clearAllActiveRiColumnFilters: jest.fn(),
}));

import { currencyPrefix, loadRIExchange, loadReshapeRecommendations, openExchangeModal } from '../riexchange';
import * as api from '../api';

const eur = { instance_type: 'm6i.large', offering_id: 'off-m6i', effective_monthly_cost: 52, currency_code: 'EUR' };
const usd = { instance_type: 'm5.large', offering_id: 'off-m5', effective_monthly_cost: 40, currency_code: 'USD' };

beforeEach(() => {
  (api.listTargetOfferings as jest.Mock).mockResolvedValue([]);
});

afterEach(() => {
  document.body.innerHTML = '';
});

describe('currencyPrefix', () => {
  it('keeps "$" for USD and for an absent code, and prefixes other ISO codes', () => {
    expect(currencyPrefix('USD')).toBe('$');
    expect(currencyPrefix(undefined)).toBe('$');
    expect(currencyPrefix('')).toBe('$');
    expect(currencyPrefix('eur')).toBe('EUR ');
  });
});

describe('exchange modal', () => {
  function openModal(alternatives: typeof eur[], suggested = eur.instance_type): HTMLElement {
    const modal = document.createElement('div');
    modal.id = 'ri-exchange-modal';
    modal.className = 'modal hidden';
    const content = document.createElement('div');
    content.className = 'modal-content';
    modal.appendChild(content);
    document.body.appendChild(modal);
    openExchangeModal('ri-abc', 2, suggested, alternatives);
    return modal;
  }

  it('prices the chip, picker label and running total in the offering currency', () => {
    const modal = openModal([eur]);
    expect(modal.querySelector('.cost-chip')?.textContent).toBe('EUR 52.00/mo each');
    const labels = Array.from(modal.querySelectorAll('option')).map((o) => o.textContent);
    expect(labels).toContain('m6i.large -- EUR 52.00/mo');
    const total = modal.querySelector('#modal-exchange-running-total')?.textContent ?? '';
    expect(total).toContain('EUR 104.00/mo');
    expect(total).not.toContain('$');
  });

  it('refuses to add up targets priced in different currencies', () => {
    const modal = openModal([eur, usd]);
    modal.querySelector<HTMLButtonElement>('#modal-exchange-add-target')?.click();
    const second = modal.querySelectorAll<HTMLSelectElement>('.modal-exchange-target-select')[1];
    if (second) {
      second.value = usd.offering_id;
      second.dispatchEvent(new Event('change'));
    }
    const total = modal.querySelector('#modal-exchange-running-total')?.textContent ?? '';
    expect(total).toContain('not totaled');
    expect(total).not.toMatch(/\d/);
  });
});

describe('RI exchange tables', () => {
  beforeEach(() => {
    for (const id of ['ri-exchange-instances-list', 'ri-exchange-recommendations-list', 'ri-exchange-history-list']) {
      const el = document.createElement('div');
      el.id = id;
      document.body.appendChild(el);
    }
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([]);
    (api.getRIUtilization as jest.Mock).mockResolvedValue([]);
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([]);
  });

  it('renders alternative cost chips in each offering currency', async () => {
    (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({
      recommendations: [{
        source_ri_id: 'ri-abc', source_instance_type: 'm5.xlarge', source_count: 1, target_instance_type: 'm6i.large',
        target_count: 2, utilization_percent: 50, normalized_used: 4, normalized_purchased: 8, reason: 'underutilized',
        alternative_targets: [eur, { ...usd, currency_code: undefined }],
      }],
      recs_staleness: '', recs_collected_at: null,
    });
    await loadReshapeRecommendations();
    const chips = Array.from(document.querySelectorAll('#ri-exchange-recommendations-list .cost-chip')).map((c) => c.textContent);
    expect(chips).toEqual(['m6i.large EUR 52.00/mo', 'm5.large $40.00/mo']);
  });

  it('shows a dash, not a lone "$", for a history row with no payment', async () => {
    (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({ recommendations: [], recs_staleness: '', recs_collected_at: null });
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue(['', '1.25'].map((payment_due, i) => ({
      id: `h${i}`, account_id: 'a', exchange_id: '', region: 'us-east-1', source_ri_ids: ['ri-1'],
      source_instance_type: 'm5.large', source_count: 1, target_offering_id: 'o', target_instance_type: 'm5.xlarge',
      target_count: 1, payment_due, status: 'failed', mode: 'manual',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    })));
    await loadRIExchange();
    const paymentCol = Array.from(document.querySelectorAll('#ri-exchange-history-list th')).findIndex((h) => h.textContent === 'Payment');
    const cells = Array.from(document.querySelectorAll('#ri-exchange-history-list tbody tr'))
      .map((tr) => tr.querySelectorAll('td')[paymentCol]?.textContent);
    expect(cells).toEqual(['—', '$1.25']);
  });
});
