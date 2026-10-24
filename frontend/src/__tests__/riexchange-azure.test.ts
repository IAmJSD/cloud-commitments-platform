/**
 * Azure RI exchange modal and table action (issue #81).
 */
jest.mock('../api', () => ({
  getAzureCompatibleOfferings: jest.fn(),
  executeAzureExchange: jest.fn(),
}));
jest.mock('../permissions', () => ({ canAccess: jest.fn(), isRIExchanger: jest.fn() }));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn() }));
jest.mock('../toast', () => ({ showToast: jest.fn() }));

import * as api from '../api';
import { canAccess, isRIExchanger } from '../permissions';
import { confirmDialog } from '../confirmDialog';
import { attachAzureExchangeToolbar, buildAzureSelectCell, canPreviewAzureExchange } from '../riexchange-azure';
import { openAzureExchangeModal } from '../riexchange-azure-modal';
import type { ExchangeableAzureRI } from '../api/types';

const SUB = '11111111-2222-3333-4444-555555555555';
const reservations: ExchangeableAzureRI[] = [
  { reservation_order_id: 'o1', reservation_id: '/r/one', sku: 'Standard_D2s_v3', quantity: 4, region: 'eastus', term: 'P3Y', instance_flexibility: 'On', display_name: 'web', billing_scope_id: `/subscriptions/${SUB}` },
  { reservation_order_id: 'o2', reservation_id: '/r/two', sku: 'Standard_E2s_v3', quantity: 1, region: 'westeurope', term: 'P1Y', instance_flexibility: 'On', billing_scope_id: `/subscriptions/${SUB}` },
  { reservation_order_id: 'o3', reservation_id: '/r/three', sku: 'Standard_F2s_v2', quantity: 1, region: 'eastus', term: 'P1Y', instance_flexibility: 'On', billing_scope_id: '/subscriptions/other' },
];

const okPreview = {
  offerings: [{ sku: 'Standard_D4s_v5', location: 'eastus', term: 'P3Y', quantity: 2, billing_currency_total: 900.5, currency_code: 'USD' }],
  preview: { session_id: 's1', net_payable: 120.25, net_payable_currency: 'USD', refunds_total: 780.25, purchases_total: 900.5 },
};

const flush = () => new Promise(r => setTimeout(r, 0));
const $ = <T extends Element>(sel: string) => document.querySelector(sel) as T;
const resultText = () => $('#azure-exchange-result').textContent ?? '';
const statusText = () => $('#azure-exchange-status').textContent ?? '';

function setValue(input: HTMLInputElement | HTMLSelectElement, value: string): void {
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
}

function grant(...perms: string[]): void {
  (canAccess as jest.Mock).mockImplementation((action: string, resource: string) => perms.includes(`${action}:${resource}`));
  (isRIExchanger as jest.Mock).mockReturnValue(perms.includes('execute:ri-exchange'));
}

async function openAndPreview(sources = [reservations[0]!]): Promise<void> {
  openAzureExchangeModal(sources, onExecuted);
  setValue($<HTMLInputElement>('.azure-exchange-target-sku'), 'Standard_D4s_v5');
  setValue($<HTMLInputElement>('.azure-exchange-target-qty'), '2');
  $<HTMLButtonElement>('.modal-buttons .btn.primary').click();
  await flush();
}

const onExecuted = jest.fn();
const executeBtn = () => Array.from(document.querySelectorAll<HTMLButtonElement>('.modal-buttons button')).find(b => b.textContent === 'Execute exchange')!;

beforeEach(() => {
  jest.clearAllMocks();
  document.body.innerHTML = '<div id="ri-exchange-modal" class="modal hidden"><div class="modal-content"></div></div>';
  grant('view:purchases', 'execute:ri-exchange');
  (api.getAzureCompatibleOfferings as jest.Mock).mockResolvedValue(okPreview);
  (confirmDialog as jest.Mock).mockResolvedValue(true);
});

