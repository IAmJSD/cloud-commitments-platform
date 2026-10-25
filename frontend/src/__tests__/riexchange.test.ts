/**
 * RI Exchange module tests — openExchangeModal and fillQuoteFromRI
 */

// Mock the api module defensively (riexchange.ts imports it)
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
  // listTargetOfferings is called by populateAwsOfferings() in openExchangeModal.
  // Default to returning an empty list so tests that don't care about the picker
  // content remain unaffected. Override per-test for picker-content assertions.
  listTargetOfferings: jest.fn().mockResolvedValue([]),
}));

// Mock navigation to avoid loading dashboard/plans/... transitively.
jest.mock('../navigation', () => ({
  switchTab: jest.fn(),
  switchSettingsSubTab: jest.fn(),
}));

// Capture subscription callbacks so tests can fire them directly.
// These arrays are populated by the mock factory below. Declared with
// `let` so the reference is stable across the hoisted jest.mock call.
let _providerListeners: Array<() => void> = [];
let _accountListeners: Array<() => void> = [];
jest.mock('../state', () => ({
  subscribeProvider: jest.fn((cb: () => void) => {
    // _providerListeners may not be initialised yet at hoist time —
    // access it lazily via the closure over the outer `let`.
    _providerListeners = _providerListeners ?? [];
    _providerListeners.push(cb);
    return () => undefined;
  }),
  subscribeAccount: jest.fn((cb: () => void) => {
    _accountListeners = _accountListeners ?? [];
    _accountListeners.push(cb);
    return () => undefined;
  }),
  getCurrentProvider: jest.fn(() => 'aws'),
  getCurrentAccountIDs: jest.fn(() => []),
  getCurrentUser: jest.fn(() => ({ id: 'u', email: 'u@example.com', groups: ['00000000-0000-5000-8000-000000000001'] })),
  // RI Exchange column-filter slice (issue #166 follow-up). Tests don't
  // exercise the filter state directly; an empty record + no-op setters
  // keep renderRecommendations on its happy path.
  getRiExchangeColumnFilters: jest.fn(() => ({})),
  setRiExchangeColumnFilter: jest.fn(),
  clearAllRiExchangeColumnFilters: jest.fn(),
  // Active Convertible RIs column-filter slice (issue #1414).
  getActiveRiColumnFilters: jest.fn(() => ({})),
  setActiveRiColumnFilter: jest.fn(),
  clearAllActiveRiColumnFilters: jest.fn(),
}));

import {
  fillQuoteFromRI,
  loadReshapeRecommendations,
  loadRIExchange,
  openExchangeModal,
  renderExcludedRIsNotice,
  renderReshapeStalenessBanner,
  setupRIExchangeHandlers,
} from '../riexchange';
import * as api from '../api';
import * as navigation from '../navigation';

function createModal(): HTMLDivElement {
  const modal = document.createElement('div');
  modal.id = 'ri-exchange-modal';
  modal.className = 'modal hidden';
  const content = document.createElement('div');
  content.className = 'modal-content';
  modal.appendChild(content);
  document.body.appendChild(modal);
  return modal;
}

