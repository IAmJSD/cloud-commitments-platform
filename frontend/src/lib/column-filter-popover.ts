/**
 * Shared column-filter popover (issue #18).
 *
 * Plans and both RI Exchange tables used to carry their own copy of the
 * same popover plumbing: build the dialog DOM, position it under the
 * trigger, close on outside click / Escape, and re-sync after the table
 * re-renders. This module owns that lifecycle; each table supplies its
 * column metadata and state accessors through ColumnFilterPopoverConfig.
 *
 * The popover is a portal (appended to document.body) so it survives the
 * table's innerHTML rewrite on every commit. After a re-render the table
 * calls rebindColumnFilterPopover(scope) so the open popover follows the
 * fresh trigger button and reflects the current filter state.
 *
 * Only one popover is open at a time, across every table that uses this
 * module. The open popover is identified by (scope, column), because
 * different tables reuse the same column ids.
 */

import { parseNumericFilter, type ColumnFilterKind } from './column-filters';

export interface ColumnFilterPopoverConfig {
  // Owning table, e.g. 'plans'. Namespaces element ids and the open-popover
  // identity, so two tables can share column ids.
  scope: string;
  column: string;
  // Header label rendered as "Filter <label>".
  label: string;
  // 'numeric' -> free-text expression input; 'categorical' -> checkboxes.
  kind: 'numeric' | 'categorical';
  // Categorical only: distinct raw values, in display order.
  distinctValues?: readonly string[];
  // Categorical only: value -> visible label. Defaults to '(empty)' for ''.
  displayLabel?: (value: string) => string;
  // Numeric only: example expression shown in the empty input.
  placeholder?: string;
  // Current filter for this column (undefined/null = not narrowed).
  getFilter: () => ColumnFilterKind | null | undefined;
  // Write the new filter (null clears) and re-render the table.
  onCommit: (filter: ColumnFilterKind | null) => void;
  // Resolve the column's trigger button in the current DOM. Called after
  // re-renders, on resize and on close, because the table rewrites its
  // buttons on every render.
  findTrigger: () => HTMLElement | null;
}

interface OpenPopover {
  config: ColumnFilterPopoverConfig;
  el: HTMLDivElement;
  input: HTMLInputElement | null;
  errorEl: HTMLElement | null;
  checkboxes: Map<string, HTMLInputElement>;
  allBox: HTMLInputElement | null;
}

let open: OpenPopover | null = null;
let listenersAttached = false;

const defaultDisplay = (v: string): string => (v === '' ? '(empty)' : v);

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

function updateAllTriState(p: OpenPopover): void {
  if (!p.allBox) return;
  const total = p.checkboxes.size;
  let checked = 0;
  p.checkboxes.forEach((cb) => { if (cb.checked) checked++; });
  p.allBox.indeterminate = checked > 0 && checked < total;
  p.allBox.checked = total > 0 && checked === total;
}

// Copy the current filter state into the open popover's controls. The
// numeric input is left alone while it has focus so a re-render triggered
// elsewhere does not clobber what the user is typing.
function resync(p: OpenPopover): void {
  const f = p.config.getFilter();
  if (p.input) {
    if (document.activeElement !== p.input) {
      p.input.value = f && f.kind === 'expr' ? f.expr : '';
      if (p.errorEl) p.errorEl.textContent = '';
    }
    return;
  }
  const values: ReadonlySet<string> | null = f == null ? null
    : new Set(f.kind === 'set' ? f.values : []);
  p.checkboxes.forEach((cb, value) => { cb.checked = values === null || values.has(value); });
  updateAllTriState(p);
}

function buildNumeric(p: OpenPopover, idBase: string): void {
  const { config } = p;
  const label = document.createElement('label');
  label.className = 'column-filter-numeric-label';
  label.textContent = 'Expression';
  const input = document.createElement('input');
  input.type = 'text';
  input.className = 'column-filter-numeric-input';
  input.placeholder = config.placeholder ?? 'e.g. >100, 50..200, 5';
  input.setAttribute('aria-describedby', `${idBase}-error`);
  label.appendChild(input);
  p.el.appendChild(label);

  const errorEl = document.createElement('div');
  errorEl.id = `${idBase}-error`;
  errorEl.className = 'column-filter-error';
  errorEl.setAttribute('role', 'status');
  p.el.appendChild(errorEl);
  p.input = input;
  p.errorEl = errorEl;

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
}

function buildCategorical(p: OpenPopover): void {
  const { config } = p;
  const display = config.displayLabel ?? defaultDisplay;
  const allLabel = document.createElement('label');
  allLabel.className = 'column-filter-all';
  const allBox = document.createElement('input');
  allBox.type = 'checkbox';
  allBox.dataset['role'] = 'all';
  allLabel.appendChild(allBox);
  const allText = document.createElement('span');
  allText.textContent = '(All)';
  allLabel.appendChild(allText);
  p.el.appendChild(allLabel);
  p.allBox = allBox;

  const list = document.createElement('div');
  list.className = 'column-filter-list';
  for (const value of config.distinctValues ?? []) {
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
    p.checkboxes.set(value, cb);
  }
  p.el.appendChild(list);

  const commit = (): void => {
    const selected: string[] = [];
    p.checkboxes.forEach((cb, value) => { if (cb.checked) selected.push(value); });
    updateAllTriState(p);
    config.onCommit(selected.length === p.checkboxes.size ? null : { kind: 'set', values: selected });
  };
  p.checkboxes.forEach((cb) => cb.addEventListener('change', commit));
  allBox.addEventListener('change', () => setAll(p, allBox.checked));
}

