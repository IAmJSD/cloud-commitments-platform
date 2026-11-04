/**
 * Sub-tab bars (Inventory & Coverage, Admin) on narrow screens.
 *
 * The bars scroll sideways when their tabs do not fit, but nothing showed
 * that more tabs were hidden ("Users, roles & API keys" sat off-screen on a
 * phone). This reuses the wide-table edge fade (lib/table-scroll.ts:
 * `.scroll-hint-left` / `.scroll-hint-right`) on each bar, and keeps the
 * selected tab scrolled into view whenever the selection changes, including
 * a deep link straight to the last tab.
 */

import { updateScrollHint } from '../lib/table-scroll';

export const SUBTAB_BAR_SELECTOR = '.settings-tabs[role="tablist"], .inventory-subnav[role="tablist"]';

/**
 * Scroll `bar` horizontally (never the page) so its selected tab is visible.
 * A cut-off tab is brought to the bar's start edge (less its scroll padding),
 * which is also where the bar's scroll snapping would settle, so the snap
 * cannot pull the tab back out of view.
 */
export function revealSelectedTab(bar: HTMLElement): void {
  const tab = bar.querySelector<HTMLElement>('[aria-selected="true"]');
  const max = bar.scrollWidth - bar.clientWidth;
  if (!tab || max <= 0) return;
  const start = tab.offsetLeft - bar.offsetLeft;
  const end = start + tab.offsetWidth;
  if (start >= bar.scrollLeft && end <= bar.scrollLeft + bar.clientWidth) return;
  const pad = parseFloat(getComputedStyle(bar).scrollPaddingLeft) || 0;
  bar.scrollLeft = Math.max(0, Math.min(max, start - pad));
}

let initialized = false;

/** Wire every sub-tab bar once for the app's lifetime. */
export function initSubtabScroll(root: ParentNode = document): void {
  if (initialized) return;
  initialized = true;
  const bars = Array.from(root.querySelectorAll<HTMLElement>(SUBTAB_BAR_SELECTOR));
  const refresh = (bar: HTMLElement): void => {
    revealSelectedTab(bar);
    updateScrollHint(bar);
  };
  for (const bar of bars) {
    bar.addEventListener('scroll', () => updateScrollHint(bar), { passive: true });
    new MutationObserver(() => refresh(bar)).observe(bar, {
      subtree: true,
      attributes: true,
      attributeFilter: ['aria-selected'],
    });
    // Also fires when a hidden page's bar is first shown (0 -> real width).
    if (typeof ResizeObserver !== 'undefined') {
      new ResizeObserver(() => refresh(bar)).observe(bar);
    }
    refresh(bar);
  }
}

/** Test hook: allow initSubtabScroll to run again on a fresh document. */
export function resetSubtabScrollForTests(): void {
  initialized = false;
}
