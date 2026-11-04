/**
 * Issue #1692: the RI Exchange table and the Exchange dialog name the cloud
 * account the RIs belong to, so an operator can confirm the target before
 * quoting or executing an exchange.
 */

jest.mock('../api', () => ({
  listConvertibleRIs: jest.fn(),
  listAccountsMinimal: jest.fn(),
  listExchangeableAzureRIs: jest.fn(),
  getRIUtilization: jest.fn(),
  getReshapeRecommendations: jest.fn(),
  getRIExchangeHistory: jest.fn(),
  listTargetOfferings: jest.fn(),
}));

jest.mock('../navigation', () => ({
  switchTab: jest.fn(),
  switchSettingsSubTab: jest.fn(),
}));

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

jest.mock('../permissions', () => ({
  canAccess: jest.fn(() => true),
}));

import { loadRIExchange, openExchangeModal } from '../riexchange';
import * as api from '../api';
import { flushPromises } from './flush-promises';

const ri = (id: string, accountID?: string) => ({
  reserved_instance_id: id,
  instance_type: 't4g.nano',
  availability_zone: '',
  instance_count: 1,
  start: '2026-05-22T00:00:00Z',
  end: '2027-05-22T00:00:00Z',
  offering_type: 'No Upfront',
  fixed_price: 0,
  usage_price: 0,
  state: 'active',
  normalization_factor: 0.25,
  ...(accountID ? { account_id: accountID } : {}),
});

const hostAccount = { id: 'uuid-host', name: 'CUDly host (123456789012)', external_id: '123456789012', provider: 'aws' };
const plainAccount = { id: 'uuid-plain', name: 'Payer', external_id: '210987654321', provider: 'aws' };

describe('RI Exchange account label (issue #1692)', () => {
  let instancesEl: HTMLDivElement;

  beforeEach(() => {
    document.body.replaceChildren();
    instancesEl = document.createElement('div');
    instancesEl.id = 'ri-exchange-instances-list';
    const recsEl = document.createElement('div');
    recsEl.id = 'ri-exchange-recommendations-list';
    const historyEl = document.createElement('div');
    historyEl.id = 'ri-exchange-history-list';
    const modal = document.createElement('div');
    modal.id = 'ri-exchange-modal';
    const content = document.createElement('div');
    content.className = 'modal-content';
    modal.appendChild(content);
    document.body.append(instancesEl, recsEl, historyEl, modal);

    (api.getRIUtilization as jest.Mock).mockResolvedValue([]);
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([]);
    (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({ recommendations: [], recs_staleness: '', recs_collected_at: null });
    (api.listTargetOfferings as jest.Mock).mockResolvedValue([]);
    (api.listAccountsMinimal as jest.Mock).mockResolvedValue([hostAccount, plainAccount]);
  });

  const accountLine = (): string =>
    instancesEl.querySelector('#ri-exchange-account-label')?.textContent?.trim() ?? '';

  it('names the account once, without repeating an ID already in its name', async () => {
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([ri('ri-1', '123456789012'), ri('ri-2', '123456789012')]);

    await loadRIExchange();
    await flushPromises();

    expect(accountLine()).toBe('Account: CUDly host (123456789012)');
  });

  it('adds the account ID when the name does not contain it', async () => {
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([ri('ri-1', '210987654321')]);

    await loadRIExchange();
    await flushPromises();

    expect(accountLine()).toBe('Account: Payer (210987654321)');
  });

  it('falls back to the account ID when the account is not registered', async () => {
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([ri('ri-1', '555555555555')]);

    await loadRIExchange();
    await flushPromises();

    expect(accountLine()).toBe('Account: AWS account 555555555555');
  });

  it('keeps the ID label when the account names cannot be loaded', async () => {
    (api.listAccountsMinimal as jest.Mock).mockRejectedValue(new Error('forbidden'));
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([ri('ri-1', '123456789012')]);

    await loadRIExchange();
    await flushPromises();

    expect(accountLine()).toBe('Account: AWS account 123456789012');
  });

  it('says when the account could not be determined', async () => {
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([ri('ri-1')]);

    await loadRIExchange();
    await flushPromises();

    expect(accountLine()).toBe('Account: could not be determined');
  });

  it('shows the account in the Exchange dialog', async () => {
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([ri('ri-1', '123456789012')]);
    await loadRIExchange();
    await flushPromises();

    openExchangeModal('ri-1', 1);

    const dialogAccount = document.querySelector('#ri-exchange-modal #modal-exchange-account')?.textContent?.trim();
    expect(dialogAccount).toBe('Account: CUDly host (123456789012)');
  });
});
