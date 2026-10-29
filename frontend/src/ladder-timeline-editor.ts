import type { ApiError } from './api/types';
import { amendLadderEvent, type LadderEvent, type LadderRun } from './api/ladder';
import { openModal, closeModal, modalCloseButtonHtml } from './modal';
import { escapeHtml } from './utils';
import { formatLadderAmount, formatLadderDateTime, formatLadderUnits, ladderAmountUnits, ladderLabel, ladderTotal } from './ladder-timeline-filters';

export function openLadderEventEditor(event: LadderEvent, run: LadderRun, siblings: readonly LadderEvent[], onReload: () => void): void {
  const modal = document.createElement('div');
  modal.className = 'modal hidden ladder-event-editor';
  modal.setAttribute('role', 'dialog');
  modal.setAttribute('aria-modal', 'true');
  modal.setAttribute('aria-labelledby', 'ladder-event-title');
  modal.innerHTML = `<div class="modal-content"><div class="modal-header">
    <h3 id="ladder-event-title">Edit planned purchase</h3>${modalCloseButtonHtml('ladder-event-cancel-btn')}</div><form>
    <p>${escapeHtml(ladderLabel('layer_type', event.layer_type))} / ${escapeHtml(ladderLabel('term', event.term))} / ${escapeHtml(ladderLabel('payment_option', event.payment_option))}</p>
    <p>Original plan: ${escapeHtml(formatLadderDateTime(run.started_at))}.
      ${run.actions.filter(action => action.layer === event.layer_type).map(action => `${escapeHtml(action.rationale)}${action.data_sources?.length ? ` Sources: ${escapeHtml(action.data_sources.join(', '))}.` : ''}`).join(' ')}</p>
    <p>These are planning budgets. Purchases are not executed by this editor.</p>
    <div class="form-row"><label for="ladder-event-date">Scheduled date and time (UTC)</label>
      <input id="ladder-event-date" type="datetime-local" step="1" required></div>
    <div class="form-row"><label for="ladder-event-amount">Planning budget (${event.amount_basis === 'commitment' ? 'commitment $/hr' : 'on-demand-equivalent $/hr'})</label>
      <input id="ladder-event-amount" inputmode="decimal" required></div>
    <p id="ladder-event-preview" aria-live="polite"></p>
    <p>Original run ceiling: ${escapeHtml(formatLadderAmount(run.total_hourly_commit))}.
      The server also enforces the current per-run cap.</p>
    <p>Reducing a tranche can cause the next planning run to add it back.
      Change target coverage in account settings for a lasting target reduction.</p>
    <p id="ladder-event-error" role="alert"></p>
    <div class="modal-footer"><button type="button" class="btn btn-secondary" id="ladder-event-cancel-btn" data-cancel>Cancel</button>
      <button type="button" class="btn btn-secondary" data-reset>Undo draft changes</button>
      <button type="button" class="btn btn-secondary" data-reload hidden>Reload and discard draft</button>
      <button type="submit" class="btn btn-primary">Save planned purchase</button></div>
    </form></div>`;
  document.body.append(modal);
  const form = modal.querySelector<HTMLFormElement>('form')!;
  const date = modal.querySelector<HTMLInputElement>('#ladder-event-date')!;
  const amount = modal.querySelector<HTMLInputElement>('#ladder-event-amount')!;
  const preview = modal.querySelector<HTMLElement>('#ladder-event-preview')!;
  const error = modal.querySelector<HTMLElement>('#ladder-event-error')!;
  const save = modal.querySelector<HTMLButtonElement>('button[type="submit"]')!;
  const cancel = modal.querySelector<HTMLButtonElement>('[data-cancel]')!;
  const reload = modal.querySelector<HTMLButtonElement>('[data-reload]')!;
  let pending = false;
  const initialDate = new Date(event.scheduled_date).toISOString().slice(0, 19);
  const originalTotal = ladderAmountUnits(ladderTotal(siblings));
  const restore = (): void => {
    date.value = initialDate;
    amount.value = event.amount_usd_hr;
    error.textContent = '';
    updatePreview();
  };
  const updatePreview = (): void => {
    try {
      const units = ladderAmountUnits(amount.value);
      const total = originalTotal - ladderAmountUnits(event.amount_usd_hr) + units;
      preview.textContent = `Current run total after this change: ${formatLadderAmount(formatLadderUnits(total))}.`;
      save.disabled = pending || units <= 0n || total > ladderAmountUnits(run.total_hourly_commit);
    } catch {
      preview.textContent = 'Enter a positive decimal with up to six fractional digits.';
      save.disabled = true;
    }
  };
  restore();
  const initialControlDate = date.value;
  amount.addEventListener('input', updatePreview);
  modal.querySelector('[data-reset]')!.addEventListener('click', restore);
  reload.addEventListener('click', () => { closeModal(modal); onReload(); });
  modal.querySelector('[data-cancel]')!.addEventListener('click', () => { if (!pending) closeModal(modal); });
  modal.addEventListener('keydown', e => {
    if (pending && e.key === 'Escape') {
      e.preventDefault();
      e.stopImmediatePropagation();
    }
  }, true);
  form.addEventListener('submit', async e => {
    e.preventDefault();
    if (pending || save.disabled || !form.reportValidity()) return;
    const proposedDate = date.value === initialControlDate ? event.scheduled_date : `${date.value}Z`;
    const proposed = new Date(proposedDate);
    if (!Number.isFinite(proposed.getTime()) || proposed.getTime() <= Date.now()) {
      error.textContent = 'Choose a future date and time in UTC.';
      return;
    }
    pending = true;
    form.querySelectorAll<HTMLInputElement | HTMLButtonElement>('input,button').forEach(input => { input.disabled = true; });
    cancel.disabled = false;
    cancel.setAttribute('aria-disabled', 'true');
    cancel.textContent = 'Save in progress';
    cancel.focus();
    error.textContent = 'Saving planned purchase...';
    try {
      await amendLadderEvent(event.id, { expected_revision: event.revision, scheduled_date: date.value === initialControlDate ? event.scheduled_date : proposed.toISOString(), amount_usd_hr: amount.value });
      closeModal(modal);
      onReload();
    } catch (err) {
      const conflict = (err as ApiError).status === 409;
      reload.hidden = !conflict;
      error.textContent = `${err instanceof Error ? err.message : 'Save failed'}. Your draft is retained.${conflict ? ' The plan changed; reload it to edit the current values.' : ''}`;
    } finally {
      pending = false;
      form.querySelectorAll<HTMLInputElement | HTMLButtonElement>('input,button').forEach(input => { input.disabled = false; });
      cancel.removeAttribute('aria-disabled');
      cancel.textContent = 'Cancel';
      updatePreview();
    }
  });
  openModal(modal, { initialFocus: '#ladder-event-date', onClose: () => modal.remove() });
}