describe('Azure exchange table action', () => {
  function mountTable(): HTMLTableElement {
    const container = document.createElement('div');
    const table = document.createElement('table');
    const body = table.createTBody();
    reservations.forEach((r, i) => body.insertRow().appendChild(buildAzureSelectCell(r, i)));
    container.appendChild(table);
    document.body.appendChild(container);
    attachAzureExchangeToolbar(container, table, reservations, onExecuted);
    return table;
  }

  it('is offered to users who can preview purchases', () => {
    expect(canPreviewAzureExchange()).toBe(true);
    grant();
    expect(canPreviewAzureExchange()).toBe(false);
  });

  it('enables "Exchange selected" once rows are ticked and opens the modal for them', async () => {
    const table = mountTable();
    const btn = $<HTMLButtonElement>('#azure-exchange-selected-btn');
    expect(btn.disabled).toBe(true);
    const boxes = table.querySelectorAll<HTMLInputElement>('input.azure-ri-select');
    boxes[0]!.checked = true;
    boxes[1]!.checked = true;
    table.dispatchEvent(new Event('change'));
    expect(btn.textContent).toBe('Exchange selected (2)');
    btn.click();
    // The modal module is loaded on demand.
    for (let i = 0; i < 5 && $('#ri-exchange-modal').classList.contains('hidden'); i++) await flush();
    expect($('#ri-exchange-modal').classList.contains('hidden')).toBe(false);
    expect(document.querySelectorAll('.azure-exchange-source-row')).toHaveLength(2);
    expect($('.azure-exchange').textContent).toContain(`Subscription ${SUB}`);
  });

  it('shows a toast when the exchange dialog fails to load', async () => {
    let toast: jest.Mock | undefined;
    jest.isolateModules(() => {
      jest.doMock('../riexchange-azure-modal', () => { throw new Error('ChunkLoadError'); });
      toast = (jest.requireMock('../toast') as { showToast: jest.Mock }).showToast;
      const isolated = jest.requireActual('../riexchange-azure') as typeof import('../riexchange-azure');
      const container = document.createElement('div');
      const table = document.createElement('table');
      table.createTBody().insertRow().appendChild(isolated.buildAzureSelectCell(reservations[0]!, 0));
      container.appendChild(table);
      document.body.appendChild(container);
      isolated.attachAzureExchangeToolbar(container, table, reservations, onExecuted);
      table.querySelector<HTMLInputElement>('input.azure-ri-select')!.checked = true;
      table.dispatchEvent(new Event('change'));
    });
    $<HTMLButtonElement>('#azure-exchange-selected-btn').click();
    for (let i = 0; i < 5 && toast!.mock.calls.length === 0; i++) await flush();
    jest.dontMock('../riexchange-azure-modal');
    expect(toast).toHaveBeenCalledWith({ message: 'Could not load the exchange dialog. Reload the page and try again.', kind: 'error' });
  });
});

