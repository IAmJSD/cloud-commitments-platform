/**
 * Mobile navigation bounded context: the hamburger drawer and the projection
 * of topbar regions into it below the drawer breakpoint (issue #1779).
 *
 * Public API:
 * - setupMobileNav() wires the drawer once at startup and returns a
 *   MobileNavHandle (or null when the page has no hamburger/sidebar).
 * - syncHeaderPlacement() moves the projected regions for a given width.
 * - MOBILE_NAV_QUERY is the breakpoint both of the above follow.
 *
 * Extracted from app.ts (issue #215); behavior is unchanged.
 */

/**
 * Drawer breakpoint. Must stay in lockstep with the `max-width: 768px` block
 * in styles/responsive.css, which lays these same regions out of the topbar.
 */
export const MOBILE_NAV_QUERY = '(max-width: 768px)';

/**
 * Handle returned by setupMobileNav. open and close are the same transitions
 * the hamburger, overlay and Escape key drive, focus moves included.
 */
export interface MobileNavHandle {
  open(): void;
  close(): void;
  isOpen(): boolean;
}

/**
 * Topbar regions with no room in the topbar below the breakpoint, listed in
 * the order they appear in the header so restoring them re-appends correctly.
 */
const DRAWER_PROJECTED_IDS = ['topbar-filters', 'user-info'] as const;

/**
 * Move the global filter chips and the account actions between the topbar
 * and the drawer (issue #1779). Relocation rather than duplication: the
 * chips and the logout/profile handlers are bound to these exact nodes, and
 * moving a node keeps its listeners and state.
 *
 * Idempotent, and a no-op when either endpoint is absent.
 */
export function syncHeaderPlacement(isNarrow: boolean): void {
  const topbar = document.querySelector<HTMLElement>('.app-topbar');
  const extras = document.getElementById('sidebar-extras');
  if (!topbar || !extras) return;

  const target = isNarrow ? extras : topbar;
  const elements = DRAWER_PROJECTED_IDS
    .map(id => document.getElementById(id))
    .filter((el): el is HTMLElement => el !== null);

  // Re-append all of them whenever any one is misplaced, so the restored
  // header keeps DRAWER_PROJECTED_IDS order. Appending an element that is
  // already in place would tear its subtree out and back in, dropping focus
  // and closing an open chip popover, so the whole pass is skipped instead.
  const misplaced = elements.some(el => el.parentElement !== target);
  if (!misplaced) return;
  for (const el of elements) target.appendChild(el);
}

/**
 * Wire the mobile navigation drawer (hamburger button + overlay + Escape).
 *
 * Activates below 768px (CSS hides the hamburger above that breakpoint).
 *
 * Open:  body.classList.add('sidebar-open')
 *        hamburger aria-expanded="true"
 *        sidebar aria-hidden="false", inert cleared
 *        Focus first focusable link in the sidebar
 *
 * Close: body.classList.remove('sidebar-open')
 *        hamburger aria-expanded="false"
 *        sidebar aria-hidden="true", inert set
 *        Return focus to the hamburger button
 *
 * A closed drawer needs both attributes and they always move together.
 * aria-hidden takes it out of the accessibility tree; inert takes it out of
 * sequential keyboard navigation, which neither aria-hidden nor the
 * off-screen transform affects. With only the first, tabbing walks into
 * controls that are invisible and expose no accessible name.
 *
 * Crossing the breakpoint in either direction is a third transition, neither
 * an open nor a close: see applyBreakpointState.
 *
 * Sidebar links also close the drawer (they navigate within the SPA).
 *
 * Returns a handle on the wired drawer, or null when the page has no
 * hamburger button or sidebar to wire. Call it once: every call binds its
 * own listeners.
 */
