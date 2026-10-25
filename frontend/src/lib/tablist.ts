/**
 * Keyboard model for the in-page sub-tab lists (Inventory & Coverage,
 * Admin) that use the ARIA tab pattern (issue #730).
 *
 * A screen reader announces `role="tab"` as "tab, 2 of 4" and the user then
 * expects the WAI-ARIA Authoring Practices keyboard model:
 *   - the tab list is a single Tab stop (roving tabindex: the selected tab
 *     has tabindex="0", the others "-1");
 *   - Left/Right (Up/Down when aria-orientation="vertical") move focus to
 *     the previous/next tab, wrapping at the ends; Home/End jump to the
 *     first/last tab.
 *
 * Activation is manual: arrows only move focus and Enter/Space (the native
 * button click) selects. Selecting a sub-tab fetches its data and pushes a
 * history entry, so following focus would fire a request and a history
 * entry for every tab the user arrows past.
 *
 * Tabs hidden for the current user (`.admin-only` without `.visible`, or the
 * `hidden` attribute) are skipped.
 */

function isAvailable(tab: HTMLElement): boolean {
  if (tab.hidden || tab.closest('[hidden]')) return false;
  if ((tab as HTMLButtonElement).disabled) return false;
  return window.getComputedStyle(tab).display !== 'none';
}

/** The tabs of `tablist` the user can currently reach, in DOM order. */
function availableTabs(tablist: Element): HTMLElement[] {
  return Array.from(tablist.querySelectorAll<HTMLElement>('[role="tab"]')).filter(isAvailable);
}

/**
 * Make the selected tab the list's only Tab stop. Call after every change
 * of `aria-selected`. When no reachable tab is selected, the first
 * reachable one takes the stop so the list never drops out of the tab order.
 */
export function syncTabStops(tablist: Element | null): void {
  if (!tablist) return;
  const all = Array.from(tablist.querySelectorAll<HTMLElement>('[role="tab"]'));
  const available = all.filter(isAvailable);
  const stop =
    available.find((tab) => tab.getAttribute('aria-selected') === 'true') ?? available[0];
  all.forEach((tab) => tab.setAttribute('tabindex', tab === stop ? '0' : '-1'));
}

function targetIndex(key: string, index: number, count: number, vertical: boolean): number | undefined {
  const prev = vertical ? 'ArrowUp' : 'ArrowLeft';
  const next = vertical ? 'ArrowDown' : 'ArrowRight';
  switch (key) {
    case prev: return (index - 1 + count) % count;
    case next: return (index + 1) % count;
    case 'Home': return 0;
    case 'End': return count - 1;
    default: return undefined;
  }
}

/**
 * Wire arrow/Home/End focus movement on a `role="tablist"` element and set
 * its initial roving tabindex. Bind once per tablist element.
 */
export function wireTablistKeyboard(tablist: HTMLElement): void {
  syncTabStops(tablist);
  tablist.addEventListener('keydown', (e: KeyboardEvent) => {
    if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
    const current = (e.target as Element | null)?.closest<HTMLElement>('[role="tab"]');
    if (!current || !tablist.contains(current)) return;
    const tabs = availableTabs(tablist);
    const index = tabs.indexOf(current);
    if (index < 0) return;
    const vertical = tablist.getAttribute('aria-orientation') === 'vertical';
    const target = targetIndex(e.key, index, tabs.length, vertical);
    if (target === undefined) return;
    e.preventDefault();
    tabs[target]!.focus();
  });
}
