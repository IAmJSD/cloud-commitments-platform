/**
 * One approval surface on the Purchases page (issue #183).
 *
 * Pending purchases are approved or cancelled from the Approval queue card at
 * the top of the page. The Purchase History table still lists those rows (it
 * is the full log), but instead of repeating the queue's Approve / Cancel
 * controls it links each one to its queue row.
 */

import { escapeHtmlAttr } from './utils';

const QUEUE_SECTION_ID = 'purchases-approval-queue-section';
const QUEUE_ID = 'purchases-approval-queue';

/** The link a pending history row shows in place of Approve / Cancel. */
export function renderApprovalQueueLink(purchaseId: string): string {
  return `<button type="button" class="btn-link history-queue-link" data-queue-id="${escapeHtmlAttr(purchaseId)}" aria-label="Review in approval queue" title="Approve or cancel this purchase in the Approval queue">Review in queue ↑</button>`;
}

/**
 * Scroll to the row's entry in the Approval queue, flash it and move focus to
 * its first action so keyboard users land on Approve. When a queue column
 * filter hides the row, fall back to the queue card itself.
 */
export function jumpToApprovalQueueRow(purchaseId: string): void {
  const row = document.querySelector<HTMLTableRowElement>(
    `#${QUEUE_ID} tr[data-execution-id="${CSS.escape(purchaseId)}"]`,
  );
  const target: HTMLElement | null = row ?? document.getElementById(QUEUE_SECTION_ID);
  if (!target) return;
  target.scrollIntoView({ behavior: 'smooth', block: 'center' });
  if (!row) return;
  row.classList.add('history-row-highlight');
  window.setTimeout(() => row.classList.remove('history-row-highlight'), 4000);
  row.querySelector<HTMLButtonElement>('button:not([disabled])')?.focus({ preventScroll: true });
}

/** Bind the queue links rendered inside `container` (the history table). */
export function wireApprovalQueueLinks(container: HTMLElement): void {
  container.querySelectorAll<HTMLButtonElement>('.history-queue-link').forEach(btn => {
    btn.addEventListener('click', () => {
      const id = btn.dataset['queueId'];
      if (id) jumpToApprovalQueueRow(id);
    });
  });
}
