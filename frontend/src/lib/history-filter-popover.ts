/**
 * Lightweight column-filter popover shared by the two History tables
 * (Purchase History + Approval Queue) — issue #166 follow-up.
 *
 * Mirrors the visual shape of recommendations.ts's popover (so the same
 * CSS in styles/components.css applies) but is intentionally simpler:
 *   - One popover open at a time across both tables; each button is
 *     identified by (scope, column) because the tables share column ids.
 *   - The caller owns the column-id enum, the filter state slice, and
 *     the re-render callback; this module only renders the popover DOM,
 *     wires inputs to commit, and manages anchor/global-listener teardown.
 *   - No "All Savings Plans" group affordance (recs-specific).
 *
 * The popover is appended to `document.body` (portal pattern) so it
 * survives the table's `innerHTML` rewrite on every commit; the next
 * render is expected to rebind the trigger button by `[data-column=…]`.
 */

import {
  parseNumericFilter,
  type ColumnFilterKind,
} from './column-filters';
import { escapeHtmlAttr } from '../utils';
import { iconSvg } from '../icons';

export interface PopoverConfig<TColumnId extends string> {
  // Namespace of the owning table (e.g. 'history' / 'queue'). Both tables
  // on the Purchases tab use the same bare column ids, so the open-popover
  // key, the anchor lookup and the generated element ids are all qualified
  // by scope; otherwise one table's popover toggles or refocuses the other
  // table's button (issue #281). Must match the scope passed to
  // renderHistoryFilterButton.
  scope: string;
  column: TColumnId;
  anchor: HTMLElement;
  // Current filter for this column (null = not narrowed).
  currentFilter: ColumnFilterKind | undefined;
  // Header label rendered inside the popover ("Filter <Label>").
  headerLabel: string;
  // 'numeric' → free-text expression input; 'categorical' → checkbox list.
  kind: 'numeric' | 'categorical';
  // Categorical only: distinct raw cell values (post-extract). Order is
  // preserved as the caller provided it (caller controls sort).
  distinctValues?: readonly string[];
  // Categorical only: render-time label resolver (e.g. account_id → name).
  displayLabel?: (value: string) => string;
  // Commit callback: caller writes to state then re-renders the table.
  // Passing `null` clears the filter (no narrowing applied).
  onCommit: (filter: ColumnFilterKind | null) => void;
}

interface OpenPopoverHandle {
  el: HTMLDivElement;
  scope: string;
  column: string;
}

// Resolve a table's filter button by (scope, column). Document-wide, because
// the button is re-created on every table re-render, but never ambiguous
// across tables thanks to the scope attribute.
function findFilterButton(scope: string, column: string): HTMLElement | null {
  return document.querySelector<HTMLElement>(
    `.history-column-filter-btn[data-filter-scope="${CSS.escape(scope)}"][data-column="${CSS.escape(column)}"]`,
  );
}

let openPopover: OpenPopoverHandle | null = null;
let outsideClickHandler: ((e: MouseEvent) => void) | null = null;
let escKeyHandler: ((e: KeyboardEvent) => void) | null = null;
let scrollCloseHandler: ((e: Event) => void) | null = null;
let resizeHandler: (() => void) | null = null;
let lastAnchorSelector: (() => HTMLElement | null) | null = null;

function detachGlobalListeners(): void {
  if (outsideClickHandler) document.removeEventListener('mousedown', outsideClickHandler);
  if (escKeyHandler) document.removeEventListener('keydown', escKeyHandler);
  if (scrollCloseHandler) {
    window.removeEventListener(
      'scroll',
      scrollCloseHandler,
      { capture: true } as EventListenerOptions,
    );
  }
  if (resizeHandler) window.removeEventListener('resize', resizeHandler);
  outsideClickHandler = null;
  escKeyHandler = null;
  scrollCloseHandler = null;
  resizeHandler = null;
}

export function closeOpenHistoryPopover(restoreFocus = false): void {
  if (!openPopover) return;
  const { el, scope, column } = openPopover;
  el.remove();
  openPopover = null;
  detachGlobalListeners();
  const trigger = findFilterButton(scope, column);
  // Always clear the stale expanded state, regardless of focus restoration.
  trigger?.setAttribute('aria-expanded', 'false');
  if (restoreFocus) {
    trigger?.focus();
  }
  lastAnchorSelector = null;
}

function positionPopover(popover: HTMLElement, anchor: HTMLElement): void {
  const rect = anchor.getBoundingClientRect();
  popover.style.display = 'block';
  const popRect = popover.getBoundingClientRect();
  const margin = 8;
  let top = rect.bottom + 4;
  if (top + popRect.height > window.innerHeight - margin) {
    top = Math.max(margin, rect.top - popRect.height - 4);
  }
  let left = rect.left;
  if (left + popRect.width > window.innerWidth - margin) {
    left = Math.max(margin, window.innerWidth - margin - popRect.width);
  }
  popover.style.position = 'absolute';
  popover.style.top = `${top + window.scrollY}px`;
  popover.style.left = `${left + window.scrollX}px`;
}

