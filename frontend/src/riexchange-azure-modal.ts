/**
 * Azure RI exchange modal (issue #81), loaded on demand from
 * riexchange-azure.ts so it stays out of the main bundle until used: pick quantities from the selected
 * source reservations, describe the target SKU / region / term / quantity,
 * preview the priced exchange (compatible offerings, net payable, refunds,
 * purchases, policy errors), then execute behind an explicit
 * max_payment_due + currency guardrail.
 *
 * Every Azure-derived string goes through textContent, never innerHTML.
 */

import * as api from './api';
import type { AzureCompatibleOfferingsResponse, AzureExecuteExchangeRequest, ExchangeableAzureRI } from './api';
import { openModal, closeModal, createModalHeader } from './modal';
import { confirmDialog } from './confirmDialog';
import { showToast } from './toast';
import { isRIExchanger } from './permissions';
import {
  AZURE_EXCHANGE_TERMS, MAX_AZURE_EXCHANGE_ITEMS, azureTermLabel, describeAzureExchangeError, formatAzureAmount,
  resolveExchangeSubscription, validateAzureExchangeForm, validateAzureGuardrails,
  isAzureExchangeOutcomeUnknown, type AzureExchangeSourceInput, type AzureExchangeTargetInput,
} from './lib/azure-exchange';

function el<K extends keyof HTMLElementTagNameMap>(tag: K, className?: string, text?: string): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function labeled(label: string, control: HTMLElement): HTMLLabelElement {
  const l = el('label', undefined, `${label} `);
  l.appendChild(control);
  return l;
}

function numberInput(className: string, value: number, max?: number): HTMLInputElement {
  const input = el('input', className);
  input.type = 'number';
  input.min = '1';
  if (max !== undefined) input.max = String(max);
  input.step = '1';
  input.value = String(value);
  return input;
}

function textInput(className: string, value: string, placeholder: string): HTMLInputElement {
  const input = el('input', className);
  input.type = 'text';
  input.value = value;
  input.placeholder = placeholder;
  return input;
}

function setMessage(container: HTMLElement, message: string, cls: string): void {
  container.textContent = '';
  container.appendChild(el('p', cls, message));
}

interface TargetRow {
  row: HTMLElement;
  sku: HTMLInputElement;
  location: HTMLInputElement;
  term: HTMLSelectElement;
  quantity: HTMLInputElement;
}

function renderPreview(container: HTMLElement, resp: AzureCompatibleOfferingsResponse): void {
  container.textContent = '';
  const preview = resp.preview;
  const policyErrors = preview?.policy_errors ?? [];
  const blocked = !preview || policyErrors.length > 0 || preview.net_payable === null;
  const box = el('div', `quote-summary azure-exchange-preview ${blocked ? 'quote-invalid' : 'quote-valid'}`);
  box.appendChild(el('h4', undefined, blocked ? 'Azure will not accept this exchange as it is' : 'Exchange preview'));
  if (!preview) {
    box.appendChild(el('p', 'error', 'Azure returned no preview for this exchange.'));
  } else {
    const currency = preview.net_payable_currency;
    const details = el('div', 'quote-details');
    for (const [label, value] of [
      ['Net payable', formatAzureAmount(preview.net_payable, currency)],
      ['Refunds total', formatAzureAmount(preview.refunds_total, currency)],
      ['Purchases total', formatAzureAmount(preview.purchases_total, currency)],
    ] as const) {
      const row = el('div', 'quote-row');
      row.append(el('span', undefined, `${label}:`), el('strong', undefined, value));
      details.appendChild(row);
    }
    box.appendChild(details);
    if (preview.net_payable !== null && preview.net_payable < 0) {
      box.appendChild(el('p', 'help-text', 'A negative net payable is a refund to you.'));
    }
    if (preview.net_payable === null) {
      box.appendChild(el('p', 'error', 'Azure did not report a net payable amount, so this exchange cannot be executed.'));
    }
    if (policyErrors.length > 0) {
      box.appendChild(el('strong', undefined, 'Azure policy errors:'));
      const ul = el('ul', 'azure-exchange-policy-errors');
      for (const msg of policyErrors) ul.appendChild(el('li', undefined, msg));
      box.appendChild(ul);
    }
  }
  const offerings = resp.offerings ?? [];
  if (offerings.length > 0) {
    const table = el('table', 'azure-exchange-offerings');
    const head = table.createTHead().insertRow();
    for (const h of ['SKU', 'Region', 'Term', 'Quantity', 'Total']) head.appendChild(el('th', undefined, h));
    const body = table.createTBody();
    for (const o of offerings) {
      const tr = body.insertRow();
      for (const v of [o.sku, o.location, azureTermLabel(o.term), String(o.quantity), formatAzureAmount(o.billing_currency_total, o.currency_code)]) {
        tr.appendChild(el('td', undefined, v));
      }
    }
    box.appendChild(el('h5', undefined, 'Compatible offerings'));
    box.appendChild(table);
  }
  container.appendChild(box);
}

