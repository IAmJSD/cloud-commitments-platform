/**
 * Admin -> Purchasing policies section links (issue #190).
 *
 * The panel is one long form (global defaults, lookback, grace period, RI
 * class, approvals, per-provider service defaults, exchange automation,
 * laddering). The #purchasing-section-nav row in index.html holds plain
 * `#id` anchors to each section. This module:
 *   - scrolls to the section without touching the URL hash (a hash change
 *     would fire popstate and run the SPA router);
 *   - opens a collapsed per-provider service grid when its link is used;
 *   - hides links whose section is hidden or empty (disabled provider,
 *     exchange automation hidden for a caller without access, a dynamic
 *     section that has not rendered), kept in sync by a MutationObserver
 *     so settings.ts / riexchange.ts / ladder.ts need no extra calls.
 */

const NAV_ID = 'purchasing-section-nav';
const PANEL_ID = 'purchasing-panel';

let observer: MutationObserver | null = null;

/** The section a nav link points at, or null for a malformed/missing target. */
function targetOf(link: HTMLAnchorElement): HTMLElement | null {
  const href = link.getAttribute('href') ?? '';
  if (!href.startsWith('#') || href.length < 2) return null;
  return document.getElementById(href.slice(1));
}

/** A section is linkable when it is in the DOM, not hidden, and has content. */
export function isSectionAvailable(section: HTMLElement | null): boolean {
  if (!section) return false;
  if (section.closest('.hidden')) return false;
  return section.childElementCount > 0;
}

/** Show exactly the links whose section the user can currently see. */
export function refreshPurchasingNav(): void {
  const nav = document.getElementById(NAV_ID);
  if (!nav) return;
  nav.querySelectorAll<HTMLAnchorElement>('a[href^="#"]').forEach((link) => {
    const hidden = !isSectionAvailable(targetOf(link));
    if (link.hidden !== hidden) link.hidden = hidden;
  });
}

/** Scroll to a section, opening any collapsed service grid inside it first. */
export function jumpToSection(section: HTMLElement): void {
  section.querySelectorAll('details.service-defaults-details:not([open])').forEach((d) => {
    (d as HTMLDetailsElement).open = true;
  });
  section.scrollIntoView({ behavior: 'smooth', block: 'start' });
  // Move focus for keyboard and screen-reader users without a second jump.
  if (!section.hasAttribute('tabindex')) section.setAttribute('tabindex', '-1');
  section.focus({ preventScroll: true });
}

/**
 * Wire the link row once. Safe to call repeatedly: the click handler is a
 * single delegated listener marked on the nav element, and the observer is
 * only created once.
 */
export function initPurchasingNav(): void {
  const nav = document.getElementById(NAV_ID);
  if (!nav) return;

  if (nav.dataset['wired'] !== '1') {
    nav.addEventListener('click', (e) => {
      const me = e as MouseEvent;
      // Let modified clicks (new tab/window) behave like normal links.
      if (me.button !== 0 || me.ctrlKey || me.metaKey || me.shiftKey || me.altKey) return;
      const link = (e.target as HTMLElement | null)?.closest<HTMLAnchorElement>('a[href^="#"]');
      if (!link) return;
      const section = targetOf(link);
      if (!section) return;
      e.preventDefault();
      jumpToSection(section);
    });
    nav.dataset['wired'] = '1';
  }

  const panel = document.getElementById(PANEL_ID);
  if (panel && !observer && typeof MutationObserver !== 'undefined') {
    observer = new MutationObserver(() => refreshPurchasingNav());
    observer.observe(panel, { subtree: true, childList: true, attributes: true, attributeFilter: ['class'] });
  }
  refreshPurchasingNav();
}

/** Test hook: drop the observer so each test starts clean. */
export function resetPurchasingNavForTests(): void {
  observer?.disconnect();
  observer = null;
}