function attachGlobalListeners(popover: HTMLElement): void {
  outsideClickHandler = (e: MouseEvent): void => {
    if (!openPopover) return;
    const target = e.target as Node | null;
    if (!target) return;
    if (popover.contains(target)) return;
    if (target instanceof Element && target.closest('.history-column-filter-btn')) return;
    closeOpenHistoryPopover();
  };
  escKeyHandler = (e: KeyboardEvent): void => {
    if (!openPopover) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      closeOpenHistoryPopover(true);
    }
  };
  scrollCloseHandler = (e: Event): void => {
    if (!openPopover) return;
    const target = e.target as Node | null;
    if (target && popover.contains(target)) return;
    closeOpenHistoryPopover();
  };
  resizeHandler = (): void => {
    if (!openPopover) return;
    const anchor = lastAnchorSelector?.();
    if (!anchor) {
      closeOpenHistoryPopover();
      return;
    }
    positionPopover(popover, anchor);
  };
  document.addEventListener('mousedown', outsideClickHandler);
  document.addEventListener('keydown', escKeyHandler);
  window.addEventListener('scroll', scrollCloseHandler, { capture: true, passive: true });
  window.addEventListener('resize', resizeHandler);
}

/**
 * Open the popover anchored to `anchor`. Toggles closed if the same
 * column is already open. Closes any previously-open popover (from
 * either History table — only one is open at a time, even across
 * the two tables, to avoid stacking dialogs).
 */
