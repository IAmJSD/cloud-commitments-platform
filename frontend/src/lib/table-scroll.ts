/**
 * Wide-table scroll affordances (issue #185).
 *
 * Wide tables scroll sideways inside their own box (issue #703), but nothing
 * told the user that columns were cut off on the right, and the Opportunities
 * table's only horizontal scrollbar sat at the bottom of a page-tall table,
 * under the sticky action box. This module adds three things:
 *
 *   - A fade on whichever edge of a table scroller has hidden columns
 *     (`.scroll-hint-left` / `.scroll-hint-right`), for every table on every
 *     page, so a clipped column reads as "more this way".
 *   - For the Opportunities table, a scrollbar strip that sticks just above
 *     the action box while the table is on screen and mirrors the table's
 *     horizontal scroll position both ways.
 *   - `--action-box-offset`, the action box's real rendered height (it wraps
 *     to two or three lines on narrow screens), which the list padding and
 *     the strip use, plus a focus handler that scrolls a keyboard-focused
 *     Opportunities row out from under the sticky topbar or action box
 *     (browsers only scroll focus into view when it is off-screen, and a
 *     row under a sticky bar counts as on-screen).
 *
 * jsdom has no layout, so the jest tests stub the size properties; the
 * Playwright spec tests-e2e/table-scroll-affordance.spec.ts covers the real
 * rendering.
 */

export const HINT_LEFT = 'scroll-hint-left';
export const HINT_RIGHT = 'scroll-hint-right';
export const STRIP_CLASS = 'table-scroll-strip';
const STRIPPED_TABLE = '#recommendations-list > table';
const ACTION_BOX_ID = 'recommendations-action-box';

function px(value: string): number {
  return parseFloat(value) || 0;
}

function setVar(el: HTMLElement, name: string, value: string): void {
  if (el.style.getPropertyValue(name) !== value) el.style.setProperty(name, value);
}

/** The element that scrolls a table sideways: the table itself when it is a
 * block scroller, else its direct container; null when neither scrolls. */
export function scrollerFor(table: HTMLTableElement): HTMLElement | null {
  for (const el of [table, table.parentElement]) {
    if (!el) continue;
    const overflowX = getComputedStyle(el).overflowX;
    if (overflowX === 'auto' || overflowX === 'scroll') return el;
  }
  return null;
}

/** Marks the edges of `el` that have content scrolled out of view. The
 * scrollbar sizes are exported so the fade's CSS mask can leave the
 * scrollbars themselves at full opacity. */
export function updateScrollHint(el: HTMLElement): void {
  const max = el.scrollWidth - el.clientWidth;
  const left = max > 1 && el.scrollLeft > 1;
  const right = max > 1 && el.scrollLeft < max - 1;
  el.classList.toggle(HINT_LEFT, left);
  el.classList.toggle(HINT_RIGHT, right);
  if (!left && !right) return;
  const cs = getComputedStyle(el);
  const sbWidth = el.offsetWidth - el.clientWidth - px(cs.borderLeftWidth) - px(cs.borderRightWidth);
  const sbHeight = el.offsetHeight - el.clientHeight - px(cs.borderTopWidth) - px(cs.borderBottomWidth);
  setVar(el, '--scroll-hint-sb-w', `${Math.max(0, sbWidth)}px`);
  setVar(el, '--scroll-hint-sb-h', `${Math.max(0, sbHeight)}px`);
}

function stripAfter(table: HTMLElement): HTMLElement | null {
  const next = table.nextElementSibling;
  return next instanceof HTMLElement && next.classList.contains(STRIP_CLASS) ? next : null;
}

/** Ensures the mirrored scrollbar strip right after `table` exists, spans the
 * same scroll range and sits at the same position. Hidden while the table
 * fits. The strip is aria-hidden: it duplicates the table's own scrolling,
 * which keyboard and assistive-technology users already reach. */
export function syncScrollStrip(table: HTMLElement): HTMLElement {
  let strip = stripAfter(table);
  if (!strip) {
    strip = document.createElement('div');
    strip.className = STRIP_CLASS;
    strip.setAttribute('aria-hidden', 'true');
    strip.tabIndex = -1;
    strip.appendChild(document.createElement('div'));
    table.after(strip);
  }
  const overflow = table.scrollWidth - table.clientWidth;
  const hidden = overflow <= 1;
  if (strip.hidden !== hidden) strip.hidden = hidden;
  if (hidden) return strip;
  const spacer = strip.firstElementChild as HTMLElement;
  // Same scroll range as the table, whatever the strip's own width.
  const width = `${strip.clientWidth + overflow}px`;
  if (spacer.style.width !== width) spacer.style.width = width;
  if (strip.scrollLeft !== table.scrollLeft) strip.scrollLeft = table.scrollLeft;
  return strip;
}

/** True while at least a sliver of `anchor` is on screen: inside the
 * viewport and inside every ancestor that clips its overflow (a table
 * scroller, a card). Header filter popovers use it to follow their button
 * through a scroll instead of closing: clicking a funnel at a scroller's
 * clipped edge scrolls the table (and its mirrored strip one frame later),
 * and those scroll events used to close the popover the click had just
 * opened. A zero-size rect (not rendered) counts as out of view. */
