/**
 * Shared column-filter popover lifecycle (issue #18): open/toggle/swap,
 * outside click, Escape, commits, Clear, inline expression errors, and
 * re-sync after the owning table re-renders.
 */

import {
  closeColumnFilterPopover,
  openColumnFilterPopover,
  rebindColumnFilterPopover,
  sortedDistinctValues,
  type ColumnFilterPopoverConfig,
} from '../lib/column-filter-popover';
import type { ColumnFilterKind } from '../lib/column-filters';

let filters: Record<string, ColumnFilterKind | null> = {};
let onCommit: jest.Mock;

function mountTable(scope: string): void {
  const host = document.createElement('div');
  host.id = scope;
  host.innerHTML = ['provider', 'count']
    .map((c) => `<button class="column-filter-btn" data-column="${c}" aria-expanded="false">F</button>`)
    .join('');
  document.getElementById(scope)?.remove();
  document.body.appendChild(host);
}

const trigger = (scope: string, column: string): HTMLElement =>
  document.querySelector<HTMLElement>(`#${scope} .column-filter-btn[data-column="${column}"]`)!;

function config(scope: string, column: 'provider' | 'count'): ColumnFilterPopoverConfig {
  return {
    scope,
    column,
    label: column === 'provider' ? 'Provider' : 'Count',
    kind: column === 'count' ? 'numeric' : 'categorical',
    distinctValues: ['', 'aws', 'azure'],
    getFilter: () => filters[`${scope}:${column}`],
    onCommit: (f) => {
      filters[`${scope}:${column}`] = f;
      onCommit(scope, column, f);
    },
    findTrigger: () => trigger(scope, column),
  };
}

function openFor(scope: string, column: 'provider' | 'count'): void {
  openColumnFilterPopover(config(scope, column), trigger(scope, column));
}

const popovers = (): HTMLElement[] => Array.from(document.querySelectorAll<HTMLElement>('.column-filter-popover'));
const box = (value: string): HTMLInputElement =>
  document.querySelector<HTMLInputElement>(`.column-filter-popover input[data-value="${value}"]`)!;
const allBox = (): HTMLInputElement =>
  document.querySelector<HTMLInputElement>('.column-filter-popover input[data-role="all"]')!;
const exprInput = (): HTMLInputElement =>
  document.querySelector<HTMLInputElement>('.column-filter-popover .column-filter-numeric-input')!;

beforeEach(() => {
  filters = {};
  onCommit = jest.fn();
  mountTable('plans');
  mountTable('riex');
});

afterEach(() => {
  closeColumnFilterPopover();
  document.body.innerHTML = '';
});

describe('open / close lifecycle', () => {
  test('opens an accessible dialog anchored to the trigger and focuses the first control', () => {
    openFor('plans', 'provider');
    const [pop] = popovers();
    expect(pop?.getAttribute('role')).toBe('dialog');
    expect(pop?.querySelector('h3')?.textContent).toBe('Filter Provider');
    expect(pop?.getAttribute('aria-labelledby')).toBe('plans-column-filter-provider-heading');
    expect(trigger('plans', 'provider').getAttribute('aria-expanded')).toBe('true');
    expect(document.activeElement).toBe(allBox());
  });

  test('clicking the same trigger again closes and returns focus to it', () => {
    openFor('plans', 'provider');
    openFor('plans', 'provider');
    expect(popovers()).toHaveLength(0);
    expect(document.activeElement).toBe(trigger('plans', 'provider'));
    expect(trigger('plans', 'provider').getAttribute('aria-expanded')).toBe('false');
  });

  test('opening another column, or the same column in another table, swaps popovers', () => {
    openFor('plans', 'provider');
    openFor('plans', 'count');
    expect(popovers()).toHaveLength(1);
    expect(popovers()[0]?.textContent).toContain('Filter Count');
    openFor('riex', 'count');
    expect(popovers()).toHaveLength(1);
    expect(trigger('plans', 'count').getAttribute('aria-expanded')).toBe('false');
    expect(trigger('riex', 'count').getAttribute('aria-expanded')).toBe('true');
  });

  test('outside mousedown closes; mousedown inside the popover does not', () => {
    openFor('plans', 'provider');
    box('aws').dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
    expect(popovers()).toHaveLength(1);
    document.body.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
    expect(popovers()).toHaveLength(0);
  });

  test('Escape closes and restores focus to the trigger', () => {
    openFor('riex', 'provider');
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    expect(popovers()).toHaveLength(0);
    expect(document.activeElement).toBe(trigger('riex', 'provider'));
  });

  test('a popover removed by someone else is forgotten instead of toggled', () => {
    openFor('plans', 'provider');
    popovers().forEach((p) => p.remove());
    openFor('plans', 'provider');
    expect(popovers()).toHaveLength(1);
  });
});

