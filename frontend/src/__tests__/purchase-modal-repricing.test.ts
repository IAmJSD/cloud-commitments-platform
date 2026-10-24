/**
 * Issue #1903: the single-bucket purchase modal's Term and Payment selects
 * must re-price the row (not just relabel it), and a pending or cancelled
 * submission must not submit twice.
 *
 * These tests drive app.ts and recommendations.ts and assert that the
 * displayed variant matches the body passed to api.executePurchase.
 * The backend independently resolves identity and pricing from stored
 * recommendations (internal/api/purchase_pricing.go). Shared mocks,
 * fixtures and the per-test reset live in purchase-modal-harness.ts.
 */

// The harness registers the module mocks, so it must be imported first.
import {
  buildRows,
  deferred,
  flush,
  setupPurchaseModalTest,
} from './purchase-modal-harness';
import * as api from '../api';
import * as state from '../state';
import { showToast } from '../toast';
import { confirmDialog } from '../confirmDialog';
import {
  openPurchaseModal,
  getPurchaseModalRecommendations,
  clearPurchaseModalRecommendations,
  loadRecommendations,
} from '../recommendations';
import { formatCurrency } from '../utils';
import type { LocalRecommendation } from '../types';

beforeEach(setupPurchaseModalTest);

describe('Issue #1903: purchase modal re-prices on Term/Payment change', () => {
  test('T1 term change re-prices the submitted body', async () => {
    const rows = buildRows();
    const v3all = rows.find((r) => r.id === 'v-3-all')!;

    await openPurchaseModal([v3all]);

    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));

    const tr = document.querySelector<HTMLTableRowElement>('.purchase-modal-table tbody tr')!;
    expect(tr.cells[5]!.textContent).toBe(formatCurrency(12000));
    expect(tr.cells[6]!.textContent).toBe(formatCurrency(0));
    expect(tr.cells[7]!.textContent).toBe(formatCurrency(700));
    expect(document.getElementById('purchase-modal-total-upfront')?.textContent).toContain(formatCurrency(12000));

    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    expect(api.executePurchase).toHaveBeenCalledTimes(1);
    const body = (api.executePurchase as jest.Mock).mock.calls[0]![0] as Array<Record<string, unknown>>;
    expect(body[0]).toMatchObject({
      id: 'v-1-all', term: 1, payment: 'all-upfront', upfront_cost: 12000, monthly_cost: 0, savings: 700,
    });
  });

  test('T2 payment change re-prices the submitted body', async () => {
    const rows = buildRows();
    const v1all = rows.find((r) => r.id === 'v-1-all')!;

    await openPurchaseModal([v1all]);

    const paymentSelect = document.querySelector<HTMLSelectElement>('.purchase-row-payment')!;
    paymentSelect.value = 'no-upfront';
    paymentSelect.dispatchEvent(new Event('change'));

    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    expect(api.executePurchase).toHaveBeenCalledTimes(1);
    const body = (api.executePurchase as jest.Mock).mock.calls[0]![0] as Array<Record<string, unknown>>;
    expect(body[0]).toMatchObject({
      id: 'v-1-no', term: 1, payment: 'no-upfront', upfront_cost: 0, monthly_cost: 800, count: 1,
    });
  });

  test('T3 options are only priced variants', async () => {
    const rows = buildRows();
    const v3all = rows.find((r) => r.id === 'v-3-all')!;

    await openPurchaseModal([v3all]);

    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    const paymentSelect = document.querySelector<HTMLSelectElement>('.purchase-row-payment')!;
    expect(Array.from(termSelect.options).map((o) => o.value)).toEqual(['1', '3']);
    expect(Array.from(paymentSelect.options).map((o) => o.value)).toEqual(['all-upfront', 'partial-upfront']);

    // Azure cell with a single loaded variant.
    const azureRec: LocalRecommendation = {
      id: 'az-1', provider: 'azure', cloud_account_id: 'a2', service: 'compute',
      region: 'eastus', resource_type: 'Standard_D2s_v3', count: 1, term: 3,
      payment: 'upfront', upfront_cost: 500, savings: 100,
    };
    (state.getRecommendations as jest.Mock).mockReturnValue([azureRec]);
    clearPurchaseModalRecommendations();

    await openPurchaseModal([azureRec]);

    const termSelect2 = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    const paymentSelect2 = document.querySelector<HTMLSelectElement>('.purchase-row-payment')!;
    expect(Array.from(termSelect2.options).map((o) => o.value)).toEqual(['3']);
    expect(Array.from(paymentSelect2.options).map((o) => o.value)).toEqual(['all-upfront']);
  });

  test('T3 capacity-aware payment options choose the viable alternate on term change', async () => {
    const details = { platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' };
    const rows = buildRows().filter((row) => row.id !== 'v-3-partial').map((row) => {
      if (row.id === 'v-1-all') return { ...row, details, count: 1, upfront_cost: 6000 };
      if (row.id === 'v-1-no') return {
        ...row, count: 2, recommended_count: 2, monthly_cost: 1600,
        details,
      };
      return { ...row, details };
    });
    (localStorage.getItem as jest.Mock).mockReturnValue(JSON.stringify({ capacity: 50 }));
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['v-3-all']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));

    const row = document.querySelector<HTMLTableRowElement>('.purchase-modal-table tbody tr')!;
    expect(row.cells[4]!.textContent).toBe('1');
    expect(row.cells[5]!.textContent).toBe(formatCurrency(0));
    expect(row.cells[6]!.textContent).toBe(formatCurrency(800));
    expect(document.querySelector<HTMLSelectElement>('.purchase-row-payment')!.value).toBe('no-upfront');
    expect(Array.from(document.querySelector<HTMLSelectElement>('.purchase-row-payment')!.options).map((o) => o.value))
      .toEqual(['no-upfront']);
    expect(getPurchaseModalRecommendations()[0]).toMatchObject({
      id: 'v-1-no', term: 1, payment: 'no-upfront', count: 1, recommended_count: 2,
      upfront_cost: 0, monthly_cost: 800,
    });
    expect(document.getElementById('purchase-modal-total-upfront')?.textContent).toContain(formatCurrency(0));
    (document.getElementById('execute-mode-direct') as HTMLInputElement).click();
    expect(document.querySelector('.direct-execute-warning')?.textContent).toContain(formatCurrency(0));

    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();
    expect(api.executePurchase).toHaveBeenCalledWith(
      [expect.objectContaining({
        id: 'v-1-no', term: 1, payment: 'no-upfront', count: 1, recommended_count: 2,
        details: { platform: 'Linux/UNIX', tenancy: 'default', scope: 'Region' },
      })],
      50,
      'direct',
    );
  });

  test('T3 viable payment survives term swaps and starts from loaded count', async () => {
    const rows = [
      { ...buildRows()[0]!, id: 'v-3-no', payment: 'no-upfront' as const, count: 2, monthly_cost: 1600 },
      { ...buildRows()[2]!, count: 2 },
      { ...buildRows()[3]!, count: 2, recommended_count: 2, monthly_cost: 1600 },
    ];
    (localStorage.getItem as jest.Mock).mockReturnValue(JSON.stringify({ capacity: 50 }));
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['v-3-no']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));
    expect(getPurchaseModalRecommendations()[0]).toMatchObject({ id: 'v-1-no', payment: 'no-upfront', count: 1 });
    expect(Array.from(document.querySelector<HTMLSelectElement>('.purchase-row-payment')!.options).map((o) => o.value))
      .toEqual(['all-upfront', 'no-upfront']);

    const termSelectAgain = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelectAgain.value = '3';
    termSelectAgain.dispatchEvent(new Event('change'));
    expect(getPurchaseModalRecommendations()[0]).toMatchObject({ id: 'v-3-no', payment: 'no-upfront', count: 1 });
    const row = document.querySelector<HTMLTableRowElement>('.purchase-modal-table tbody tr')!;
    expect(row.cells[6]!.textContent).toBe(formatCurrency(800));
    expect(row.querySelector<HTMLSelectElement>('.purchase-row-term')!.value).toBe('3');
    expect(row.querySelector<HTMLSelectElement>('.purchase-row-payment')!.value).toBe('no-upfront');
  });

  test('T3 all-zero term restores the prior priced row', async () => {
    const rows = [
      { ...buildRows()[0]!, count: 2 },
      { ...buildRows()[2]!, count: 1, upfront_cost: 6000 },
      { ...buildRows()[3]!, count: 1, recommended_count: 1 },
    ];
    (localStorage.getItem as jest.Mock).mockReturnValue(JSON.stringify({ capacity: 50 }));
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['v-3-all']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const before = getPurchaseModalRecommendations()[0]!;
    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));

    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'warning' }));
    expect(termSelect.value).toBe('3');
    expect(getPurchaseModalRecommendations()[0]).toEqual(before);
    expect(document.querySelector<HTMLSelectElement>('.purchase-row-payment')!.value).toBe('all-upfront');
  });

  test('T4 capacity scaling survives a swap (bulk path)', async () => {
    const rows = buildRows();
    (localStorage.getItem as jest.Mock).mockReturnValue(JSON.stringify({ capacity: 50 }));
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['v-3-all']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const tr = document.querySelector<HTMLTableRowElement>('.purchase-modal-table tbody tr')!;
    expect(tr.cells[4]!.textContent).toBe('1');
    expect(tr.cells[5]!.textContent).toBe(formatCurrency(18000));

    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));

    const tr2 = document.querySelector<HTMLTableRowElement>('.purchase-modal-table tbody tr')!;
    expect(tr2.cells[5]!.textContent).toBe(formatCurrency(6000));

    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    expect(api.executePurchase).toHaveBeenCalledWith(
      expect.arrayContaining([expect.objectContaining({ id: 'v-1-all', count: 1, recommended_count: 2, upfront_cost: 6000 })]),
      50,
      undefined,
    );
  });

  test('T5 zero-unit variant is refused and the row keeps its price', async () => {
    const rows = buildRows();
    (localStorage.getItem as jest.Mock).mockReturnValue(JSON.stringify({ capacity: 50 }));
    (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: rows, regions: [] });
    (state.getRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getVisibleRecommendations as jest.Mock).mockReturnValue(rows);
    (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(['v-3-all']));

    await loadRecommendations();
    (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));

    // The term-change re-render replaced the row — re-query the fresh Payment select.
    const paymentSelect = document.querySelector<HTMLSelectElement>('.purchase-row-payment')!;
    paymentSelect.value = 'no-upfront';
    paymentSelect.dispatchEvent(new Event('change'));

    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'warning' }));
    expect(paymentSelect.value).toBe('all-upfront');

    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    const body = (api.executePurchase as jest.Mock).mock.calls[0]![0] as Array<Record<string, unknown>>;
    expect(body[0]).toMatchObject({ id: 'v-1-all', upfront_cost: 6000 });
  });

  test('T6 account override re-prices at open', async () => {
    const rows = buildRows();
    const v3all = rows.find((r) => r.id === 'v-3-all')!;

    (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([
      { id: 'ovr-1', account_id: 'a1', provider: 'aws', service: 'ec2', payment: 'partial-upfront' },
    ]);
    await openPurchaseModal([v3all]);

    const live = getPurchaseModalRecommendations();
    expect(live[0]).toMatchObject({ id: 'v-3-partial', payment: 'partial-upfront', upfront_cost: 18000 });
    expect(document.querySelector('.purchase-row-payment-source')).not.toBeNull();
    expect(document.querySelector<HTMLTableRowElement>('.purchase-modal-table tbody tr')!.cells[5]!.textContent)
      .toBe(formatCurrency(18000));

    clearPurchaseModalRecommendations();
    (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([
      { id: 'ovr-2', account_id: 'a1', provider: 'aws', service: 'ec2', payment: 'no-upfront' },
    ]);
    await openPurchaseModal([v3all]);

    const live2 = getPurchaseModalRecommendations();
    expect(live2[0]).toMatchObject({ id: 'v-3-all', payment: 'all-upfront', upfront_cost: 36000 });
    expect(document.querySelector('.purchase-row-payment-source')).toBeNull();
  });

  test('T7 direct-execute warning follows a term change', async () => {
    const rows = buildRows();
    const v3all = rows.find((r) => r.id === 'v-3-all')!;

    await openPurchaseModal([v3all]);

    const directRadio = document.getElementById('execute-mode-direct') as HTMLInputElement;
    expect(directRadio).not.toBeNull();
    directRadio.click();
    directRadio.dispatchEvent(new Event('change', { bubbles: true }));

    expect(document.querySelector('.direct-execute-warning')?.textContent).toContain('36,000.00');

    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));

    expect(document.querySelector('.direct-execute-warning')?.textContent).toContain('12,000.00');
  });

  test('busy single purchase stays disabled while its request is pending', async () => {
    const request = deferred<Awaited<ReturnType<typeof api.executePurchase>>>();
    (api.executePurchase as jest.Mock).mockReturnValue(request.promise);
    const rows = buildRows();
    await openPurchaseModal([rows[0]!]);

    const executeBtn = document.getElementById('execute-purchase-btn') as HTMLButtonElement;
    executeBtn.click();
    await flush();
    expect(api.executePurchase).toHaveBeenCalledTimes(1);

    const termSelect = document.querySelector<HTMLSelectElement>('.purchase-row-term')!;
    termSelect.value = '1';
    termSelect.dispatchEvent(new Event('change'));
    const include = document.querySelector<HTMLInputElement>('.purchase-modal-row-include')!;
    include.checked = false;
    include.dispatchEvent(new Event('change'));
    include.checked = true;
    include.dispatchEvent(new Event('change'));

    expect(executeBtn.disabled).toBe(true);
    executeBtn.click();
    await flush();
    expect(api.executePurchase).toHaveBeenCalledTimes(1);

    request.resolve({
      execution_id: 'exec-single',
      status: 'pending',
      email_sent: true,
      approval_recipient: 'approver@example.com',
    });
    await flush();
    expect(executeBtn.dataset['submitting']).toBeUndefined();
  });

  test('confirmation cancel restores normal single-purchase submission', async () => {
    (confirmDialog as jest.Mock)
      .mockResolvedValueOnce(false)
      .mockResolvedValueOnce(true);
    const rows = buildRows();
    await openPurchaseModal([rows[0]!]);

    const executeBtn = document.getElementById('execute-purchase-btn') as HTMLButtonElement;
    executeBtn.click();
    await flush();

    expect(api.executePurchase).not.toHaveBeenCalled();
    expect(executeBtn.dataset['submitting']).toBeUndefined();
    expect(executeBtn.disabled).toBe(false);

    executeBtn.click();
    await flush();
    expect(api.executePurchase).toHaveBeenCalledTimes(1);
  });
});
