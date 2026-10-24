/**
 * Azure RI exchange entry points on the exchangeable-reservations table
 * (issue #81): a selection column and an "Exchange selected" action. The
 * exchange modal itself (riexchange-azure-modal.ts) is loaded on demand.
 */

import type { ExchangeableAzureRI } from './api';
import { canAccess } from './permissions';

/** Previewing needs view:purchases; executing needs execute:ri-exchange (carved out of admin:*). */
export function canPreviewAzureExchange(): boolean {
  return canAccess('view', 'purchases');
}

function el<K extends keyof HTMLElementTagNameMap>(tag: K, className?: string, text?: string): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

/** Checkbox cell that selects one reservation for an exchange. */
export function buildAzureSelectCell(r: ExchangeableAzureRI, index: number): HTMLTableCellElement {
  const td = el('td');
  const box = el('input', 'azure-ri-select');
  box.type = 'checkbox';
  box.dataset['index'] = String(index);
  box.setAttribute('aria-label', `Select ${r.display_name || r.reservation_id} for exchange`);
  td.appendChild(box);
  return td;
}

/**
 * Adds the "Exchange selected" action above the reservations table. The
 * button counts the checked rows and opens the exchange modal for them.
 */
export function attachAzureExchangeToolbar(container: HTMLElement, table: HTMLTableElement, reservations: readonly ExchangeableAzureRI[], onExecuted: () => void): void {
  const bar = el('div', 'setting-row azure-exchange-toolbar');
  const btn = el('button', 'btn btn-small', 'Exchange selected');
  btn.type = 'button';
  btn.id = 'azure-exchange-selected-btn';
  const selected = (): ExchangeableAzureRI[] => Array.from(table.querySelectorAll<HTMLInputElement>('input.azure-ri-select:checked'))
    .map(b => reservations[Number(b.dataset['index'])])
    .filter((r): r is ExchangeableAzureRI => r !== undefined);
  const sync = (): void => {
    const n = selected().length;
    btn.disabled = n === 0;
    btn.textContent = n === 0 ? 'Exchange selected' : `Exchange selected (${n})`;
  };
  table.addEventListener('change', sync);
  btn.addEventListener('click', () => {
    const picked = selected();
    void import('./riexchange-azure-modal').then(m => m.openAzureExchangeModal(picked, onExecuted));
  });
  bar.append(btn, el('span', 'help-text', 'Tick the reservations to hand back, then preview what Azure offers for them.'));
  sync();
  container.insertBefore(bar, table);
}