describe('categorical commits', () => {
  test('unchecking a value commits the remaining allow-list; rechecking all clears', () => {
    openFor('plans', 'provider');
    expect(box('aws').checked).toBe(true);
    box('aws').click();
    expect(onCommit).toHaveBeenLastCalledWith('plans', 'provider', { kind: 'set', values: ['', 'azure'] });
    expect(allBox().indeterminate).toBe(true);
    box('aws').click();
    expect(onCommit).toHaveBeenLastCalledWith('plans', 'provider', null);
  });

  test('Select none writes an explicit empty allow-list', () => {
    openFor('plans', 'provider');
    const btn = document.querySelector<HTMLButtonElement>('.column-filter-clear')!;
    // #727 (UX-28): it hides every row, so it is not labelled "Clear".
    expect(btn.textContent).toBe('Select none');
    btn.click();
    expect(onCommit).toHaveBeenLastCalledWith('plans', 'provider', { kind: 'set', values: [] });
    expect(box('azure').checked).toBe(false);
  });

  test('empty value renders as (empty) unless displayLabel overrides it', () => {
    openFor('plans', 'provider');
    expect(box('').parentElement?.textContent).toBe('(empty)');
  });
});

describe('numeric commits', () => {
  test('Enter commits a valid expression', () => {
    openFor('plans', 'count');
    exprInput().value = '>2';
    exprInput().dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(onCommit).toHaveBeenLastCalledWith('plans', 'count', { kind: 'expr', expr: '>2' });
  });

  test('an invalid expression shows an inline error and does not commit', () => {
    openFor('plans', 'count');
    exprInput().value = '>>';
    exprInput().dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(onCommit).not.toHaveBeenCalled();
    const err = document.getElementById('plans-column-filter-count-error');
    expect(err?.textContent).not.toBe('');
    expect(exprInput().getAttribute('aria-describedby')).toBe('plans-column-filter-count-error');
  });

  test('Clear empties the input and clears the filter', () => {
    filters['plans:count'] = { kind: 'expr', expr: '5' };
    openFor('plans', 'count');
    expect(exprInput().value).toBe('5');
    const btn = document.querySelector<HTMLButtonElement>('.column-filter-clear')!;
    expect(btn.textContent).toBe('Clear');
    btn.click();
    expect(exprInput().value).toBe('');
    expect(onCommit).toHaveBeenLastCalledWith('plans', 'count', null);
  });
});

describe('rebind after re-render', () => {
  test('re-anchors to the fresh trigger and re-syncs from external state', () => {
    openFor('plans', 'provider');
    filters['plans:provider'] = { kind: 'set', values: ['azure'] };
    mountTable('plans');
    rebindColumnFilterPopover('plans');
    expect(trigger('plans', 'provider').getAttribute('aria-expanded')).toBe('true');
    expect(box('aws').checked).toBe(false);
    expect(box('azure').checked).toBe(true);
  });

  test('ignores other scopes and closes when the column disappears', () => {
    openFor('plans', 'provider');
    rebindColumnFilterPopover('riex');
    expect(popovers()).toHaveLength(1);
    document.getElementById('plans')!.innerHTML = '';
    rebindColumnFilterPopover('plans');
    expect(popovers()).toHaveLength(0);
  });

  test('does not overwrite the expression input while it has focus', () => {
    openFor('plans', 'count');
    exprInput().value = '>1';
    filters['plans:count'] = { kind: 'expr', expr: '9' };
    rebindColumnFilterPopover('plans');
    expect(exprInput().value).toBe('>1');
  });
});

test('sortedDistinctValues puts the empty value first, then locale order', () => {
  expect(sortedDistinctValues(['b', '', 'a', 'b'], (v) => v)).toEqual(['', 'a', 'b']);
});