export function setupMobileNav(): MobileNavHandle | null {
  const hamburger = document.getElementById('hamburger-btn') as HTMLButtonElement | null;
  const sidebar = document.getElementById('sidebar') as HTMLElement | null;
  const overlay = document.getElementById('sidebar-overlay') as HTMLElement | null;
  if (!hamburger || !sidebar) return null;

  function openDrawer(): void {
    document.body.classList.add('sidebar-open');
    hamburger!.setAttribute('aria-expanded', 'true');
    sidebar!.setAttribute('aria-hidden', 'false');
    // Before the focus below, not after: focus into an inert subtree is
    // dropped silently and the caret would land on <body>.
    sidebar!.removeAttribute('inert');
    if (overlay) overlay.setAttribute('aria-hidden', 'false');

    // The desktop collapse toggle is display:none below the drawer
    // breakpoint, where focus() on it is a no-op, so skip it.
    const firstFocusable = sidebar!.querySelector<HTMLElement>(
      'a[href], button:not([disabled]):not(.app-sidebar-toggle), [tabindex]:not([tabindex="-1"])',
    );
    firstFocusable?.focus();
  }

  /**
   * `returnFocus: 'if-inside'` returns focus to the hamburger only when it is
   * still inside the drawer. A sidebar link click has by then moved it to the
   * new page's heading (issue #730), or a dirty-guard dialog holds it, and
   * pulling it back to the hamburger would undo either.
   */
  function closeDrawer(returnFocus: 'always' | 'if-inside' = 'always'): void {
    const focusInside = sidebar!.contains(document.activeElement);
    document.body.classList.remove('sidebar-open');
    hamburger!.setAttribute('aria-expanded', 'false');
    sidebar!.setAttribute('aria-hidden', 'true');
    // Before inert lands, which does not blur focus in Chromium.
    if (returnFocus === 'always' || focusInside) hamburger!.focus();
    sidebar!.setAttribute('inert', '');
    if (overlay) overlay.setAttribute('aria-hidden', 'true');
  }

  /**
   * Put the drawer into the closed state the current breakpoint calls for.
   * Runs on load and on every crossing, neither of which the user asked for,
   * so unlike openDrawer and closeDrawer it moves focus only to rescue it.
   *
   * Whether the sidebar is reachable at all is what the two widths disagree
   * about. Above the breakpoint it is the permanently visible navigation and
   * must stay both in the accessibility tree and in the tab order; below, it
   * is a drawer parked off-screen holding the controls syncHeaderPlacement
   * moves into it, and everything in there has to leave both with it.
   *
   * The narrow branch can never be hiding an open drawer: the only crossing
   * that reaches it comes from the desktop side, where nothing opens one.
   */
  function applyBreakpointState(isNarrow: boolean): void {
    // body.sidebar-open sets overflow:hidden outside the media query, so a
    // drawer open across a resize would lock scrolling with no drawer left.
    document.body.classList.remove('sidebar-open');
    hamburger!.setAttribute('aria-expanded', 'false');
    if (overlay) overlay.setAttribute('aria-hidden', 'true');

    if (!isNarrow) {
      sidebar!.removeAttribute('aria-hidden');
      sidebar!.removeAttribute('inert');
      return;
    }
    // Focus survives the crossing when it was already on a sidebar link, and
    // hiding the subtree under it strands the caret where no screen reader
    // can follow. Chromium does not blur it for us when inert lands, so this
    // has to run first.
    if (sidebar!.contains(document.activeElement)) hamburger!.focus();
    sidebar!.setAttribute('aria-hidden', 'true');
    sidebar!.setAttribute('inert', '');
  }

  hamburger.addEventListener('click', () => {
    const isOpen = document.body.classList.contains('sidebar-open');
    if (isOpen) {
      closeDrawer();
    } else {
      openDrawer();
    }
  });

  // Overlay click closes the drawer
  if (overlay) {
    overlay.addEventListener('click', () => closeDrawer());
  }

  // Escape key closes the drawer
  document.addEventListener('keydown', (e: KeyboardEvent) => {
    if (e.key === 'Escape' && document.body.classList.contains('sidebar-open')) {
      e.preventDefault();
      closeDrawer();
    }
  });

  // Projected topbar controls: place them for the current width, then follow
  // the breakpoint. Listeners are bound once here, never per relocation.
  const mediaQuery = window.matchMedia(MOBILE_NAV_QUERY);
  syncHeaderPlacement(mediaQuery.matches);
  applyBreakpointState(mediaQuery.matches);
  mediaQuery.addEventListener('change', event => {
    syncHeaderPlacement(event.matches);
    applyBreakpointState(event.matches);
  });

  // Account actions inside the drawer navigate or end the session, so they
  // close it. Delegated from the slot, which outlives every relocation; the
  // filter chips are deliberately excluded so picking a filter keeps the
  // drawer open for the next one.
  const extras = document.getElementById('sidebar-extras');
  extras?.addEventListener('click', (e: MouseEvent) => {
    const target = e.target as HTMLElement | null;
    if (!target?.closest('#user-info a, #user-info button:not(#user-email-display)')) return;
    if (document.body.classList.contains('sidebar-open')) closeDrawer();
  });
  // The profile button opens a modal. Close the drawer in the capture phase,
  // before the button's own handler runs: closing afterwards would pull focus
  // from the modal back to the hamburger. Closing first lets the handler
  // record the hamburger, not the now-inert button, as the return-focus trigger.
  extras?.addEventListener('click', (e: MouseEvent) => {
    if (!(e.target as HTMLElement | null)?.closest('#user-email-display')) return;
    if (document.body.classList.contains('sidebar-open')) closeDrawer();
  }, true);

  // Clicking any sidebar link closes the drawer (SPA navigation)
  sidebar.querySelectorAll<HTMLElement>('.tab-btn').forEach(link => {
    link.addEventListener('click', (e: MouseEvent) => {
      // Only close on unmodified left-click (same guard as tab switching above)
      if (
        e.button !== 0 ||
        e.metaKey ||
        e.ctrlKey ||
        e.shiftKey ||
        e.altKey
      ) {
        return;
      }
      if (document.body.classList.contains('sidebar-open')) {
        closeDrawer('if-inside');
      }
    });
  });

  return {
    open: openDrawer,
    close: () => closeDrawer(),
    isOpen: () => document.body.classList.contains('sidebar-open'),
  };
}
