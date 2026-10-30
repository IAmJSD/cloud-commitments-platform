/**
 * Issue #281: the Purchase History and Approval Queue tables render filter
 * buttons with the same bare column ids. The shared popover must key the
 * open popover and resolve its anchor by (scope, column) so the two tables
 * do not toggle, refocus or leave aria-expanded stuck on each other.
 */

import {
  closeOpenHistoryPopover,
  openHistoryColumnPopover,
  renderHistoryFilterButton,
} from '../lib/history-filter-popover';

function mountTables(): { queueBtn: HTMLElement; historyBtn: HTMLElement } {
  // Queue first in the DOM, mirroring the Purchases tab layout, so a bare
  // document-wide `[data-column="provider"]` lookup would hit the queue.
  document.body.innerHTML = `
    <div id="queue">${renderHistoryFilterButton('provider', 'Provider', false, 'queue')}</div>
    <div id="history">${renderHistoryFilterButton('provider', 'Provider', false, 'history')}</div>
  `;
  return {
    queueBtn: document.querySelector<HTMLElement>('#queue .history-column-filter-btn')!,
    historyBtn: document.querySelector<HTMLElement>('#history .history-column-filter-btn')!,
  };
}

function open(scope: string, anchor: HTMLElement): void {
  openHistoryColumnPopover({
    scope,
    column: 'provider',
    anchor,
    currentFilter: undefined,
    headerLabel: `Provider (${scope})`,
    kind: 'categorical',
    distinctValues: ['aws', 'azure'],
    onCommit: jest.fn(),
  });
}

const popovers = (): NodeListOf<HTMLElement> =>
  document.querySelectorAll<HTMLElement>('.column-filter-popover');

describe('history filter popover scoping (issue #281)', () => {
  afterEach(() => {
    closeOpenHistoryPopover();
    document.body.innerHTML = '';
  });

  test('opening the same column in the other table swaps instead of closing', () => {
    const { queueBtn, historyBtn } = mountTables();
    open('queue', queueBtn);
    expect(popovers()).toHaveLength(1);

    open('history', historyBtn);
    expect(popovers()).toHaveLength(1);
    expect(popovers()[0]?.textContent).toContain('Provider (history)');
    expect(queueBtn.getAttribute('aria-expanded')).toBe('false');
    expect(historyBtn.getAttribute('aria-expanded')).toBe('true');
  });

  test('re-opening the same scope and column still toggles closed', () => {
    const { historyBtn } = mountTables();
    open('history', historyBtn);
    open('history', historyBtn);
    expect(popovers()).toHaveLength(0);
    expect(historyBtn.getAttribute('aria-expanded')).toBe('false');
  });

  test('Escape returns focus to the owning table button', () => {
    const { queueBtn, historyBtn } = mountTables();
    open('history', historyBtn);
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));

    expect(popovers()).toHaveLength(0);
    expect(document.activeElement).toBe(historyBtn);
    expect(historyBtn.getAttribute('aria-expanded')).toBe('false');
    expect(queueBtn.getAttribute('aria-expanded')).toBe('false');
  });

  test('a commit that re-renders the table keeps the new button expanded and re-anchors to it', () => {
    mountTables();
    const historyBox = document.getElementById('history')!;
    const onCommit = jest.fn(() => {
      // The caller re-renders its table, replacing the trigger button.
      historyBox.innerHTML = renderHistoryFilterButton('provider', 'Provider', true, 'history');
    });
    const oldBtn = historyBox.querySelector<HTMLElement>('.history-column-filter-btn')!;
    openHistoryColumnPopover({
      scope: 'history',
      column: 'provider',
      anchor: oldBtn,
      currentFilter: undefined,
      headerLabel: 'Provider',
      kind: 'categorical',
      distinctValues: ['aws', 'azure'],
      onCommit,
    });
    const box = popovers()[0]!.querySelector<HTMLInputElement>('input[data-value="aws"]')!;
    box.checked = false;
    box.dispatchEvent(new Event('change'));

    expect(onCommit).toHaveBeenCalledWith({ kind: 'set', values: ['azure'] });
    const newBtn = historyBox.querySelector<HTMLElement>('.history-column-filter-btn')!;
    expect(newBtn).not.toBe(oldBtn);
    expect(popovers()).toHaveLength(1);
    expect(newBtn.getAttribute('aria-expanded')).toBe('true');
    // Closing still clears the new button's state.
    closeOpenHistoryPopover();
    expect(newBtn.getAttribute('aria-expanded')).toBe('false');
  });

  test('a button rendered while its popover is open starts expanded; others do not', () => {
    const { historyBtn } = mountTables();
    open('history', historyBtn);
    expect(renderHistoryFilterButton('provider', 'Provider', false, 'history')).toContain('aria-expanded="true"');
    expect(renderHistoryFilterButton('provider', 'Provider', false, 'queue')).toContain('aria-expanded="false"');
    expect(renderHistoryFilterButton('service', 'Service', false, 'history')).toContain('aria-expanded="false"');
  });

  test('popover element ids are unique per table', () => {
    const { queueBtn } = mountTables();
    open('queue', queueBtn);
    const heading = popovers()[0]?.querySelector('h3');
    expect(heading?.id).toBe('history-column-filter-heading-queue-provider');
  });
});