describe('openAzureExchangeModal', () => {
  it('refuses reservations billed to different subscriptions', () => {
    openAzureExchangeModal([reservations[0]!, reservations[2]!], onExecuted);
    expect(resultText()).toMatch(/billed to different subscriptions/);
    expect(document.querySelector('.azure-exchange-target-row')).toBeNull();
  });

  it('seeds the target from the source and previews the priced exchange', async () => {
    openAzureExchangeModal([reservations[0]!], onExecuted);
    expect($<HTMLInputElement>('.azure-exchange-target-location').value).toBe('eastus');
    expect($<HTMLSelectElement>('.azure-exchange-target-term').value).toBe('P3Y');
    setValue($<HTMLInputElement>('.azure-exchange-source-qty'), '3');
    setValue($<HTMLInputElement>('.azure-exchange-target-sku'), 'Standard_D4s_v5');
    $<HTMLButtonElement>('.modal-buttons .btn.primary').click();
    await flush();
    expect(api.getAzureCompatibleOfferings).toHaveBeenCalledWith({
      subscription_id: SUB,
      sources: [{ reservation_id: '/r/one', quantity: 3 }],
      targets: [{ sku: 'Standard_D4s_v5', location: 'eastus', term: 'P3Y', quantity: 1 }],
    });
    const text = resultText();
    expect(text).toContain('Net payable:USD 120.25');
    expect(text).toContain('Refunds total:USD 780.25');
    expect(text).toContain('Purchases total:USD 900.50');
    expect(text).toContain('Standard_D4s_v5');
    expect(executeBtn().classList.contains('hidden')).toBe(false);
    // Guardrails are visible but never prefilled from the quote.
    expect($<HTMLInputElement>('#azure-exchange-max-payment').value).toBe('');
    expect($<HTMLInputElement>('#azure-exchange-currency').value).toBe('');
  });

  it('validates the form before calling the API', async () => {
    openAzureExchangeModal([reservations[0]!], onExecuted);
    setValue($<HTMLInputElement>('.azure-exchange-source-qty'), '9');
    $<HTMLButtonElement>('.modal-buttons .btn.primary').click();
    await flush();
    expect(api.getAzureCompatibleOfferings).not.toHaveBeenCalled();
    expect(resultText()).toMatch(/Source 1: quantity must be a whole number from 1 to 4/);
  });

  it.each([
    ['policy errors', { ...okPreview, preview: { ...okPreview.preview, policy_errors: ['Reservation expired', 'Term not allowed'] } }, /Reservation expired.*Term not allowed/s],
    ['no net payable', { ...okPreview, preview: { ...okPreview.preview, net_payable: null } }, /did not report a net payable/],
    ['no preview', { offerings: null, preview: null }, /no preview/],
  ])('does not offer Execute when Azure returns %s', async (_name, resp, msg) => {
    (api.getAzureCompatibleOfferings as jest.Mock).mockResolvedValue(resp);
    await openAndPreview();
    expect(resultText()).toMatch(msg);
    expect(executeBtn().classList.contains('hidden')).toBe(true);
  });

  it('shows a preview failure as a user-facing message', async () => {
    (api.getAzureCompatibleOfferings as jest.Mock).mockRejectedValue(Object.assign(new Error('sources[0].reservation_id is not a reservation billed to subscription'), { status: 403 }));
    await openAndPreview();
    expect(resultText()).toMatch(/^Could not preview the exchange: you are not allowed to do this\./);
  });

  it('discards the preview when a target changes', async () => {
    await openAndPreview();
    expect(executeBtn().classList.contains('hidden')).toBe(false);
    setValue($<HTMLInputElement>('.azure-exchange-target-location'), 'westus2');
    expect(executeBtn().classList.contains('hidden')).toBe(true);
    expect(resultText()).toBe('');
  });

  it('requires the spending limit and currency before executing', async () => {
    await openAndPreview();
    executeBtn().click();
    await flush();
    expect(confirmDialog).not.toHaveBeenCalled();
    expect(api.executeAzureExchange).not.toHaveBeenCalled();
    expect($('.azure-exchange-guard-error').textContent).toMatch(/maximum you are willing to pay/);
  });

  it('executes with the explicit cap and currency after confirmation', async () => {
    (api.executeAzureExchange as jest.Mock).mockResolvedValue({ session_id: 's2', status: 'Succeeded', net_payable: 118, net_payable_currency: 'USD', refunds_total: 780.25, purchases_total: 898.25 });
    await openAndPreview();
    setValue($<HTMLInputElement>('#azure-exchange-max-payment'), '150');
    setValue($<HTMLInputElement>('#azure-exchange-currency'), 'usd');
    expect(executeBtn().classList.contains('hidden')).toBe(false);
    executeBtn().click();
    await flush();
    await flush();
    const confirmBody = (confirmDialog as jest.Mock).mock.calls[0]![0].body as HTMLElement;
    expect(confirmBody.textContent).toContain('Refused if Azure\'s fresh price is above USD 150');
    expect(api.executeAzureExchange).toHaveBeenCalledWith({
      subscription_id: SUB,
      sources: [{ reservation_id: '/r/one', quantity: 4 }],
      targets: [{ sku: 'Standard_D4s_v5', location: 'eastus', term: 'P3Y', quantity: 2 }],
      max_payment_due: '150',
      currency: 'USD',
    });
    expect(onExecuted).toHaveBeenCalled();
    expect(statusText()).toMatch(/^Exchange Succeeded\. Net payable USD 118\.00/);
    // The preview the user acted on stays on screen.
    expect(resultText()).toContain('Net payable:USD 120.25');
  });

  it('does nothing when the confirmation is cancelled', async () => {
    (confirmDialog as jest.Mock).mockResolvedValue(false);
    await openAndPreview();
    setValue($<HTMLInputElement>('#azure-exchange-max-payment'), '150');
    setValue($<HTMLInputElement>('#azure-exchange-currency'), 'USD');
    executeBtn().click();
    await flush();
    expect(api.executeAzureExchange).not.toHaveBeenCalled();
  });

  it('explains a 422 guardrail refusal', async () => {
    (api.executeAzureExchange as jest.Mock).mockRejectedValue(Object.assign(new Error('quoted net payable 160.00 USD exceeds max_payment_due 150.00 USD'), { status: 422 }));
    await openAndPreview();
    setValue($<HTMLInputElement>('#azure-exchange-max-payment'), '150');
    setValue($<HTMLInputElement>('#azure-exchange-currency'), 'USD');
    executeBtn().click();
    await flush();
    await flush();
    expect(statusText()).toBe('Exchange blocked by a safety check: quoted net payable 160.00 USD exceeds max_payment_due 150.00 USD. Raise the maximum payment or change the targets, then preview again.');
    expect(onExecuted).not.toHaveBeenCalled();
  });

  it.each([
    ['a 504 gateway timeout', Object.assign(new Error('HTTP 504'), { status: 504 }), 'HTTP 504'],
    ['a client timeout', Object.assign(new Error('Request to /ri-exchange/azure-instances/exchange timed out after 90000ms'), { status: 0 }),
      'Request to /ri-exchange/azure-instances/exchange timed out after 90000ms'],
  ])('treats %s on execute as an unknown outcome: refreshes and requires a new preview', async (_label, error, detail) => {
    (api.executeAzureExchange as jest.Mock).mockRejectedValue(error);
    await openAndPreview();
    setValue($<HTMLInputElement>('#azure-exchange-max-payment'), '150');
    setValue($<HTMLInputElement>('#azure-exchange-currency'), 'USD');
    executeBtn().click();
    await flush();
    await flush();
    expect(statusText()).toBe(`The exchange may have been submitted to Azure. Check the RI exchange history (or the Azure portal) before trying again. Details: ${detail}`);
    expect(onExecuted).toHaveBeenCalledTimes(1);
    expect(executeBtn().classList.contains('hidden')).toBe(true);
    expect($('.azure-exchange-guardrails').classList.contains('hidden')).toBe(true);
    // A stray click on the now-hidden button cannot resubmit the old preview.
    executeBtn().click();
    await flush();
    expect(api.executeAzureExchange).toHaveBeenCalledTimes(1);
  });

  it('previews but hides Execute without execute:ri-exchange', async () => {
    grant('view:purchases');
    await openAndPreview();
    expect(resultText()).toContain('Net payable');
    expect(executeBtn().classList.contains('hidden')).toBe(true);
    expect($('.azure-exchange-guardrails').textContent).toContain('needs the execute:ri-exchange permission');
  });

  it('adds and removes target rows', () => {
    openAzureExchangeModal([reservations[0]!], onExecuted);
    const add = Array.from(document.querySelectorAll<HTMLButtonElement>('button')).find(b => b.textContent === '+ Add target')!;
    add.click();
    expect(document.querySelectorAll('.azure-exchange-target-row')).toHaveLength(2);
    document.querySelector<HTMLButtonElement>('.exchange-remove-target')!.click();
    expect(document.querySelectorAll('.azure-exchange-target-row')).toHaveLength(1);
    document.querySelector<HTMLButtonElement>('.exchange-remove-target')!.click();
    expect(document.querySelectorAll('.azure-exchange-target-row')).toHaveLength(1);
  });
});