const CLIPPING = new Set(['hidden', 'clip', 'auto', 'scroll']);

export function anchorInView(anchor: HTMLElement): boolean {
  const rect = anchor.getBoundingClientRect();
  if (rect.width <= 0 || rect.height <= 0) return false;
  let left = Math.max(rect.left, 0);
  let right = Math.min(rect.right, window.innerWidth);
  let top = Math.max(rect.top, 0);
  let bottom = Math.min(rect.bottom, window.innerHeight);
  for (let el = anchor.parentElement; el && el !== document.body; el = el.parentElement) {
    const cs = getComputedStyle(el);
    if (!CLIPPING.has(cs.overflowX) && !CLIPPING.has(cs.overflowY)) continue;
    const box = el.getBoundingClientRect();
    left = Math.max(left, box.left);
    right = Math.min(right, box.right);
    top = Math.max(top, box.top);
    bottom = Math.min(bottom, box.bottom);
  }
  return right - left >= 1 && bottom - top >= 1;
}

/** Re-measures every table scroller, the Opportunities strip and the action
 * box offset. Cheap enough to run once per animation frame after a change. */
export function refreshTableScroll(root: ParentNode = document): void {
  root.querySelectorAll<HTMLTableElement>('table').forEach((table) => {
    const scroller = scrollerFor(table);
    if (scroller) updateScrollHint(scroller);
  });
  root.querySelectorAll<HTMLElement>(STRIPPED_TABLE).forEach((table) => {
    syncScrollStrip(table);
  });
  const box = document.getElementById(ACTION_BOX_ID);
  if (box && box.offsetHeight > 0) {
    setVar(document.documentElement, '--action-box-offset', `${box.offsetHeight}px`);
  }
}

/** Keeps a strip and its table scrolled to the same position, and refreshes
 * the edge fade of whatever scrolled. Exported for the unit tests. */
export function handleScroll(target: EventTarget | null): void {
  if (!(target instanceof HTMLElement)) return;
  if (target.classList.contains(STRIP_CLASS)) {
    const table = target.previousElementSibling;
    if (table instanceof HTMLElement && table.scrollLeft !== target.scrollLeft) {
      table.scrollLeft = target.scrollLeft;
    }
    return;
  }
  if (target.matches(STRIPPED_TABLE)) {
    const strip = stripAfter(target);
    if (strip && strip.scrollLeft !== target.scrollLeft) strip.scrollLeft = target.scrollLeft;
  }
  if (target.classList.contains(HINT_LEFT) || target.classList.contains(HINT_RIGHT)) {
    updateScrollHint(target);
  }
}

const FOCUS_MARGIN = 16;

/** A mouse click on a half-hidden checkbox should not jolt the page, so only
 * keyboard focus (`:focus-visible`) is scrolled clear. */
function isKeyboardFocus(el: HTMLElement): boolean {
  try {
    return el.matches(':focus-visible');
  } catch {
    return true; // engines without :focus-visible support
  }
}

/** Scrolls the page so a focused control inside the Opportunities list is
 * not hidden under the sticky topbar, the strip or the action box.
 * Exported for the unit tests. */
export function keepFocusClear(target: EventTarget | null): void {
  if (!(target instanceof HTMLElement) || !target.closest('#recommendations-list')) return;
  if (!isKeyboardFocus(target)) return;
  const rect = target.getBoundingClientRect();
  const topbar = document.querySelector<HTMLElement>('.app-topbar');
  const top = topbar ? topbar.getBoundingClientRect().bottom : 0;
  let bottom = window.innerHeight;
  const box = document.getElementById(ACTION_BOX_ID);
  if (box && box.offsetHeight > 0) bottom = Math.min(bottom, box.getBoundingClientRect().top);
  const strip = document.querySelector<HTMLElement>(`${STRIPPED_TABLE} + .${STRIP_CLASS}`);
  if (strip && !strip.hidden) bottom = Math.min(bottom, strip.getBoundingClientRect().top);
  if (rect.bottom > bottom) {
    window.scrollBy(0, rect.bottom - bottom + FOCUS_MARGIN);
  } else if (rect.top < top) {
    window.scrollBy(0, rect.top - top - FOCUS_MARGIN);
  }
}

let initialized = false;

/** Wires the affordances for the app's lifetime: refresh after DOM changes
 * (tables are re-rendered wholesale), on resize, and on every scroll. */
export function initTableScroll(): void {
  if (initialized) return;
  initialized = true;
  let queued = false;
  const schedule = (): void => {
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => {
      queued = false;
      refreshTableScroll();
    });
  };
  document.addEventListener('scroll', (e) => handleScroll(e.target), { capture: true, passive: true });
  document.addEventListener('focusin', (e) => keepFocusClear(e.target));
  window.addEventListener('resize', schedule);
  // class/hidden catch tab switches and the sidebar collapsing, both of
  // which change a table's width without touching its children.
  new MutationObserver(schedule).observe(document.body, {
    childList: true,
    subtree: true,
    attributes: true,
    attributeFilter: ['class', 'hidden'],
  });
  schedule();
}