export function openHistoryColumnPopover<TColumnId extends string>(
  config: PopoverConfig<TColumnId>,
): void {
  // Toggle / swap.
  if (openPopover) {
    const sameColumn = openPopover.scope === config.scope
      && openPopover.column === config.column;
    closeOpenHistoryPopover();
    if (sameColumn) return;
  }

  const popover = document.createElement('div');
  popover.className = 'column-filter-popover';
  popover.setAttribute('role', 'dialog');
  popover.setAttribute('aria-modal', 'false');

  const idSuffix = `${config.scope}-${String(config.column)}`;
  const headingId = `history-column-filter-heading-${idSuffix}`;
  popover.setAttribute('aria-labelledby', headingId);

  const heading = document.createElement('h3');
  heading.id = headingId;
  heading.className = 'column-filter-heading';
  heading.textContent = `Filter ${config.headerLabel}`;
  popover.appendChild(heading);

  let commitAllRef: ((target: boolean) => void) | null = null;
  let inputRef: HTMLInputElement | null = null;
  let errorRef: HTMLElement | null = null;

  if (config.kind === 'numeric') {
    const label = document.createElement('label');
    label.className = 'column-filter-numeric-label';
    label.textContent = 'Expression';
    const input = document.createElement('input');
    input.type = 'text';
    input.className = 'column-filter-numeric-input';
    input.placeholder = 'e.g. >100, 50..200, 5';
    const errorId = `history-column-filter-error-${idSuffix}`;
    input.setAttribute('aria-describedby', errorId);
    label.appendChild(input);
    popover.appendChild(label);

    const errorEl = document.createElement('div');
    errorEl.id = errorId;
    errorEl.className = 'column-filter-error';
    errorEl.setAttribute('role', 'status');
    popover.appendChild(errorEl);

    inputRef = input;
    errorRef = errorEl;

    const cur = config.currentFilter;
    if (cur && cur.kind === 'expr') input.value = cur.expr;

    const commit = (): void => {
      const expr = input.value.trim();
      if (expr === '') {
        errorEl.textContent = '';
        config.onCommit(null);
        return;
      }
      const parsed = parseNumericFilter(expr);
      if (!parsed.ok) {
        errorEl.textContent = parsed.error;
        return;
      }
      errorEl.textContent = '';
      config.onCommit({ kind: 'expr', expr });
    };
    input.addEventListener('blur', commit);
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        commit();
      }
    });
  } else {
    const distinct = config.distinctValues ?? [];
    const display = config.displayLabel ?? ((v: string): string => (v === '' ? '(empty)' : v));

    const allLabel = document.createElement('label');
    allLabel.className = 'column-filter-all';
    const allBox = document.createElement('input');
    allBox.type = 'checkbox';
    allBox.dataset['role'] = 'all';
    allLabel.appendChild(allBox);
    const allText = document.createElement('span');
    allText.textContent = '(All)';
    allLabel.appendChild(allText);
    popover.appendChild(allLabel);

    const list = document.createElement('div');
    list.className = 'column-filter-list';
    const checkboxes = new Map<string, HTMLInputElement>();
    for (const value of distinct) {
      const itemLabel = document.createElement('label');
      itemLabel.className = 'column-filter-item';
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.dataset['value'] = value;
      itemLabel.appendChild(cb);
      const text = document.createElement('span');
      text.textContent = display(value);
      itemLabel.appendChild(text);
      list.appendChild(itemLabel);
      checkboxes.set(value, cb);
    }
    popover.appendChild(list);

    // Resync checked state from current filter.
    const cur = config.currentFilter;
    if (cur == null) {
      checkboxes.forEach((cb) => { cb.checked = true; });
    } else if (cur.kind === 'set') {
      const set = new Set(cur.values);
      checkboxes.forEach((cb, v) => { cb.checked = set.has(v); });
    }

    const updateAllTriState = (): void => {
      const total = checkboxes.size;
      let checked = 0;
      checkboxes.forEach((cb) => { if (cb.checked) checked++; });
      allBox.indeterminate = checked > 0 && checked < total;
      allBox.checked = checked === total && total > 0;
    };
    updateAllTriState();

    const commit = (): void => {
      const selected: string[] = [];
      checkboxes.forEach((cb, value) => { if (cb.checked) selected.push(value); });
      if (selected.length === checkboxes.size) {
        config.onCommit(null);
      } else {
        config.onCommit({ kind: 'set', values: selected });
      }
      updateAllTriState();
    };
    const commitAll = (target: boolean): void => {
      checkboxes.forEach((cb) => { cb.checked = target; });
      if (target) {
        config.onCommit(null);
      } else {
        config.onCommit({ kind: 'set', values: [] });
      }
      updateAllTriState();
    };
    commitAllRef = commitAll;

    checkboxes.forEach((cb) => {
      cb.addEventListener('change', commit);
    });
    allBox.addEventListener('change', () => {
      commitAll(allBox.checked);
    });
  }

  // Footer with Clear button.
  const footer = document.createElement('div');
  footer.className = 'column-filter-footer';
  const clearBtn = document.createElement('button');
  clearBtn.type = 'button';
  clearBtn.className = 'column-filter-clear';
  // Issue #727 (UX-28): on a categorical filter this button selects
  // nothing (an explicit empty allow-list, which hides every row), so it
  // must not be called "Clear". (All) is how the filter is removed.
  clearBtn.textContent = config.kind === 'numeric' ? 'Clear' : 'Select none';
  clearBtn.addEventListener('click', () => {
    if (config.kind === 'numeric') {
      if (inputRef) inputRef.value = '';
      if (errorRef) errorRef.textContent = '';
      config.onCommit(null);
    } else {
      // Categorical Clear: explicit empty allow-list to distinguish from
      // "no filter applied" (which would show every row). Mirrors the
      // recommendations popover behaviour (issue #482 / #700).
      commitAllRef?.(false);
    }
  });
  footer.appendChild(clearBtn);
  popover.appendChild(footer);

  document.body.appendChild(popover);
  config.anchor.setAttribute('aria-expanded', 'true');
  positionPopover(popover, config.anchor);

  const scope = config.scope;
  const column = String(config.column);
  openPopover = { el: popover, scope, column };
  lastAnchorSelector = (): HTMLElement | null => findFilterButton(scope, column);
  attachGlobalListeners(popover);

  // Focus first interactive control for keyboard users.
  const firstFocusable = inputRef
    ?? popover.querySelector<HTMLInputElement>('input[type="checkbox"]');
  firstFocusable?.focus();
}

/**
 * Render the filter-icon button placed inside a `<th>` cell. The caller
 * binds the click handler against `.history-column-filter-btn` after the
 * table is innerHTML-rewritten. `scope` names the owning table and must
 * match the `scope` the caller passes to openHistoryColumnPopover.
 */
export function renderHistoryFilterButton<TColumnId extends string>(
  column: TColumnId,
  label: string,
  active: boolean,
  scope: string,
): string {
  const cls = `history-column-filter-btn${active ? ' active' : ''}`;
  const ariaLabel = escapeHtmlAttr(
    active ? `Filter ${label} - currently active` : `Filter ${label}`,
  );
  const safeColumn = escapeHtmlAttr(String(column));
  const safeScope = escapeHtmlAttr(scope);
  // Use the same filter icon the recommendations popover uses so the
  // two surfaces are visually identical to the user.
  return `<button type="button" class="${cls}" data-filter-scope="${safeScope}" data-column="${safeColumn}" aria-haspopup="dialog" aria-expanded="false" aria-label="${ariaLabel}" title="${ariaLabel}">${iconSvg('filter')}</button>`;
}