// Check or uncheck every value. Unchecking writes an explicit empty
// allow-list (show no rows), which is distinct from "no filter".
function setAll(p: OpenPopover, target: boolean): void {
  p.checkboxes.forEach((cb) => { cb.checked = target; });
  updateAllTriState(p);
  p.config.onCommit(target ? null : { kind: 'set', values: [] });
}

function buildPopover(config: ColumnFilterPopoverConfig): OpenPopover {
  const el = document.createElement('div');
  el.className = 'column-filter-popover';
  el.setAttribute('role', 'dialog');
  el.setAttribute('aria-modal', 'false');
  const idBase = `${config.scope}-column-filter-${config.column}`;
  el.setAttribute('aria-labelledby', `${idBase}-heading`);

  const heading = document.createElement('h3');
  heading.id = `${idBase}-heading`;
  heading.className = 'column-filter-heading';
  heading.textContent = `Filter ${config.label}`;
  el.appendChild(heading);

  const p: OpenPopover = { config, el, input: null, errorEl: null, checkboxes: new Map(), allBox: null };
  if (config.kind === 'numeric') buildNumeric(p, idBase);
  else buildCategorical(p);

  const footer = document.createElement('div');
  footer.className = 'column-filter-footer';
  const clearBtn = document.createElement('button');
  clearBtn.type = 'button';
  clearBtn.className = 'column-filter-clear';
  clearBtn.textContent = 'Clear';
  clearBtn.addEventListener('click', () => {
    if (p.input) {
      p.input.value = '';
      if (p.errorEl) p.errorEl.textContent = '';
      config.onCommit(null);
    } else {
      setAll(p, false);
    }
  });
  footer.appendChild(clearBtn);
  el.appendChild(footer);
  return p;
}

function onOutsideMouseDown(e: MouseEvent): void {
  if (!open) return;
  const target = e.target as Node | null;
  if (!target || open.el.contains(target)) return;
  // Trigger clicks are handled by openColumnFilterPopover (toggle / swap).
  if (target instanceof Element && target.closest('.column-filter-btn')) return;
  closeColumnFilterPopover();
}

function onKeyDown(e: KeyboardEvent): void {
  if (!open || e.key !== 'Escape') return;
  e.preventDefault();
  closeColumnFilterPopover(true);
}

function onResize(): void {
  if (!open) return;
  const trigger = open.config.findTrigger();
  if (!trigger) {
    closeColumnFilterPopover();
    return;
  }
  positionPopover(open.el, trigger);
}

function attachListeners(): void {
  if (listenersAttached) return;
  document.addEventListener('mousedown', onOutsideMouseDown);
  document.addEventListener('keydown', onKeyDown);
  window.addEventListener('resize', onResize);
  listenersAttached = true;
}

function detachListeners(): void {
  if (!listenersAttached) return;
  document.removeEventListener('mousedown', onOutsideMouseDown);
  document.removeEventListener('keydown', onKeyDown);
  window.removeEventListener('resize', onResize);
  listenersAttached = false;
}

// Forget a popover that something else removed from the DOM.
function dropIfDetached(): void {
  if (open && !open.el.isConnected) {
    open = null;
    detachListeners();
  }
}

/** Close the open popover, optionally returning focus to its trigger. */
export function closeColumnFilterPopover(restoreFocus = false): void {
  dropIfDetached();
  if (!open) return;
  const { el, config } = open;
  el.remove();
  open = null;
  detachListeners();
  const trigger = config.findTrigger();
  trigger?.setAttribute('aria-expanded', 'false');
  if (restoreFocus) trigger?.focus();
}

/**
 * Open the popover for `config.column` anchored to `anchor`. Clicking the
 * trigger of the already-open column closes it (focus returns to the
 * trigger); opening any other column replaces the open popover.
 */
export function openColumnFilterPopover(config: ColumnFilterPopoverConfig, anchor: HTMLElement): void {
  dropIfDetached();
  if (open) {
    const same = open.config.scope === config.scope && open.config.column === config.column;
    closeColumnFilterPopover(same);
    if (same) return;
  }
  const p = buildPopover(config);
  document.body.appendChild(p.el);
  open = p;
  resync(p);
  positionPopover(p.el, anchor);
  anchor.setAttribute('aria-expanded', 'true');
  attachListeners();
  const first = p.input ?? p.el.querySelector<HTMLInputElement>('input[type="checkbox"]');
  first?.focus();
}

/**
 * Call after a table re-render. If this table's popover is open, re-anchor
 * it to the fresh trigger and re-sync its controls; close it if the column
 * is gone.
 */
export function rebindColumnFilterPopover(scope: string): void {
  dropIfDetached();
  if (!open || open.config.scope !== scope) return;
  const trigger = open.config.findTrigger();
  if (!trigger) {
    closeColumnFilterPopover();
    return;
  }
  trigger.setAttribute('aria-expanded', 'true');
  positionPopover(open.el, trigger);
  resync(open);
}

/** Distinct values for a categorical column, '' first, then locale order. */
export function sortedDistinctValues<T>(rows: readonly T[], extract: (row: T) => string): string[] {
  const seen = new Set<string>();
  for (const row of rows) seen.add(extract(row));
  return Array.from(seen).sort((a, b) => {
    if (a === '' && b !== '') return -1;
    if (a !== '' && b === '') return 1;
    return a.localeCompare(b);
  });
}