describe('openExchangeModal', () => {
  let modal: HTMLDivElement;

  beforeEach(() => {
    modal = createModal();
    // Other describes call jest.resetAllMocks(), which wipes the default
    // listTargetOfferings resolution; restore it so test order does not matter.
    (api.listTargetOfferings as jest.Mock).mockResolvedValue([]);
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('removes hidden class to show the modal', () => {
    openExchangeModal('ri-abc123', 2);
    expect(modal.classList.contains('hidden')).toBe(false);
  });

  it('displays the RI ID in the modal content', () => {
    openExchangeModal('ri-abc123', 2);
    expect(modal.textContent).toContain('ri-abc123');
  });

  it('pre-fills count input with given count', () => {
    openExchangeModal('ri-abc123', 5);
    // Multi-target refactor: count + target inputs now live inside
    // per-row containers; selectors changed from #id to class.
    const countInput = modal.querySelector<HTMLInputElement>('.modal-exchange-count');
    expect(countInput?.value).toBe('5');
  });

  it('pre-fills hidden target input with offering_id when suggestedTargetType matches an alternativeTarget', () => {
    // suggestedTargetType is resolved to an offering_id via alternativeTargets lookup.
    openExchangeModal('ri-abc123', 2, 'm5.large', [
      { instance_type: 'm5.large', offering_id: '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91', effective_monthly_cost: 42.5 },
    ]);
    const targetInput = modal.querySelector<HTMLInputElement>('.modal-exchange-target');
    // Hidden input must contain the UUID, not the instance type string.
    expect(targetInput?.value).toBe('4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91');
  });

  it('leaves hidden target input empty when suggestedTargetType is not provided', () => {
    openExchangeModal('ri-abc123', 2);
    const targetInput = modal.querySelector<HTMLInputElement>('.modal-exchange-target');
    expect(targetInput?.value).toBe('');
  });

  it('leaves hidden target input empty when suggestedTargetType has no matching alternativeTarget', () => {
    openExchangeModal('ri-abc123', 2, 'm5.large');
    const targetInput = modal.querySelector<HTMLInputElement>('.modal-exchange-target');
    // No alternativeTargets provided -- cannot resolve instance type to UUID.
    expect(targetInput?.value).toBe('');
  });

  it('starts with exactly one target row', () => {
    openExchangeModal('ri-abc123', 2);
    const rows = modal.querySelectorAll<HTMLDivElement>('.exchange-target-row');
    expect(rows.length).toBe(1);
  });

  it('adds a second target row when "+ Add target" is clicked', () => {
    openExchangeModal('ri-abc123', 2);
    const addBtn = modal.querySelector<HTMLButtonElement>('#modal-exchange-add-target');
    expect(addBtn).not.toBeNull();
    addBtn?.click();
    const rows = modal.querySelectorAll<HTMLDivElement>('.exchange-target-row');
    expect(rows.length).toBe(2);
  });

  it('posts singleton target_offering_id/target_count when exactly one row is present', async () => {
    const mockGetQuote = api.getExchangeQuote as jest.Mock;
    mockGetQuote.mockResolvedValueOnce({
      IsValidExchange: false,
      ValidationFailureReason: 'test',
      CurrencyCode: 'USD',
      PaymentDueRaw: '0',
      SourceHourlyPriceRaw: '',
      SourceRemainingUpfrontRaw: '',
      SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '',
      TargetRemainingUpfrontRaw: '',
      TargetRemainingTotalRaw: '',
    });
    // Pre-seed with a CE alternative so suggestedTargetType resolves to a UUID.
    const offeringUUID = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
    openExchangeModal('ri-abc', 3, 'm5.large', [
      { instance_type: 'm5.large', offering_id: offeringUUID, effective_monthly_cost: 42.5 },
    ]);
    const quoteBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote');
    quoteBtn?.click();
    // Wait for the async submit handler to settle.
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(mockGetQuote).toHaveBeenCalledTimes(1);
    const req = mockGetQuote.mock.calls[0][0];
    expect(req.ri_ids).toEqual(['ri-abc']);
    // Singleton shape: target_offering_id must be the UUID, not the instance type.
    expect(req.target_offering_id).toBe(offeringUUID);
    expect(req.target_count).toBe(3);
    expect(req.targets).toBeUndefined();
  });

  it('carries the quote-resolved region onto the execute request (issue #238)', async () => {
    const mockGetQuote = api.getExchangeQuote as jest.Mock;
    const mockExecute = api.executeExchange as jest.Mock;
    mockGetQuote.mockResolvedValueOnce({
      IsValidExchange: true,
      ValidationFailureReason: '',
      CurrencyCode: 'USD',
      PaymentDueRaw: '12.50',
      SourceHourlyPriceRaw: '',
      SourceRemainingUpfrontRaw: '',
      SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '',
      TargetRemainingUpfrontRaw: '',
      TargetRemainingTotalRaw: '',
      // Region the backend's SDK chain resolved for this quote.
      Region: 'us-west-2',
    });
    mockExecute.mockResolvedValueOnce({ exchange_id: 'exch-1', quote: {} });

    const offeringUUID = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
    openExchangeModal('ri-abc', 3, 'm5.large', [
      { instance_type: 'm5.large', offering_id: offeringUUID, effective_monthly_cost: 42.5 },
    ]);
    const quoteBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote');
    quoteBtn?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const executeBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Execute Exchange');
    expect(executeBtn?.classList.contains('hidden')).toBe(false);
    executeBtn?.click();
    expect(mockExecute).not.toHaveBeenCalled();
    expect(document.querySelector('.modal-confirm-body')?.textContent).toContain('USD 12.50');
    document.querySelector<HTMLButtonElement>('.modal-confirm-actions .btn-destructive')?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(mockExecute).toHaveBeenCalledTimes(1);
    const execReq = mockExecute.mock.calls[0][0];
    // Pre-fix, execute never carried a region at all, so the backend's
    // validateExecuteExchangeBody rejected every UI-initiated exchange
    // with a 400. This must equal the region the quote resolved.
    expect(execReq.region).toBe('us-west-2');
  });

  it('names instance types and the hourly price change in the execute confirm', async () => {
    const mockGetQuote = api.getExchangeQuote as jest.Mock;
    mockGetQuote.mockResolvedValueOnce({
      IsValidExchange: true,
      ValidationFailureReason: '',
      CurrencyCode: 'USD',
      PaymentDueRaw: '12.50',
      SourceHourlyPriceRaw: '0.1000',
      SourceRemainingUpfrontRaw: '',
      SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '0.1250',
      TargetRemainingUpfrontRaw: '',
      TargetRemainingTotalRaw: '',
      Region: 'us-west-2',
    });
    const offeringUUID = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
    openExchangeModal('ri-abc', 3, 'm5.large', [
      { instance_type: 'm5.large', offering_id: offeringUUID, effective_monthly_cost: 42.5 },
    ]);
    Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote')?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));
    Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Execute Exchange')?.click();

    const text = document.querySelector('.modal-confirm-body')?.textContent ?? '';
    expect(text).toContain('Pay now: USD 12.50');
    expect(text).toContain('3 \u00d7 m5.large');
    // The exact offering id stays visible next to the instance type.
    expect(text).toContain(`3 \u00d7 m5.large (offering ${offeringUUID})`);
    expect(text).toContain('USD 0.1000 \u2192 0.1250 (+0.0250 USD/hr, about +18.25 USD/month)');
    expect(document.activeElement?.textContent).toBe('Cancel');
    document.querySelector<HTMLButtonElement>('.modal-confirm-close')?.click();
  });

  it('shows the hourly price change in the quote summary (issue #709)', async () => {
    (api.getExchangeQuote as jest.Mock).mockResolvedValueOnce({
      IsValidExchange: true, ValidationFailureReason: '', CurrencyCode: 'USD', PaymentDueRaw: '12.50',
      SourceHourlyPriceRaw: '0.1000', SourceRemainingUpfrontRaw: '', SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '0.1250', TargetRemainingUpfrontRaw: '', TargetRemainingTotalRaw: '', Region: 'us-west-2',
    });
    openExchangeModal('ri-abc', 3, 'm5.large', [
      { instance_type: 'm5.large', offering_id: '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91', effective_monthly_cost: 42.5 },
    ]);
    Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote')?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));
    const rows = Array.from(modal.querySelectorAll('.quote-row')).map((r) => r.textContent);
    expect(rows).toContain('Hourly Price Change:+0.0250 USD/hr, about +18.25 USD/month');
  });

  it('invalidates a fetched quote when the count changes, hiding Execute (issue #244)', async () => {
    const mockGetQuote = api.getExchangeQuote as jest.Mock;
    mockGetQuote.mockResolvedValueOnce({
      IsValidExchange: true,
      ValidationFailureReason: '',
      CurrencyCode: 'USD',
      PaymentDueRaw: '40.00',
      SourceHourlyPriceRaw: '',
      SourceRemainingUpfrontRaw: '',
      SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '',
      TargetRemainingUpfrontRaw: '',
      TargetRemainingTotalRaw: '',
      Region: 'us-west-2',
    });
    const offeringUUID = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
    openExchangeModal('ri-abc', 2, 'm5.large', [
      { instance_type: 'm5.large', offering_id: offeringUUID, effective_monthly_cost: 42.5 },
    ]);
    const quoteBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote');
    quoteBtn?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const executeBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Execute Exchange');
    expect(executeBtn?.classList.contains('hidden')).toBe(false);

    // Change the count after a valid quote was fetched -- the stale quote
    // must be discarded and Execute re-hidden until a fresh quote is fetched.
    const countInput = modal.querySelector<HTMLInputElement>('.modal-exchange-count');
    if (countInput) {
      countInput.value = '8';
      countInput.dispatchEvent(new Event('input'));
    }
    expect(executeBtn?.classList.contains('hidden')).toBe(true);
  });

  it('invalidates a fetched quote when the target picker changes (issue #244)', async () => {
    const mockGetQuote = api.getExchangeQuote as jest.Mock;
    mockGetQuote.mockResolvedValueOnce({
      IsValidExchange: true,
      ValidationFailureReason: '',
      CurrencyCode: 'USD',
      PaymentDueRaw: '40.00',
      SourceHourlyPriceRaw: '',
      SourceRemainingUpfrontRaw: '',
      SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '',
      TargetRemainingUpfrontRaw: '',
      TargetRemainingTotalRaw: '',
      Region: 'us-west-2',
    });
    const uuid1 = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
    const uuid2 = '7e123456-0000-4567-abcd-ef0123456789';
    openExchangeModal('ri-abc', 2, 'm5.large', [
      { instance_type: 'm5.large', offering_id: uuid1, effective_monthly_cost: 42.5 },
      { instance_type: 'm5.4xlarge', offering_id: uuid2, effective_monthly_cost: 320.0 },
    ]);
    const quoteBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote');
    quoteBtn?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const executeBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Execute Exchange');
    expect(executeBtn?.classList.contains('hidden')).toBe(false);

    const picker = modal.querySelector<HTMLSelectElement>('.modal-exchange-target-select');
    if (picker) {
      picker.value = uuid2;
      picker.dispatchEvent(new Event('change'));
    }
    expect(executeBtn?.classList.contains('hidden')).toBe(true);
  });

  it('invalidates a fetched quote when a target row is removed (issue #244)', async () => {
    const mockGetQuote = api.getExchangeQuote as jest.Mock;
    mockGetQuote.mockResolvedValueOnce({
      IsValidExchange: true,
      ValidationFailureReason: '',
      CurrencyCode: 'USD',
      PaymentDueRaw: '0',
      SourceHourlyPriceRaw: '',
      SourceRemainingUpfrontRaw: '',
      SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '',
      TargetRemainingUpfrontRaw: '',
      TargetRemainingTotalRaw: '',
    });
    const uuid1 = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
    const uuid2 = '7e123456-0000-4567-abcd-ef0123456789';
    openExchangeModal('ri-multi', 1, 'm5.large', [
      { instance_type: 'm5.large', offering_id: uuid1, effective_monthly_cost: 40.0 },
    ]);
    modal.querySelector<HTMLButtonElement>('#modal-exchange-add-target')?.click();
    const rows = modal.querySelectorAll<HTMLDivElement>('.exchange-target-row');
    const secondOffering = rows[1]?.querySelector<HTMLInputElement>('.modal-exchange-target');
    if (secondOffering) secondOffering.value = uuid2;

    const quoteBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote');
    quoteBtn?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const executeBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Execute Exchange');
    expect(executeBtn?.classList.contains('hidden')).toBe(false);

    modal.querySelector<HTMLButtonElement>('.exchange-remove-target')?.click();
    expect(executeBtn?.classList.contains('hidden')).toBe(true);
  });

  it('disables the Quote button while a request is in flight to prevent overlapping quotes (issue #244)', async () => {
    const mockGetQuote = api.getExchangeQuote as jest.Mock;
    let resolveQuote: ((v: unknown) => void) | undefined;
    mockGetQuote.mockImplementationOnce(() => new Promise((resolve) => { resolveQuote = resolve; }));
    const offeringUUID = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
    openExchangeModal('ri-abc', 2, 'm5.large', [
      { instance_type: 'm5.large', offering_id: offeringUUID, effective_monthly_cost: 42.5 },
    ]);
    const quoteBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote') as HTMLButtonElement;
    quoteBtn.click();
    expect(quoteBtn.disabled).toBe(true);
    quoteBtn.click(); // second click while in flight must not fire a second request
    await Promise.resolve();
    expect(mockGetQuote).toHaveBeenCalledTimes(1);
    resolveQuote?.({
      IsValidExchange: false,
      ValidationFailureReason: 'test',
      CurrencyCode: 'USD',
      PaymentDueRaw: '0',
      SourceHourlyPriceRaw: '',
      SourceRemainingUpfrontRaw: '',
      SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '',
      TargetRemainingUpfrontRaw: '',
      TargetRemainingTotalRaw: '',
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(quoteBtn.disabled).toBe(false);
  });

  it('posts targets[] when two or more rows are present', async () => {
    const mockGetQuote = api.getExchangeQuote as jest.Mock;
    mockGetQuote.mockResolvedValueOnce({
      IsValidExchange: false,
      ValidationFailureReason: 'test',
      CurrencyCode: 'USD',
      PaymentDueRaw: '0',
      SourceHourlyPriceRaw: '',
      SourceRemainingUpfrontRaw: '',
      SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '',
      TargetRemainingUpfrontRaw: '',
      TargetRemainingTotalRaw: '',
    });
    const uuid1 = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
    const uuid2 = '7e123456-0000-4567-abcd-ef0123456789';
    // Pre-seed with CE alternatives so the first row can resolve a UUID.
    openExchangeModal('ri-multi', 1, 'm5.large', [
      { instance_type: 'm5.large', offering_id: uuid1, effective_monthly_cost: 40.0 },
    ]);
    modal.querySelector<HTMLButtonElement>('#modal-exchange-add-target')?.click();

    // Inject a UUID into the second row's hidden input directly (simulates
    // the user picking from the dropdown for the second target).
    const rows = modal.querySelectorAll<HTMLDivElement>('.exchange-target-row');
    const secondOffering = rows[1]?.querySelector<HTMLInputElement>('.modal-exchange-target');
    const secondCount = rows[1]?.querySelector<HTMLInputElement>('.modal-exchange-count');
    if (secondOffering) secondOffering.value = uuid2;
    if (secondCount) secondCount.value = '2';

    const quoteBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote');
    quoteBtn?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(mockGetQuote).toHaveBeenCalledTimes(1);
    const req = mockGetQuote.mock.calls[0][0];
    expect(req.ri_ids).toEqual(['ri-multi']);
    expect(req.targets).toEqual([
      { offering_id: uuid1, count: 1 },
      { offering_id: uuid2, count: 2 },
    ]);
    expect(req.target_offering_id).toBeUndefined();
    expect(req.target_count).toBeUndefined();
  });

  it('removes a target row when the × button is clicked, but keeps at least one', () => {
    openExchangeModal('ri-abc123', 2);
    const addBtn = modal.querySelector<HTMLButtonElement>('#modal-exchange-add-target');
    addBtn?.click();
    addBtn?.click();
    // Three rows now; removing one should leave 2.
    const removes = modal.querySelectorAll<HTMLButtonElement>('.exchange-remove-target');
    removes[0]?.click();
    let rows = modal.querySelectorAll<HTMLDivElement>('.exchange-target-row');
    expect(rows.length).toBe(2);

    // Remove down to 1 row; further clicks are ignored.
    modal.querySelector<HTMLButtonElement>('.exchange-remove-target')?.click();
    rows = modal.querySelectorAll<HTMLDivElement>('.exchange-target-row');
    expect(rows.length).toBe(1);
    modal.querySelector<HTMLButtonElement>('.exchange-remove-target')?.click();
    rows = modal.querySelectorAll<HTMLDivElement>('.exchange-target-row');
    expect(rows.length).toBe(1);
  });

  it('hides modal when cancel button is clicked', () => {
    openExchangeModal('ri-abc123', 2);
    const cancelBtn = Array.from(modal.querySelectorAll('button')).find(b => b.textContent === 'Cancel');
    cancelBtn?.click();
    expect(modal.classList.contains('hidden')).toBe(true);
  });

  it('shows a cost chip when the selected offering_id matches an alternative', () => {
    // suggestedTargetType='m5.large' resolves to offering_id 'off-m5' via alternativeTargets.
    // The chip should show the cost for that offering.
    openExchangeModal('ri-abc', 2, 'm5.large', [
      { instance_type: 'm5.large', offering_id: 'off-m5', effective_monthly_cost: 42.5 },
      { instance_type: 'm6i.large', offering_id: 'off-m6i', effective_monthly_cost: 35.0 },
    ]);
    const chip = modal.querySelector<HTMLSpanElement>('.cost-chip');
    expect(chip).not.toBeNull();
    expect(chip?.textContent).toBe('$42.50/mo each');
  });

  it('shows an em-dash in the cost chip when the selected offering_id has no CE pricing match', () => {
    // 'unknown.shape' cannot be resolved to an offering_id from alternativeTargets,
    // so the hidden input stays empty and the chip shows "—".
    openExchangeModal('ri-abc', 2, 'unknown.shape', [
      { instance_type: 'm5.large', offering_id: 'off-m5', effective_monthly_cost: 42.5 },
    ]);
    const chip = modal.querySelector<HTMLSpanElement>('.cost-chip');
    expect(chip?.textContent).toBe('—');
  });

  it('computes the running total across two rows using per-row count', () => {
    openExchangeModal('ri-multi', 1, 'm5.large', [
      { instance_type: 'm5.large', offering_id: 'off-m5', effective_monthly_cost: 40.0 },
      { instance_type: 'm6i.large', offering_id: 'off-m6i', effective_monthly_cost: 30.0 },
    ]);
    const firstCountInput = modal.querySelector<HTMLInputElement>('.modal-exchange-count');
    if (firstCountInput) {
      firstCountInput.value = '2';
      firstCountInput.dispatchEvent(new Event('input'));
    }
    modal.querySelector<HTMLButtonElement>('#modal-exchange-add-target')?.click();
    const rows = modal.querySelectorAll<HTMLDivElement>('.exchange-target-row');
    const secondOffering = rows[1]?.querySelector<HTMLInputElement>('.modal-exchange-target');
    const secondCount = rows[1]?.querySelector<HTMLInputElement>('.modal-exchange-count');
    if (secondOffering) {
      // Inject the offering_id UUID directly into the hidden input and
      // trigger an input event so updateRunningTotal fires.
      secondOffering.value = 'off-m6i';
      secondOffering.dispatchEvent(new Event('input'));
    }
    if (secondCount) {
      secondCount.value = '3';
      secondCount.dispatchEvent(new Event('input'));
    }
    const total = modal.querySelector<HTMLDivElement>('#modal-exchange-running-total');
    expect(total?.textContent).toContain('$170.00/mo');
    expect(total?.textContent).not.toContain('incomplete');
  });

  it('marks the running total as incomplete when some rows have no pricing match', () => {
    openExchangeModal('ri-incomplete', 1, 'm5.large', [
      { instance_type: 'm5.large', offering_id: 'off-m5', effective_monthly_cost: 40.0 },
    ]);
    modal.querySelector<HTMLButtonElement>('#modal-exchange-add-target')?.click();
    const rows = modal.querySelectorAll<HTMLDivElement>('.exchange-target-row');
    const secondOffering = rows[1]?.querySelector<HTMLInputElement>('.modal-exchange-target');
    if (secondOffering) {
      // 'unknown-offering' does not match any CE alternative offering_id.
      secondOffering.value = 'unknown-offering';
      secondOffering.dispatchEvent(new Event('input'));
    }
    const total = modal.querySelector<HTMLDivElement>('#modal-exchange-running-total');
    expect(total?.textContent).toContain('$40.00/mo');
    expect(total?.textContent).toContain('incomplete');
  });

  it('hides the running total when called without alternativeTargets', () => {
    openExchangeModal('ri-no-alts', 2, 'm5.large');
    const total = modal.querySelector<HTMLDivElement>('#modal-exchange-running-total');
    expect(total?.classList.contains('hidden')).toBe(true);
  });

  it('does not throw when modal element is missing', () => {
    document.body.innerHTML = '';
    expect(() => openExchangeModal('ri-abc123', 2)).not.toThrow();
  });

  // Defect 1 -- picker tests
  it('renders a select picker (not a free-text input) for the target offering', () => {
    openExchangeModal('ri-abc123', 2);
    // There must be a <select> for the picker.
    const picker = modal.querySelector<HTMLSelectElement>('.modal-exchange-target-select');
    expect(picker).not.toBeNull();
    // There must NOT be a visible text input (the hidden field has type="hidden").
    const textInput = modal.querySelector<HTMLInputElement>('input[type="text"].modal-exchange-target');
    expect(textInput).toBeNull();
  });

  it('populates the select with CE recommendation options when alternativeTargets are provided', async () => {
    openExchangeModal('ri-abc', 2, undefined, [
      { instance_type: 'm5.large', offering_id: 'off-m5', effective_monthly_cost: 42.5 },
      { instance_type: 'm6i.large', offering_id: 'off-m6i', effective_monthly_cost: 35.0 },
    ]);
    // CE recommendations land in the select synchronously (no async fetch needed).
    const picker = modal.querySelector<HTMLSelectElement>('.modal-exchange-target-select');
    const options = picker ? Array.from(picker.querySelectorAll('option')) : [];
    const optionValues = options.map((o) => o.value);
    expect(optionValues).toContain('off-m5');
    expect(optionValues).toContain('off-m6i');
  });

  it('populates the select with AWS offerings after async load completes', async () => {
    const mockListOfferings = api.listTargetOfferings as jest.Mock;
    mockListOfferings.mockResolvedValueOnce([
      {
        offering_id: '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91',
        instance_type: 'm5.xlarge',
        offering_type: 'No Upfront',
        product_description: 'Linux/UNIX',
        duration: 31536000,
        fixed_price: 0,
        usage_price: 0.12,
        currency_code: 'USD',
        scope: 'Region',
        normalization_factor: 8,
      },
    ]);
    openExchangeModal('ri-abc', 2);
    // Let the async populateAwsOfferings() settle.
    await new Promise((resolve) => setTimeout(resolve, 10));

    const picker = modal.querySelector<HTMLSelectElement>('.modal-exchange-target-select');
    const options = picker ? Array.from(picker.querySelectorAll('option')) : [];
    const optionValues = options.map((o) => o.value);
    expect(optionValues).toContain('4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91');
    expect(mockListOfferings).toHaveBeenCalledWith('ri-abc');
  });

  it('shows an error with Retry, not a "type a UUID" dead end, when offerings fail to load (issue #740)', async () => {
    const mockListOfferings = api.listTargetOfferings as jest.Mock;
    mockListOfferings.mockRejectedValueOnce(new Error('throttled'));
    openExchangeModal('ri-abc', 2);
    await new Promise((resolve) => setTimeout(resolve, 10));

    const picker = modal.querySelector<HTMLSelectElement>('.modal-exchange-target-select');
    expect(picker?.options[0]?.textContent).toBe('Could not load offerings');
    expect(modal.textContent).not.toContain('UUID');
    const errorRow = modal.querySelector<HTMLElement>('.exchange-offerings-error');
    expect(errorRow?.classList.contains('hidden')).toBe(false);
    expect(errorRow?.querySelector('[role="alert"]')?.textContent).toBe('Could not load target offerings: throttled');

    mockListOfferings.mockResolvedValueOnce([{
      offering_id: '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91', instance_type: 'm5.xlarge', offering_type: 'No Upfront',
      product_description: 'Linux/UNIX', duration: 31536000, fixed_price: 0, usage_price: 0.12,
      currency_code: 'USD', scope: 'Region', normalization_factor: 8,
    }]);
    Array.from(errorRow?.querySelectorAll('button') ?? []).find((b) => b.textContent === 'Retry')?.click();
    expect(picker?.options[0]?.textContent).toBe('Loading offerings...');
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(mockListOfferings).toHaveBeenCalledTimes(2);
    expect(errorRow?.classList.contains('hidden')).toBe(true);
    expect(Array.from(picker?.options ?? []).map((o) => o.value)).toContain('4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91');
  });

  it('keeps CE recommendations selectable when the AWS offerings fail to load', async () => {
    (api.listTargetOfferings as jest.Mock).mockRejectedValueOnce(new Error('throttled'));
    openExchangeModal('ri-abc', 2, undefined, [
      { instance_type: 'm5.large', offering_id: '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91', effective_monthly_cost: 42.5 },
    ]);
    await new Promise((resolve) => setTimeout(resolve, 10));
    const picker = modal.querySelector<HTMLSelectElement>('.modal-exchange-target-select');
    expect(picker?.options[0]?.textContent).toBe('AWS offerings unavailable -- pick a recommendation or retry');
    expect(Array.from(picker?.options ?? []).map((o) => o.value)).toContain('4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91');
  });

  it('selecting a picker option drives the hidden offering input with a UUID', () => {
    openExchangeModal('ri-abc', 2, undefined, [
      { instance_type: 'm5.large', offering_id: '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91', effective_monthly_cost: 42.5 },
    ]);
    const picker = modal.querySelector<HTMLSelectElement>('.modal-exchange-target-select');
    const hiddenInput = modal.querySelector<HTMLInputElement>('.modal-exchange-target');
    if (picker) {
      picker.value = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
      picker.dispatchEvent(new Event('change'));
    }
    expect(hiddenInput?.value).toBe('4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91');
  });

  it('rejects submission when the hidden offering input contains a non-UUID value', async () => {
    const mockGetQuote = api.getExchangeQuote as jest.Mock;
    openExchangeModal('ri-abc', 1);
    // Force a non-UUID value into the hidden input (simulates a bypass attempt).
    const hiddenInput = modal.querySelector<HTMLInputElement>('.modal-exchange-target');
    if (hiddenInput) hiddenInput.value = 't3.medium';

    const quoteBtn = Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote');
    quoteBtn?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    // The quote API must NOT have been called -- the frontend guard fires first.
    expect(mockGetQuote).not.toHaveBeenCalled();
    // The result container must show an error mentioning the invalid value.
    const result = modal.querySelector('#modal-exchange-result');
    expect(result?.textContent).toContain('t3.medium');
  });

  describe('exchange result announcement and receipt (issue #738)', () => {
    const offeringUUID = '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91';
    const flush = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));
    const button = (label: string): HTMLButtonElement | undefined =>
      Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === label);

    async function executeExchange(): Promise<void> {
      (api.getExchangeQuote as jest.Mock).mockResolvedValueOnce({
        IsValidExchange: true, ValidationFailureReason: '', CurrencyCode: 'USD', PaymentDueRaw: '12.50',
        SourceHourlyPriceRaw: '', SourceRemainingUpfrontRaw: '', SourceRemainingTotalRaw: '',
        TargetHourlyPriceRaw: '', TargetRemainingUpfrontRaw: '', TargetRemainingTotalRaw: '', Region: 'us-west-2',
      });
      (api.executeExchange as jest.Mock).mockResolvedValueOnce({ exchange_id: 'exch-42', quote: {} });
      openExchangeModal('ri-abc', 1, 'm5.large', [
        { instance_type: 'm5.large', offering_id: offeringUUID, effective_monthly_cost: 42.5 },
      ]);
      button('Get Quote')?.click();
      await flush();
      button('Execute Exchange')?.click();
      document.querySelector<HTMLButtonElement>('.modal-confirm-actions .btn-destructive')?.click();
      await flush();
      await flush();
    }

    it('makes the result area a polite live region and announces errors as alerts', async () => {
      (api.getExchangeQuote as jest.Mock).mockRejectedValueOnce(new Error('throttled'));
      openExchangeModal('ri-abc', 1, 'm5.large', [
        { instance_type: 'm5.large', offering_id: offeringUUID, effective_monthly_cost: 42.5 },
      ]);
      const result = modal.querySelector('#modal-exchange-result');
      expect(result?.getAttribute('role')).toBe('status');
      expect(result?.getAttribute('aria-live')).toBe('polite');
      button('Get Quote')?.click();
      await flush();
      const alert = result?.querySelector('[role="alert"]');
      expect(alert?.textContent).toBe('Quote failed: throttled');
    });

    it('keeps the exchange ID on screen until the user closes the modal', async () => {
      await executeExchange();
      await new Promise((resolve) => setTimeout(resolve, 2100));
      expect(modal.classList.contains('hidden')).toBe(false);
      expect(modal.querySelector('#modal-exchange-result')?.textContent).toBe('Exchange completed. ID: exch-42');
      // The form is frozen so an edit cannot wipe the receipt, and only Close remains.
      expect(button('Get Quote')?.classList.contains('hidden')).toBe(true);
      expect(modal.querySelector<HTMLSelectElement>('.modal-exchange-target-select')?.disabled).toBe(true);
      const close = button('Close');
      expect(document.activeElement).toBe(close);
      close?.click();
      expect(modal.classList.contains('hidden')).toBe(true);
    });

    it('shows a success toast with the exchange ID and a link to the history', async () => {
      const section = document.createElement('section');
      const list = document.createElement('div');
      list.id = 'ri-exchange-history-list';
      section.appendChild(list);
      section.scrollIntoView = jest.fn();
      document.body.appendChild(section);

      await executeExchange();
      const toast = document.querySelector('.toast--success');
      expect(toast?.querySelector('.toast-message')?.textContent).toBe('RI exchange completed. Exchange ID: exch-42');
      Array.from(toast?.querySelectorAll('button') ?? []).find((b) => b.textContent === 'View history')?.click();
      expect(modal.classList.contains('hidden')).toBe(true);
      expect(section.scrollIntoView).toHaveBeenCalled();
      expect(document.activeElement).toBe(list);
    });
  });
});

describe('fillQuoteFromRI', () => {
  let modal: HTMLDivElement;

  beforeEach(() => {
    modal = createModal();
    (api.listTargetOfferings as jest.Mock).mockResolvedValue([]);
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('opens the exchange modal', () => {
    fillQuoteFromRI('ri-abc123', 2);
    expect(modal.classList.contains('hidden')).toBe(false);
  });

  it('displays the RI ID in the modal', () => {
    fillQuoteFromRI('ri-xyz', 3);
    expect(modal.textContent).toContain('ri-xyz');
  });

  it('does not throw when modal is missing', () => {
    const body = document.body;
    while (body.firstChild) body.removeChild(body.firstChild);
    expect(() => fillQuoteFromRI('ri-abc123', 1)).not.toThrow();
  });
});

// Reshape-table rendering tests. Kept in a separate describe block so
// the shared modal-element fixture doesn't leak in.
describe('reshape recommendations table', () => {
  let tableContainer: HTMLDivElement;

  beforeEach(() => {
    tableContainer = document.createElement('div');
    tableContainer.id = 'ri-exchange-recommendations-list';
    document.body.appendChild(tableContainer);
    // Re-apply the column-filter mock impls after a prior test's
    // jest.resetAllMocks(); without this the renderers blow up on
    // Object.entries(undefined) when reading filter state.
    const stateMod = jest.requireMock('../state') as {
      getRiExchangeColumnFilters: jest.Mock;
      getActiveRiColumnFilters: jest.Mock;
    };
    stateMod.getRiExchangeColumnFilters.mockReturnValue({});
    stateMod.getActiveRiColumnFilters.mockReturnValue({});
  });

  afterEach(() => {
    const body = document.body;
    while (body.firstChild) body.removeChild(body.firstChild);
    jest.resetAllMocks();
  });

  // Baseline rec with the reshape fields the renderer expects. Tests
  // extend this with alternative_targets when needed.
  const baseRec = {
    source_ri_id: 'ri-abc',
    source_instance_type: 'm5.xlarge',
    source_count: 1,
    target_instance_type: 'm5.large',
    target_count: 2,
    utilization_percent: 50,
    normalized_used: 4,
    normalized_purchased: 8,
    reason: 'underutilized',
  };

  it('renders the Alternatives column with cost chips when the rec carries alternative_targets', async () => {
    const mockGet = api.getReshapeRecommendations as jest.Mock;
    mockGet.mockResolvedValueOnce({
      recommendations: [
        {
          ...baseRec,
          alternative_targets: [
            { instance_type: 'm7g.large', offering_id: 'off-m7g', effective_monthly_cost: 30.0 },
            { instance_type: 'm6i.large', offering_id: 'off-m6i', effective_monthly_cost: 35.0 },
          ],
        },
      ],
      recs_staleness: '',
      recs_collected_at: null,
    });

    await loadReshapeRecommendations();

    const chips = tableContainer.querySelectorAll<HTMLSpanElement>('.cost-chip');
    expect(chips.length).toBe(2);
    // Chips include the instance type + formatted cost; the backend
    // emits them in ascending-cost order (commit 7378ceaa5 sorts
    // fillAlternativesFromOfferings output by EffectiveMonthlyCost)
    // so the UI shows cheapest first.
    expect(chips[0]?.textContent).toContain('m7g.large');
    expect(chips[0]?.textContent).toContain('$30.00/mo');
    expect(chips[1]?.textContent).toContain('m6i.large');
    expect(chips[1]?.textContent).toContain('$35.00/mo');
  });

  it('renders an unknown utilization as a neutral em-dash instead of throwing (#136)', async () => {
    const mockGet = api.getReshapeRecommendations as jest.Mock;
    const { utilization_percent: _omit, ...withoutUtil } = baseRec;
    mockGet.mockResolvedValueOnce({
      recommendations: [withoutUtil, { ...baseRec, source_ri_id: 'ri-def', utilization_percent: 97 }],
      recs_staleness: '',
      recs_collected_at: null,
    });

    await loadReshapeRecommendations();

    const rows = tableContainer.querySelectorAll<HTMLTableRowElement>('tbody tr');
    expect(rows).toHaveLength(2);
    const unknown = rows[0]!.querySelectorAll('td')[4]!;
    expect(unknown.textContent).toBe('—');
    expect(unknown.className).toBe('');
    const known = rows[1]!.querySelectorAll('td')[4]!;
    expect(known.textContent).toBe('97.0%');
    expect(known.className).toBe('util-green');
  });

  it('renders an em-dash in the Alternatives column when the rec has no alternative_targets', async () => {
    const mockGet = api.getReshapeRecommendations as jest.Mock;
    mockGet.mockResolvedValueOnce({ recommendations: [{ ...baseRec }], recs_staleness: '', recs_collected_at: null }); // no alternative_targets

    await loadReshapeRecommendations();

    // The Alternatives <td> is the 4th column (Source RI, Current,
    // Suggested, Alternatives, Utilization, Normalized Units, Reason,
    // Actions). Select the single data row and pull the 4th cell.
    const row = tableContainer.querySelector<HTMLTableRowElement>('tbody tr');
    expect(row).not.toBeNull();
    const cells = row?.querySelectorAll<HTMLTableCellElement>('td');
    expect(cells?.[3]?.textContent).toBe('—');
    expect(tableContainer.querySelectorAll('.cost-chip').length).toBe(0);
  });
});

// Empty-state copy varies with RI-fleet presence. Prior to commit P4 the
// "All convertible RIs are well-utilized" copy ran on every zero-rec state
// including a totally empty fleet — a truthy claim about an empty set.
describe('reshape recommendations empty state', () => {
  let instancesEl: HTMLDivElement;
  let recsEl: HTMLDivElement;
  let historyEl: HTMLDivElement;

  const sampleRI = {
    reserved_instance_id: 'ri-1',
    instance_type: 'm5.large',
    availability_zone: 'us-east-1a',
    instance_count: 1,
    start: '2026-01-01T00:00:00Z',
    end: '2027-01-01T00:00:00Z',
    offering_type: 'Convertible',
    fixed_price: 0,
    usage_price: 0,
    state: 'active',
    normalization_factor: 4,
  };

  beforeEach(() => {
    instancesEl = document.createElement('div');
    instancesEl.id = 'ri-exchange-instances-list';
    recsEl = document.createElement('div');
    recsEl.id = 'ri-exchange-recommendations-list';
    historyEl = document.createElement('div');
    historyEl.id = 'ri-exchange-history-list';
    document.body.append(instancesEl, recsEl, historyEl);

    (api.getRIUtilization as jest.Mock).mockResolvedValue([]);
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([]);
    (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({ recommendations: [], recs_staleness: '', recs_collected_at: null });
    // resetAllMocks() in afterEach wipes the state mock implementations;
    // loadRIExchange reads the chips to scope the request (issue #871), so
    // restore the AWS/all-accounts default here. Also restore the column-
    // filter slices so renderRIsTable / renderRecommendations don't crash
    // on Object.entries(undefined) for non-empty tables.
    const stateMod = jest.requireMock('../state') as {
      getCurrentProvider: jest.Mock;
      getCurrentAccountIDs: jest.Mock;
      getRiExchangeColumnFilters: jest.Mock;
      getActiveRiColumnFilters: jest.Mock;
    };
    stateMod.getCurrentProvider.mockReturnValue('aws');
    stateMod.getCurrentAccountIDs.mockReturnValue([]);
    stateMod.getRiExchangeColumnFilters.mockReturnValue({});
    stateMod.getActiveRiColumnFilters.mockReturnValue({});
  });

  afterEach(() => {
    const body = document.body;
    while (body.firstChild) body.removeChild(body.firstChild);
    jest.resetAllMocks();
  });

  it('advises the user their accounts have no convertible RIs yet when both lists are empty', async () => {
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([]);

    await loadRIExchange();

    expect(recsEl.textContent).toContain('none are registered yet');
    expect(recsEl.textContent).not.toContain('well-utilized');
    expect(recsEl.textContent).not.toContain('utilization threshold');
  });

  it('claims all RIs meet the threshold only when RIs actually exist', async () => {
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([sampleRI]);

    await loadRIExchange();

    expect(recsEl.textContent).toContain('meet your utilization threshold');
    expect(recsEl.textContent).toContain('1 convertible RI ');
    expect(recsEl.textContent).not.toContain('none are registered');
  });

  // Issue #1135: the three loads race. An empty reshape response that lands
  // before the RI list must not keep the previous scope's count.
  it('re-renders the empty-state count when recommendations resolve before the RI list', async () => {
    (api.listConvertibleRIs as jest.Mock).mockResolvedValueOnce([sampleRI]);
    await loadRIExchange();
    expect(recsEl.textContent).toContain('All 1 convertible RI ');

    let releaseRIs: (v: unknown) => void = () => undefined;
    (api.listConvertibleRIs as jest.Mock).mockReturnValueOnce(new Promise(r => { releaseRIs = r; }));
    const reload = loadRIExchange();
    await new Promise(r => setTimeout(r, 0));
    // Recommendations are in, RIs are not: no stale "All 1" claim.
    expect(api.getReshapeRecommendations).toHaveBeenCalledTimes(2);
    expect(recsEl.textContent).not.toContain('All 1 convertible RI');
    expect(recsEl.textContent).toContain('No reshape recommendations right now.');

    releaseRIs([sampleRI, { ...sampleRI, reserved_instance_id: 'ri-2' }, { ...sampleRI, reserved_instance_id: 'ri-3' }]);
    await reload;
    expect(recsEl.textContent).toContain('All 3 convertible RIs meet your utilization threshold');
  });

  it('does not quote a count from a failed RI load', async () => {
    (api.listConvertibleRIs as jest.Mock).mockResolvedValueOnce([sampleRI]);
    await loadRIExchange();
    (api.listConvertibleRIs as jest.Mock).mockRejectedValueOnce(new Error('boom'));
    await loadRIExchange();
    expect(recsEl.textContent).toContain('No reshape recommendations right now.');
    expect(recsEl.textContent).not.toContain('convertible RI');
  });

  it('keeps the error from a failed reshape load after the RI list arrives', async () => {
    let releaseRIs: (v: unknown) => void = () => undefined;
    (api.listConvertibleRIs as jest.Mock).mockReturnValueOnce(new Promise(r => { releaseRIs = r; }));
    (api.getReshapeRecommendations as jest.Mock).mockRejectedValueOnce(new Error('ce down'));
    const reload = loadRIExchange();
    await new Promise(r => setTimeout(r, 0));
    releaseRIs([sampleRI]);
    await reload;
    expect(recsEl.querySelector('.error')?.textContent).toContain('Failed to load recommendations: ce down');
    expect(recsEl.textContent).not.toContain('utilization threshold');
  });
});

// Staleness banner tests for issue #150.
describe('renderReshapeStalenessBanner', () => {
  let listEl: HTMLDivElement;

  beforeEach(() => {
    const wrapper = document.createElement('div');
    listEl = document.createElement('div');
    listEl.id = 'ri-exchange-recommendations-list';
    wrapper.appendChild(listEl);
    document.body.appendChild(wrapper);
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders no banner when staleness is empty', () => {
    renderReshapeStalenessBanner('', null);
    const banner = document.getElementById('ri-exchange-recommendations-freshness');
    expect(banner?.textContent).toBe('');
  });

  it('renders a soft-warning banner for staleness=soft', () => {
    renderReshapeStalenessBanner('soft', null);
    const banner = document.getElementById('ri-exchange-recommendations-freshness');
    expect(banner?.className).toContain('warning');
    expect(banner?.textContent).toContain('may be up to 24h old');
  });

  it('renders a hard-warning banner for staleness=hard', () => {
    renderReshapeStalenessBanner('hard', null);
    const banner = document.getElementById('ri-exchange-recommendations-freshness');
    expect(banner?.className).toContain('error');
    expect(banner?.textContent).toContain('older than 24h');
  });

  it('includes an age label when recs_collected_at is provided', () => {
    const twoHoursAgo = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();
    renderReshapeStalenessBanner('soft', twoHoursAgo);
    const banner = document.getElementById('ri-exchange-recommendations-freshness');
    expect(banner?.textContent).toContain('last collected 2h');
  });

  it('clears an existing banner when staleness becomes empty', () => {
    renderReshapeStalenessBanner('hard', null);
    expect(document.getElementById('ri-exchange-recommendations-freshness')?.className).toContain('error');
    renderReshapeStalenessBanner('', null);
    const banner = document.getElementById('ri-exchange-recommendations-freshness');
    expect(banner?.textContent).toBe('');
    expect(banner?.className).toBe('');
  });
});

describe('⚙︎ Exchange settings deep-link', () => {
  beforeEach(() => {
    const btn = document.createElement('button');
    btn.id = 'ri-exchange-settings-btn';
    document.body.appendChild(btn);
  });

  afterEach(() => {
    const body = document.body;
    while (body.firstChild) body.removeChild(body.firstChild);
    jest.resetAllMocks();
  });

  // Issue #1775: 'settings' was the pre-#340 tab name, so switchTab fell back
  // to Home while switchSettingsSubTab still set the Admin title and pushed
  // /admin/purchasing -- URL and title said Admin over the Home dashboard.
  it('switches to Admin → Purchasing when clicked', () => {
    setupRIExchangeHandlers();
    const btn = document.getElementById('ri-exchange-settings-btn')!;
    btn.click();
    expect(navigation.switchTab).toHaveBeenCalledWith('admin', { push: false, skipDefaultLoad: true });
    expect(navigation.switchSettingsSubTab).toHaveBeenCalledWith('purchasing');
  });
});

// issue #186: provider/account subscriptions on the RI Exchange tab
describe('RI Exchange filter subscriptions (issue #186)', () => {
  let instancesEl: HTMLDivElement;
  let recsEl: HTMLDivElement;
  let historyEl: HTMLDivElement;
  let riExchangePanel: HTMLDivElement;

  beforeEach(() => {
    instancesEl = document.createElement('div');
    instancesEl.id = 'ri-exchange-instances-list';
    recsEl = document.createElement('div');
    recsEl.id = 'ri-exchange-recommendations-list';
    historyEl = document.createElement('div');
    historyEl.id = 'ri-exchange-history-list';
    // The sub-tab panel must exist and be visible for the guard to pass.
    riExchangePanel = document.createElement('div');
    riExchangePanel.id = 'inventory-ri-exchange';
    document.body.append(instancesEl, recsEl, historyEl, riExchangePanel);

    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([]);
    (api.getRIUtilization as jest.Mock).mockResolvedValue([]);
    (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({ recommendations: [], recs_staleness: '', recs_collected_at: null });
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([]);
    _providerListeners.length = 0;
    _accountListeners.length = 0;
    // Re-apply the implementation after jest.resetAllMocks() from a prior
    // describe block may have cleared it.
    const stateMod = jest.requireMock('../state') as {
      subscribeProvider: jest.Mock;
      subscribeAccount: jest.Mock;
      getCurrentProvider: jest.Mock;
      getCurrentAccountIDs: jest.Mock;
    };
    stateMod.subscribeProvider.mockImplementation((cb: () => void) => {
      _providerListeners.push(cb);
      return () => undefined;
    });
    stateMod.subscribeAccount.mockImplementation((cb: () => void) => {
      _accountListeners.push(cb);
      return () => undefined;
    });
    // loadRIExchange now reads the provider/account chips to scope the
    // request (issue #871); restore their implementations too.
    stateMod.getCurrentProvider.mockImplementation(() => 'aws');
    stateMod.getCurrentAccountIDs.mockImplementation(() => []);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    // Use clearAllMocks rather than resetAllMocks so the subscribeProvider/
    // subscribeAccount mock implementations (which push to _providerListeners)
    // are preserved across tests in this block.
    jest.clearAllMocks();
  });

  it('setupRIExchangeHandlers registers subscribeProvider and subscribeAccount', () => {
    const stateMod = jest.requireMock('../state');
    setupRIExchangeHandlers();
    expect(stateMod.subscribeProvider).toHaveBeenCalled();
    expect(stateMod.subscribeAccount).toHaveBeenCalled();
  });

  it('a provider change triggers loadRIExchange when the sub-tab is active', async () => {
    setupRIExchangeHandlers();
    // Fire the provider listener (simulates topbar provider change).
    _providerListeners.forEach(cb => cb());
    // Flush the microtask queue: queueMicrotask fires after all pending
    // micro-ticks; wrapping in a resolved promise ensures we drain it.
    await new Promise(r => setTimeout(r, 0));
    expect(api.listConvertibleRIs).toHaveBeenCalled();
  });

  it('a provider change does NOT trigger loadRIExchange when the sub-tab is hidden', async () => {
    riExchangePanel.classList.add('hidden');
    setupRIExchangeHandlers();
    _providerListeners.forEach(cb => cb());
    await new Promise(r => setTimeout(r, 0));
    expect(api.listConvertibleRIs).not.toHaveBeenCalled();
  });

  it('an account change triggers loadRIExchange when the sub-tab is active', async () => {
    setupRIExchangeHandlers();
    _accountListeners.forEach(cb => cb());
    await new Promise(r => setTimeout(r, 0));
    expect(api.listConvertibleRIs).toHaveBeenCalled();
  });

  it('coalesces provider and account changes into a single reload', async () => {
    setupRIExchangeHandlers();
    // Simulate topbar filter cascade: provider change triggers account reset
    _providerListeners.forEach(cb => cb());
    _accountListeners.forEach(cb => cb());
    await new Promise(r => setTimeout(r, 0));
    // Should be called once, not twice
    expect(api.listConvertibleRIs).toHaveBeenCalledTimes(1);
  });
});

// issue #871: RI Exchange must honour the Main Header global Provider/Account
// filter, matching the Active Commitments + Coverage sub-tabs (#866/#881).
describe('RI Exchange global filter scoping (issue #871)', () => {
  let instancesEl: HTMLDivElement;
  let recsEl: HTMLDivElement;
  let historyEl: HTMLDivElement;
  let riExchangePanel: HTMLDivElement;

  const stateMod = (): {
    getCurrentProvider: jest.Mock;
    getCurrentAccountIDs: jest.Mock;
    subscribeProvider: jest.Mock;
    subscribeAccount: jest.Mock;
  } => jest.requireMock('../state');

  beforeEach(() => {
    instancesEl = document.createElement('div');
    instancesEl.id = 'ri-exchange-instances-list';
    recsEl = document.createElement('div');
    recsEl.id = 'ri-exchange-recommendations-list';
    historyEl = document.createElement('div');
    historyEl.id = 'ri-exchange-history-list';
    riExchangePanel = document.createElement('div');
    riExchangePanel.id = 'inventory-ri-exchange';
    document.body.append(instancesEl, recsEl, historyEl, riExchangePanel);

    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([]);
    (api.listExchangeableAzureRIs as jest.Mock).mockResolvedValue([]);
    (api.listAccountsMinimal as jest.Mock).mockResolvedValue([
      { id: 'uuid-1', name: 'Prod', external_id: '123456789012', provider: 'aws' },
      { id: 'uuid-2', name: 'Other', external_id: '210987654321', provider: 'aws' },
      { id: 'uuid-az', name: 'Sub', external_id: 'aaaaaaaa-0000-0000-0000-bbbbbbbbbbbb', provider: 'azure' },
    ]);
    (api.getRIUtilization as jest.Mock).mockResolvedValue([]);
    (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({ recommendations: [], recs_staleness: '', recs_collected_at: null });
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([]);

    _providerListeners.length = 0;
    _accountListeners.length = 0;
    const s = stateMod();
    s.subscribeProvider.mockImplementation((cb: () => void) => { _providerListeners.push(cb); return () => undefined; });
    s.subscribeAccount.mockImplementation((cb: () => void) => { _accountListeners.push(cb); return () => undefined; });
    s.getCurrentProvider.mockReturnValue('aws');
    s.getCurrentAccountIDs.mockReturnValue([]);
    // Restore column-filter slices wiped by a prior resetAllMocks() so
    // renderRIsTable / renderRecommendations don't throw on non-empty data.
    (s as unknown as { getActiveRiColumnFilters: jest.Mock }).getActiveRiColumnFilters?.mockReturnValue({});
    (s as unknown as { getRiExchangeColumnFilters: jest.Mock }).getRiExchangeColumnFilters?.mockReturnValue({});
  });

  afterEach(() => {
    document.body.innerHTML = '';
    jest.clearAllMocks();
  });

  it('forwards the single selected account to the AWS list endpoint', async () => {
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-1']);
    await loadRIExchange();
    expect(api.listConvertibleRIs).toHaveBeenCalledWith('123456789012');
  });

  it('keeps the account scope and refreshes reshape recs after a successful exchange (closes #285)', async () => {
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-1']);
    (api.listTargetOfferings as jest.Mock).mockResolvedValue([]);
    const modal = createModal();
    (api.getExchangeQuote as jest.Mock).mockResolvedValueOnce({
      IsValidExchange: true, ValidationFailureReason: '', CurrencyCode: 'USD', PaymentDueRaw: '12.50',
      SourceHourlyPriceRaw: '', SourceRemainingUpfrontRaw: '', SourceRemainingTotalRaw: '',
      TargetHourlyPriceRaw: '', TargetRemainingUpfrontRaw: '', TargetRemainingTotalRaw: '', Region: 'us-west-2',
    });
    (api.executeExchange as jest.Mock).mockResolvedValueOnce({ exchange_id: 'exch-1', quote: {} });
    openExchangeModal('ri-abc', 3, 'm5.large', [
      { instance_type: 'm5.large', offering_id: '4b2293b4-5fbc-4017-9c75-d5a9d3aa8c91', effective_monthly_cost: 42.5 },
    ]);
    Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Get Quote')?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));
    Array.from(modal.querySelectorAll('button')).find((b) => b.textContent === 'Execute Exchange')?.click();
    document.querySelector<HTMLButtonElement>('.modal-confirm-actions .btn-destructive')?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(api.executeExchange).toHaveBeenCalledTimes(1);
    expect(api.listConvertibleRIs).toHaveBeenCalledWith('123456789012');
    expect(api.listConvertibleRIs).not.toHaveBeenCalledWith(undefined);
    expect(api.getReshapeRecommendations).toHaveBeenCalled();
  });

  it('forwards the Azure subscription GUID (external_id), not the account UUID', async () => {
    stateMod().getCurrentProvider.mockReturnValue('azure');
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-az']);
    await loadRIExchange();
    expect(api.listExchangeableAzureRIs).toHaveBeenCalledWith('aaaaaaaa-0000-0000-0000-bbbbbbbbbbbb');
  });

  it('shows an error and calls no list endpoint when the selected account is unknown', async () => {
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-missing']);
    await loadRIExchange();
    expect(api.listConvertibleRIs).not.toHaveBeenCalled();
    expect(instancesEl.textContent).toContain('uuid-missing');
    expect(instancesEl.querySelector('.error')).not.toBeNull();
  });

  it('a pending utilization fetch from a prior scope cannot overwrite the resolve error', async () => {
    const sampleRI = {
      reserved_instance_id: 'ri-old', instance_type: 'm5.large', availability_zone: 'us-east-1a',
      instance_count: 1, start: '2026-01-01T00:00:00Z', end: '2027-01-01T00:00:00Z',
      offering_type: 'Convertible', fixed_price: 0, usage_price: 0, state: 'active', normalization_factor: 4,
    };
    let releaseUtil: (v: unknown) => void = () => undefined;
    (api.getRIUtilization as jest.Mock).mockReturnValueOnce(new Promise(r => { releaseUtil = r; }));
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([sampleRI]);
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-1']);
    await loadRIExchange();
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-missing']);
    await loadRIExchange();
    releaseUtil([]);
    await new Promise(r => setTimeout(r, 0));
    expect(instancesEl.querySelector('.error')).not.toBeNull();
    expect(instancesEl.textContent).not.toContain('ri-old');
    expect(recsEl.textContent).toContain('Select a valid account');
    expect(historyEl.textContent).toContain('Select a valid account');
  });

  it('drops a superseded failing resolve instead of rendering its error', async () => {
    let rejectLookup: (e: Error) => void = () => undefined;
    (api.listAccountsMinimal as jest.Mock).mockReturnValueOnce(new Promise((_, rej) => { rejectLookup = rej; }));
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-1']);
    const first = loadRIExchange();
    stateMod().getCurrentAccountIDs.mockReturnValue([]);
    await loadRIExchange();
    rejectLookup(new Error('boom'));
    await first;
    expect(instancesEl.querySelector('.error')).toBeNull();
    expect(instancesEl.textContent).not.toContain('boom');
  });

  it('shows the request failure and makes no list call when listAccountsMinimal rejects', async () => {
    (api.listAccountsMinimal as jest.Mock).mockRejectedValueOnce(new Error('HTTP 500'));
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-1']);
    await loadRIExchange();
    expect(api.listConvertibleRIs).not.toHaveBeenCalled();
    expect(instancesEl.textContent).toContain('HTTP 500');
  });

  it('fails loudly when the selected account has no external_id', async () => {
    (api.listAccountsMinimal as jest.Mock).mockResolvedValueOnce([
      { id: 'uuid-1', name: 'Prod', external_id: '', provider: 'aws' },
    ]);
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-1']);
    await loadRIExchange();
    expect(api.listConvertibleRIs).not.toHaveBeenCalled();
    expect(instancesEl.querySelector('.error')).not.toBeNull();
  });

  it('in-flight loaders from a superseded load cannot overwrite a later resolve error', async () => {
    const pending = <T,>() => {
      let resolve: (v: T) => void = () => undefined;
      const promise = new Promise<T>(r => { resolve = r; });
      return { promise, resolve };
    };
    const ris = pending<unknown[]>();
    const recs = pending<unknown>();
    const hist = pending<unknown[]>();
    (api.listConvertibleRIs as jest.Mock).mockReturnValueOnce(ris.promise);
    (api.getReshapeRecommendations as jest.Mock).mockReturnValueOnce(recs.promise);
    (api.getRIExchangeHistory as jest.Mock).mockReturnValueOnce(hist.promise);
    const loadA = loadRIExchange();
    await new Promise(r => setTimeout(r, 0));
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-missing']);
    await loadRIExchange();
    ris.resolve([{
      reserved_instance_id: 'ri-old', instance_type: 'm5.large', availability_zone: 'us-east-1a',
      instance_count: 1, start: '2026-01-01T00:00:00Z', end: '2027-01-01T00:00:00Z',
      offering_type: 'Convertible', fixed_price: 0, usage_price: 0, state: 'active', normalization_factor: 4,
    }]);
    recs.resolve({ recommendations: [], recs_staleness: '', recs_collected_at: null });
    hist.resolve([]);
    await loadA;
    expect(instancesEl.querySelector('.error')).not.toBeNull();
    expect(instancesEl.textContent).not.toContain('ri-old');
    expect(recsEl.textContent).toContain('Select a valid account');
    expect(historyEl.textContent).toContain('Select a valid account');
  });

  it('forwards undefined and skips the account lookup when no account is selected', async () => {
    await loadRIExchange();
    expect(api.listAccountsMinimal).not.toHaveBeenCalled();
    expect(api.listConvertibleRIs).toHaveBeenCalledWith(undefined);
  });

  it('drops a superseded load when the account chip changes mid-lookup', async () => {
    let release: (v: unknown) => void = () => undefined;
    (api.listAccountsMinimal as jest.Mock).mockReturnValueOnce(new Promise(r => { release = r; }));
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-1']);
    const first = loadRIExchange();
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-2']);
    const second = loadRIExchange();
    await second;
    release([{ id: 'uuid-1', name: 'Prod', external_id: '123456789012', provider: 'aws' }]);
    await first;
    expect(api.listConvertibleRIs).toHaveBeenCalledTimes(1);
    expect(api.listConvertibleRIs).toHaveBeenCalledWith('210987654321');
  });

  it('does not forward an account id when more than one account is selected', async () => {
    stateMod().getCurrentAccountIDs.mockReturnValue(['111111111111', '222222222222']);
    await loadRIExchange();
    expect(api.listConvertibleRIs).toHaveBeenCalledWith(undefined);
  });

  it('renders a scoped empty-state naming the AWS account', async () => {
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-1']);
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([]);
    await loadRIExchange();
    expect(instancesEl.textContent).toContain('123456789012');
  });

  it('loads Azure reservations (not AWS RIs) when provider is azure', async () => {
    stateMod().getCurrentProvider.mockReturnValue('azure');
    (api.listExchangeableAzureRIs as jest.Mock).mockResolvedValue([
      { reservation_order_id: 'o1', reservation_id: 'r1', sku: 'Standard_D2s_v3', quantity: 2, region: 'eastus', term: 'P1Y', instance_flexibility: 'On', display_name: 'web-rsv' },
    ]);
    await loadRIExchange();
    expect(api.listExchangeableAzureRIs).toHaveBeenCalled();
    expect(api.listConvertibleRIs).not.toHaveBeenCalled();
    expect(instancesEl.textContent).toContain('Standard_D2s_v3');
    expect(instancesEl.textContent).toContain('web-rsv');
    // Reshape recommendations are AWS-only -> provider-aware not-applicable copy.
    expect(recsEl.textContent).toContain('not available for Azure');
  });

  it('shows a provider-aware empty state for Azure with no reservations', async () => {
    stateMod().getCurrentProvider.mockReturnValue('azure');
    stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-az']);
    (api.listExchangeableAzureRIs as jest.Mock).mockResolvedValue([]);
    await loadRIExchange();
    expect(api.listExchangeableAzureRIs).toHaveBeenCalledWith('aaaaaaaa-0000-0000-0000-bbbbbbbbbbbb');
    expect(instancesEl.textContent).toContain('No exchangeable reservations for Azure subscription aaaaaaaa-0000-0000-0000-bbbbbbbbbbbb');
  });

  it('shows the not-available empty state for GCP and calls no list endpoint', async () => {
    stateMod().getCurrentProvider.mockReturnValue('gcp');
    await loadRIExchange();
    expect(api.listConvertibleRIs).not.toHaveBeenCalled();
    expect(api.listExchangeableAzureRIs).not.toHaveBeenCalled();
    expect(instancesEl.textContent).toContain("isn't available for GCP");
    expect(recsEl.textContent).toContain("isn't available for GCP");
    expect(historyEl.textContent).toContain("isn't available for GCP");
  });

  it('does not leave stale AWS rows when switching to GCP', async () => {
    // First load AWS with a row present.
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([{
      reserved_instance_id: 'ri-1', instance_type: 'm5.large', availability_zone: 'us-east-1a',
      instance_count: 1, start: '2026-01-01T00:00:00Z', end: '2027-01-01T00:00:00Z',
      offering_type: 'Convertible', fixed_price: 0, usage_price: 0, state: 'active', normalization_factor: 4,
    }]);
    await loadRIExchange();
    expect(instancesEl.textContent).toContain('m5.large');
    // Switch to GCP -> table must be replaced by the empty state.
    stateMod().getCurrentProvider.mockReturnValue('gcp');
    await loadRIExchange();
    expect(instancesEl.textContent).not.toContain('m5.large');
    expect(instancesEl.textContent).toContain("isn't available for GCP");
  });

  it('closes an in-progress exchange modal when the filter changes', async () => {
    const modal = createModal();
    modal.classList.remove('hidden'); // simulate an open exchange modal
    setupRIExchangeHandlers();
    _providerListeners.forEach(cb => cb());
    // Modal is closed synchronously on the chip event.
    expect(modal.classList.contains('hidden')).toBe(true);
  });

  describe('excluded-RI notice lifecycle (issue #1181)', () => {
    const noticeText = (): string => document.getElementById('ri-exchange-recommendations-excluded')?.textContent ?? '';

    beforeEach(async () => {
      (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({
        recommendations: [],
        excluded_ris: [{ ri_id: 'ri-noterm', reason: 'no term' }],
      });
      await loadRIExchange();
      expect(noticeText()).toContain('ri-noterm');
    });

    it('clears after a failed reload', async () => {
      (api.getReshapeRecommendations as jest.Mock).mockRejectedValue(new Error('boom'));
      await loadReshapeRecommendations();
      expect(noticeText()).toBe('');
    });

    it('clears when switching to GCP', async () => {
      stateMod().getCurrentProvider.mockReturnValue('gcp');
      await loadRIExchange();
      expect(noticeText()).toBe('');
    });

    it('clears when switching to Azure', async () => {
      stateMod().getCurrentProvider.mockReturnValue('azure');
      stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-az']);
      await loadRIExchange();
      expect(noticeText()).toBe('');
    });

    it('clears on an account-resolution error', async () => {
      stateMod().getCurrentAccountIDs.mockReturnValue(['uuid-missing']);
      await loadRIExchange();
      expect(noticeText()).toBe('');
    });
  });
});

describe('exchange history table counts (issue #286)', () => {
  beforeEach(() => {
    for (const id of ['ri-exchange-instances-list', 'ri-exchange-recommendations-list', 'ri-exchange-history-list']) {
      const el = document.createElement('div');
      el.id = id;
      document.body.appendChild(el);
    }
    const s = jest.requireMock('../state') as { getCurrentProvider: jest.Mock; getCurrentAccountIDs: jest.Mock; getRiExchangeColumnFilters: jest.Mock; getActiveRiColumnFilters: jest.Mock };
    s.getCurrentProvider.mockReturnValue('aws');
    s.getCurrentAccountIDs.mockReturnValue([]);
    s.getRiExchangeColumnFilters.mockReturnValue({});
    s.getActiveRiColumnFilters.mockReturnValue({});
    (api.listConvertibleRIs as jest.Mock).mockResolvedValue([]);
    (api.getRIUtilization as jest.Mock).mockResolvedValue([]);
    (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({ recommendations: [], recs_staleness: '', recs_collected_at: null });
  });

  afterEach(() => {
    document.body.innerHTML = '';
    jest.clearAllMocks();
  });

  it('shows each count once, inside the type cells, with no duplicate Count column', async () => {
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([{
      id: 'h1', account_id: 'a', exchange_id: 'exch-1', region: 'us-east-1', source_ri_ids: ['ri-1'],
      source_instance_type: 'm5.large', source_count: 4, target_offering_id: 'o', target_instance_type: 'm5.4xlarge',
      target_count: 1, payment_due: '1.00', status: 'completed', mode: 'manual',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    }]);
    await loadRIExchange();
    const table = document.querySelector('#ri-exchange-history-list table')!;
    const headers = Array.from(table.querySelectorAll('th')).map((h) => h.textContent);
    expect(headers).not.toContain('Count');
    const cells = Array.from(table.querySelectorAll('tbody tr td')).map((c) => c.textContent);
    expect(cells).toHaveLength(headers.length);
    expect(cells).toContain('4x m5.large');
    expect(cells).toContain('1x m5.4xlarge');
    expect(cells).not.toContain('4');
  });

  // Issue #737: the status cell used to print the raw lowercase value with a
  // page-local color (pending blue, processing orange). It now uses the shared
  // badge tones and display labels.
  it.each([
    ['pending', 'badge-warning', 'Pending'],
    ['processing', 'badge-warning', 'Processing'],
    ['completed', 'badge-success', 'Completed'],
    ['failed', 'badge-danger', 'Failed'],
    ['canceled', 'badge-muted', 'Cancelled'],
  ])('renders status %s as a %s badge reading %s', async (status, cls, label) => {
    (api.getRIExchangeHistory as jest.Mock).mockResolvedValue([{
      id: 'h1', account_id: 'a', exchange_id: 'exch-1', region: 'us-east-1', source_ri_ids: ['ri-1'],
      source_instance_type: 'm5.large', source_count: 1, target_offering_id: 'o', target_instance_type: 'm5.xlarge',
      target_count: 1, payment_due: '1.00', status, mode: 'manual',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    }]);
    await loadRIExchange();
    const badge = document.querySelector('#ri-exchange-history-list tbody .badge')!;
    expect(badge.className).toBe(`badge ${cls}`);
    expect(badge.textContent).toBe(label);
  });
});

// Issue #1181: RIs left out of reshape analysis must be visible in the UI.
describe('excluded RIs notice', () => {
  let listEl: HTMLDivElement;

  beforeEach(() => {
    const wrapper = document.createElement('div');
    listEl = document.createElement('div');
    listEl.id = 'ri-exchange-recommendations-list';
    wrapper.appendChild(listEl);
    document.body.appendChild(wrapper);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    jest.resetAllMocks();
  });

  it('lists the excluded ids as text when excluded_ris is non-empty', async () => {
    (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({
      recommendations: [],
      excluded_ris: [
        { ri_id: 'ri-0', reason: 'no term' },
        { ri_id: '<img src=x onerror=alert(1)>', reason: 'no term' },
      ],
    });

    await loadReshapeRecommendations();

    const notice = document.getElementById('ri-exchange-recommendations-excluded');
    expect(notice?.textContent).toContain('2 convertible RIs were left out because no term');
    expect(notice?.textContent).toContain('ri-0');
    expect(notice?.textContent).toContain('<img src=x onerror=alert(1)>');
    expect(notice?.querySelector('img')).toBeNull();
  });

  it('shows each excluded RI\'s server reason, grouped by reason', () => {
    renderExcludedRIsNotice([
      { ri_id: 'ri-dur', reason: 'non-positive duration' },
      { ri_id: 'ri-price', reason: 'no pricing available' },
    ]);
    const text = document.getElementById('ri-exchange-recommendations-excluded')?.textContent ?? '';
    expect(text).toContain('1 convertible RI was left out because non-positive duration: ri-dur');
    expect(text).toContain('1 convertible RI was left out because no pricing available: ri-price');
    expect(text).not.toContain('no term length');
  });

  it('falls back to the term-length text when reason is empty or missing', () => {
    renderExcludedRIsNotice([
      { ri_id: 'ri-a', reason: '' },
      { ri_id: 'ri-b' } as unknown as { ri_id: string; reason: string },
    ]);
    expect(document.getElementById('ri-exchange-recommendations-excluded')?.textContent).toContain(
      '2 convertible RIs were left out because AWS returned no term length: ri-a, ri-b',
    );
  });

  it('renders a reason containing HTML special characters as text', () => {
    renderExcludedRIsNotice([{ ri_id: 'ri-x', reason: '<b>bad</b> & "quoted"' }]);
    const notice = document.getElementById('ri-exchange-recommendations-excluded');
    expect(notice?.textContent).toContain('<b>bad</b> & "quoted"');
    expect(notice?.querySelector('b')).toBeNull();
  });

  it('shows no notice when excluded_ris is empty or absent, and clears a stale one', () => {
    renderExcludedRIsNotice([{ ri_id: 'ri-0', reason: 'x' }]);
    expect(document.getElementById('ri-exchange-recommendations-excluded')?.textContent).toContain('1 convertible RI was left out');
    renderExcludedRIsNotice(undefined);
    expect(document.getElementById('ri-exchange-recommendations-excluded')?.textContent).toBe('');
  });

  it('does not render a notice for an empty list on load', async () => {
    (api.getReshapeRecommendations as jest.Mock).mockResolvedValue({ recommendations: [], excluded_ris: [] });
    await loadReshapeRecommendations();
    expect(document.getElementById('ri-exchange-recommendations-excluded')?.textContent ?? '').toBe('');
  });
});