/**
 * Opens the exchange modal for the selected reservations. onExecuted runs
 * after an exchange that succeeded or may have, so the caller can reload the
 * list and history.
 */
export function openAzureExchangeModal(sources: readonly ExchangeableAzureRI[], onExecuted: () => void): void {
  const modal = document.getElementById('ri-exchange-modal');
  const content = modal?.querySelector('.modal-content');
  if (!modal || !content) return;

  let sessionOpen = true;
  let revision = 0;
  let lastPreview: AzureCompatibleOfferingsResponse | null = null;
  let lastRequest: { subscription_id: string; sources: AzureExchangeSourceInput[]; targets: AzureExchangeTargetInput[] } | null = null;

  // A fresh root per open: the modal element is shared with the AWS flow, so
  // listeners must not be attached to the long-lived .modal-content.
  content.textContent = '';
  const root = el('div', 'azure-exchange');
  content.appendChild(root);
  // The header sits beside the root (the dialog chrome keys off a direct
  // child of .modal-content). "Close dialog", not "Close": the error state's
  // only action is a Close button.
  content.insertBefore(createModalHeader(el('h3', undefined, 'Azure Reservation Exchange'), { closeLabel: 'Close dialog' }), root);
  const resultBox = el('div', 'azure-exchange-result');
  resultBox.id = 'azure-exchange-result';

  const subscription = resolveExchangeSubscription(sources);
  if ('error' in subscription) {
    setMessage(resultBox, subscription.error, 'error');
    const close = el('button', 'btn', 'Close');
    close.type = 'button';
    close.addEventListener('click', () => closeModal(modal));
    const btns = el('div', 'modal-buttons');
    btns.appendChild(close);
    root.append(resultBox, btns);
    openModal(modal);
    return;
  }
  const subscriptionId = subscription.subscriptionId;
  root.appendChild(el('p', 'help-text', `Subscription ${subscriptionId}. Azure prices the exchange first; nothing changes until you execute it.`));

  // Sources: how many of each selected reservation to hand back.
  root.appendChild(el('h4', undefined, 'Return'));
  const sourceQty: HTMLInputElement[] = [];
  for (const r of sources) {
    const row = el('div', 'setting-row azure-exchange-source-row');
    row.appendChild(el('span', undefined, `${r.display_name || r.reservation_id}: ${r.sku}, ${r.region || 'region not reported'}, ${azureTermLabel(r.term)}`));
    const qty = numberInput('azure-exchange-source-qty', r.quantity, r.quantity);
    row.appendChild(labeled('Quantity', qty));
    row.appendChild(el('span', 'help-text', `of ${r.quantity}`));
    sourceQty.push(qty);
    root.appendChild(row);
  }

  // Targets: what to buy instead, seeded from the first source.
  root.appendChild(el('h4', undefined, 'Buy instead'));
  const targetsBox = el('div', 'azure-exchange-targets');
  root.appendChild(targetsBox);
  const targetRows: TargetRow[] = [];
  const first = sources[0]!;
  const addTarget = (): void => {
    if (targetRows.length >= MAX_AZURE_EXCHANGE_ITEMS) return;
    const row = el('div', 'setting-row azure-exchange-target-row');
    const sku = textInput('azure-exchange-target-sku', first.sku, 'Standard_D4s_v5');
    const location = textInput('azure-exchange-target-location', first.region ?? '', 'eastus');
    const term = el('select', 'azure-exchange-target-term');
    for (const t of AZURE_EXCHANGE_TERMS) {
      const opt = el('option', undefined, azureTermLabel(t));
      opt.value = t;
      term.appendChild(opt);
    }
    term.value = first.term && AZURE_EXCHANGE_TERMS.includes(first.term) ? first.term : 'P1Y';
    const quantity = numberInput('azure-exchange-target-qty', 1);
    const remove = el('button', 'btn exchange-remove-target', '×');
    remove.type = 'button';
    remove.setAttribute('aria-label', 'Remove target');
    const entry: TargetRow = { row, sku, location, term, quantity };
    remove.addEventListener('click', () => {
      if (targetRows.length <= 1) return;
      targetRows.splice(targetRows.indexOf(entry), 1);
      row.remove();
      invalidate();
    });
    row.append(labeled('VM size', sku), labeled('Region', location), labeled('Term', term), labeled('Quantity', quantity), remove);
    targetsBox.appendChild(row);
    targetRows.push(entry);
  };
  addTarget();
  const addBtn = el('button', 'btn', '+ Add target');
  addBtn.type = 'button';
  addBtn.addEventListener('click', () => { addTarget(); invalidate(); });
  const addRow = el('div', 'setting-row');
  addRow.appendChild(addBtn);
  root.appendChild(addRow);

  root.appendChild(resultBox);

  // Guardrails: required before execute, never prefilled from the quote.
  const guard = el('fieldset', 'azure-exchange-guardrails hidden');
  guard.appendChild(el('legend', undefined, 'Spending limit (required to execute)'));
  const maxInput = textInput('azure-exchange-max-payment', '', 'e.g. 1250.00');
  maxInput.id = 'azure-exchange-max-payment';
  maxInput.inputMode = 'decimal';
  const currencyInput = textInput('azure-exchange-currency', '', 'e.g. USD');
  currencyInput.id = 'azure-exchange-currency';
  currencyInput.maxLength = 3;
  guard.append(
    el('p', 'help-text', 'Azure prices the exchange again right before executing it. It is refused if that price is above this limit or in another currency.'),
    labeled('Maximum payment due', maxInput),
    labeled('Currency', currencyInput),
  );
  const canExecute = isRIExchanger();
  if (!canExecute) guard.appendChild(el('p', 'help-text', 'Executing an exchange needs the execute:ri-exchange permission (RI Exchanger group).'));
  root.appendChild(guard);
  // Execute progress and outcome go below the guardrails so the preview the
  // user is acting on stays visible.
  const statusBox = el('div', 'azure-exchange-status');
  statusBox.id = 'azure-exchange-status';
  root.appendChild(statusBox);

  const btns = el('div', 'modal-buttons');
  const previewBtn = el('button', 'btn primary', 'Preview exchange');
  previewBtn.type = 'button';
  const executeBtn = el('button', 'btn primary hidden', 'Execute exchange');
  executeBtn.type = 'button';
  const cancelBtn = el('button', 'btn', 'Cancel');
  cancelBtn.type = 'button';
  btns.append(previewBtn, executeBtn, cancelBtn);
  root.appendChild(btns);

  function readForm(): { sources: AzureExchangeSourceInput[]; targets: AzureExchangeTargetInput[] } {
    return {
      sources: sources.map((r, i) => ({ reservation_id: r.reservation_id, quantity: Number(sourceQty[i]!.value) })),
      targets: targetRows.map(t => ({
        sku: t.sku.value.trim(), location: t.location.value.trim(), term: t.term.value, quantity: Number(t.quantity.value),
      })),
    };
  }

  function invalidate(): void {
    revision++;
    lastPreview = null;
    lastRequest = null;
    executeBtn.classList.add('hidden');
    guard.classList.add('hidden');
    resultBox.textContent = '';
    statusBox.textContent = '';
  }
  // Any change to sources or targets discards the preview, so Execute can
  // never send something other than what was priced. The guardrail inputs
  // don't affect the price and are excluded.
  const onEdit = (e: Event): void => { if (!guard.contains(e.target as Node)) invalidate(); };
  root.addEventListener('input', onEdit);
  root.addEventListener('change', onEdit);

  async function preview(): Promise<void> {
    if (previewBtn.disabled) return;
    const form = readForm();
    const invalid = validateAzureExchangeForm(form.sources, sources.map(r => r.quantity), form.targets);
    if (invalid) {
      setMessage(resultBox, invalid, 'error');
      return;
    }
    invalidate();
    const rev = revision;
    setMessage(resultBox, 'Asking Azure to price the exchange...', 'loading');
    previewBtn.disabled = true;
    const request = { subscription_id: subscriptionId, ...form };
    try {
      const resp = await api.getAzureCompatibleOfferings(request);
      if (!sessionOpen || rev !== revision) return;
      renderPreview(resultBox, resp);
      const p = resp.preview;
      if (p && p.net_payable !== null && (p.policy_errors ?? []).length === 0) {
        lastPreview = resp;
        lastRequest = request;
        maxInput.placeholder = `quoted ${formatAzureAmount(p.net_payable, undefined)}`;
        currencyInput.placeholder = p.net_payable_currency ? `quoted ${p.net_payable_currency}` : 'e.g. USD';
        guard.classList.remove('hidden');
        if (canExecute) executeBtn.classList.remove('hidden');
      }
    } catch (error) {
      if (!sessionOpen || rev !== revision) return;
      setMessage(resultBox, describeAzureExchangeError(error, 'preview'), 'error');
    } finally {
      previewBtn.disabled = false;
    }
  }

  // Hides Execute and drops the preview it would send, keeping the outcome
  // message and the preview on screen.
  function retirePreview(): void {
    executeBtn.classList.add('hidden');
    guard.classList.add('hidden');
    lastPreview = null;
    lastRequest = null;
  }

  async function execute(): Promise<void> {
    if (executeBtn.disabled || !lastPreview?.preview || !lastRequest) return;
    const guardError = validateAzureGuardrails(maxInput.value, currencyInput.value);
    const status = el('p', 'error azure-exchange-guard-error');
    guard.querySelector('.azure-exchange-guard-error')?.remove();
    if (guardError) {
      status.textContent = guardError;
      guard.appendChild(status);
      return;
    }
    const req: AzureExecuteExchangeRequest = {
      ...lastRequest, max_payment_due: maxInput.value.trim(), currency: currencyInput.value.trim().toUpperCase(),
    };
    const quoted = lastPreview.preview;
    const body = el('div');
    body.append(
      el('p', undefined, `Return: ${req.sources.map((s, i) => `${s.quantity} × ${sources[i]!.sku} (${sources[i]!.display_name || s.reservation_id})`).join(', ')}`),
      el('p', undefined, `Buy: ${req.targets.map(t => `${t.quantity} × ${t.sku} in ${t.location}, ${azureTermLabel(t.term)}`).join(', ')}`),
      el('p', undefined, `Quoted net payable: ${formatAzureAmount(quoted.net_payable, quoted.net_payable_currency)}`),
      el('p', undefined, `Refused if Azure's fresh price is above ${req.currency} ${req.max_payment_due}.`),
      el('p', undefined, 'This exchange executes immediately and cannot be reversed.'),
    );
    executeBtn.disabled = true;
    previewBtn.disabled = true;
    try {
      const ok = await confirmDialog({ title: 'Execute Azure reservation exchange', body, confirmLabel: 'Execute exchange', destructive: true, initialFocus: 'cancel' });
      if (!ok || !sessionOpen) return;
      setMessage(statusBox, 'Executing exchange...', 'loading');
      const result = await api.executeAzureExchange(req);
      onExecuted();
      const done = `Exchange ${result.status || 'submitted'}. Net payable ${formatAzureAmount(result.net_payable, result.net_payable_currency)}; refunds ${formatAzureAmount(result.refunds_total, result.net_payable_currency)}; purchases ${formatAzureAmount(result.purchases_total, result.net_payable_currency)}. Session ${result.session_id}.`;
      // Closed mid-execute: the outcome still has to reach the user.
      if (!sessionOpen) {
        showToast({ message: done, kind: 'success' });
        return;
      }
      setMessage(statusBox, done, 'success-message');
      retirePreview();
    } catch (error) {
      // An unknown outcome may still commit at Azure: refresh the list and
      // history, and retire this preview so a retry needs a fresh one.
      const unknown = isAzureExchangeOutcomeUnknown(error);
      if (unknown) onExecuted();
      const text = describeAzureExchangeError(error, 'execute');
      if (!sessionOpen) {
        // An unknown outcome stays until dismissed: it may mean money moved.
        showToast(unknown ? { message: text, kind: 'warning', timeout: null } : { message: text, kind: 'error' });
        return;
      }
      setMessage(statusBox, text, 'error');
      if (unknown) retirePreview();
    } finally {
      executeBtn.disabled = false;
      previewBtn.disabled = false;
    }
  }

  previewBtn.addEventListener('click', () => { void preview(); });
  executeBtn.addEventListener('click', () => { void execute(); });
  cancelBtn.addEventListener('click', () => closeModal(modal));
  openModal(modal, { onClose: () => { sessionOpen = false; revision++; } });
}
