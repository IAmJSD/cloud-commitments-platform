/**
 * Accessible modal helpers — keyboard focus trap, ESC-to-close, and focus
 * restoration for any element used as a dialog. ARIA semantics
 * (`role="dialog"`, `aria-modal="true"`) live in the markup; this file is
 * the keyboard layer that the original ARIA fix deliberately deferred
 * (issue #56).
 *
 * Usage:
 *   openModal(modalEl);          // shows + traps + focuses first focusable
 *   openModal(modalEl, { initialFocus: '#some-input' });
 *   openModal(modalEl, { onClose: () => discardCallerState() }); // runs on EVERY close path (issue #331)
 *   closeModal(modalEl);         // hides + releases trap + restores focus + runs onClose
 *
 * A modal holding a one-time secret sets `data-require-ack="true"` so
 * Escape does not dismiss it; the caller removes the attribute once the user
 * acknowledges; until then closeModal is a no-op.
 *
 * The helper toggles the existing `.hidden` CSS class so it slots into the
 * established show/hide convention without touching the stylesheet.
 *
 * Header close button: every dialog header carries a close-X marked with
 * `data-modal-dismiss` (see modalCloseButtonHtml / createModalCloseButton).
 * Its value names the dialog's own Cancel/Close button, which the X clicks,
 * so the X runs exactly the same close path (state resets, confirmation
 * guards, disabled-while-submitting) as that button. An empty value closes
 * the dialog through closeModal, the same path as Escape.
 */

import { createIcon, iconSvg } from './icons';

/**
 * Single source of truth for "what counts as a focusable element inside a
 * modal". Exported so the integration test in `modal.test.ts` can import
 * the same selector instead of duplicating the string and silently drifting
 * if the canonical list changes.
 */
export const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',');

interface ModalState {
  trigger: Element | null;
  keydownHandler: (e: KeyboardEvent) => void;
  clickHandler: (e: MouseEvent) => void;
  onClose?: () => void;
}

/** Marks a dialog header's close-X; the value is the id of the button it stands in for. */
const DISMISS_ATTR = 'data-modal-dismiss';
// The header close-X is reachable by Tab but is not where focus lands on
// open: the dialog's first field or action is.
const HEADER_CLOSE_SELECTOR = `[${DISMISS_ATTR}], .modal-header .modal-close`;

/**
 * Markup for a dialog header's close-X, for template-literal dialogs.
 * `dismissTargetId` is the id of the dialog's Cancel/Close button the X
 * clicks; omit it to close through closeModal (the Escape path).
 */
export function modalCloseButtonHtml(dismissTargetId = '', label = 'Close'): string {
  return `<button type="button" class="modal-close" ${DISMISS_ATTR}="${dismissTargetId}" aria-label="${label}" title="${label}">${iconSvg('x')}</button>`;
}

/** DOM version of modalCloseButtonHtml. */
export function createModalCloseButton(dismissTargetId = '', label = 'Close'): HTMLButtonElement {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'modal-close';
  btn.setAttribute(DISMISS_ATTR, dismissTargetId);
  btn.setAttribute('aria-label', label);
  btn.title = label;
  btn.appendChild(createIcon('x'));
  return btn;
}

/**
 * A dialog header: the title plus, unless `closable` is false (dialogs that
 * must be acknowledged), the close-X.
 */
export function createModalHeader(heading: HTMLElement, opts: { dismissTargetId?: string; closable?: boolean; closeLabel?: string } = {}): HTMLDivElement {
  const header = document.createElement('div');
  header.className = 'modal-header';
  header.appendChild(heading);
  if (opts.closable !== false) header.appendChild(createModalCloseButton(opts.dismissTargetId, opts.closeLabel));
  return header;
}

// WeakMap so a removed modal element doesn't leak its handler reference.
// Avoids stomping on `dataset` (which would survive in the DOM after
// closeModal and confuse anything that inspects it).
const openModals = new WeakMap<HTMLElement, ModalState>();

/**
 * Whether `el` is currently visible enough to focus. We deliberately
 * avoid `offsetParent` because jsdom (and some headless layout cases)
 * returns null for elements that are perfectly visible to a real user.
 * Instead we walk ancestors looking for an explicit hide signal —
 * `hidden`, `display: none`, `visibility: hidden`, or the project's
 * `.hidden` utility class. This stays correct in real browsers while
 * letting jsdom-based tests find focusables inside our modals.
 */
function isVisible(el: HTMLElement): boolean {
  let node: HTMLElement | null = el;
  while (node) {
    if (node.hidden) return false;
    if (node.classList.contains('hidden')) return false;
    const style = getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden') return false;
    node = node.parentElement;
  }
  return true;
}

function getFocusables(root: HTMLElement): HTMLElement[] {
  const nodes = Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR));
  return nodes.filter(n => !n.hasAttribute('disabled') && isVisible(n));
}

function resolveInitialFocus(
  el: HTMLElement,
  initialFocus: HTMLElement | string | undefined,
  focusables: HTMLElement[],
): HTMLElement | null {
  if (initialFocus) {
    const target =
      typeof initialFocus === 'string'
        ? el.querySelector<HTMLElement>(initialFocus)
        : initialFocus;
    if (target) return target;
  }
  return focusables.find(f => !f.matches(HEADER_CLOSE_SELECTOR)) ?? focusables[0] ?? null;
}

/**
 * Show a modal: clear `.hidden`, install the focus trap + ESC handler,
 * record the previously focused element so closeModal can restore it,
 * then focus either `opts.initialFocus` or the first focusable inside.
 *
 * Calling openModal twice on the same element is safe — the previous
 * trap is torn down before the new one wires up, so the trigger
 * recorded for restoration matches the most recent open() call.
 */
export function openModal(
  el: HTMLElement,
  opts?: { initialFocus?: HTMLElement | string; onClose?: () => void },
): void {
  // Re-opening: tear down the prior trap so we don't stack listeners
  // and so the recorded trigger reflects the latest opener.
  if (openModals.has(el)) {
    teardown(el);
  }

  const trigger = document.activeElement;

  el.classList.remove('hidden');

  const keydownHandler = (e: KeyboardEvent): void => {
    if (e.key === 'Escape') {
      e.preventDefault();
      closeModal(el);
      return;
    }

    if (e.key !== 'Tab') return;

    // Re-query on each Tab — the modal body can change (e.g. a form
    // toggles a fieldset visible) between open and the user's tab.
    const focusables = getFocusables(el);
    if (focusables.length === 0) {
      e.preventDefault();
      return;
    }

    const first = focusables[0]!;
    const last = focusables[focusables.length - 1]!;
    const active = document.activeElement as HTMLElement | null;

    // Wrap when the active element is at (or has slipped past) the edge.
    // Checking active not in focusables covers the case where focus
    // somehow escaped the modal subtree — pull it back to a sensible end.
    if (e.shiftKey) {
      if (active === first || !el.contains(active)) {
        e.preventDefault();
        last.focus();
      }
    } else {
      if (active === last || !el.contains(active)) {
        e.preventDefault();
        first.focus();
      }
    }
  };

  // Header close-X: stand in for the dialog's own Cancel/Close button.
  const clickHandler = (e: MouseEvent): void => {
    const btn = (e.target as Element | null)?.closest?.(`[${DISMISS_ATTR}]`);
    if (!btn || !el.contains(btn)) return;
    const targetId = btn.getAttribute(DISMISS_ATTR);
    const target = targetId ? document.getElementById(targetId) : null;
    if (target) target.click();
    else closeModal(el);
  };

  el.addEventListener('keydown', keydownHandler);
  el.addEventListener('click', clickHandler);
  openModals.set(el, { trigger, keydownHandler, clickHandler, onClose: opts?.onClose });

  const focusables = getFocusables(el);
  const initialTarget = resolveInitialFocus(el, opts?.initialFocus, focusables);
  initialTarget?.focus();
}

/**
 * Hide a modal: add `.hidden`, detach the trap handler, and restore
 * focus to the element that triggered openModal. No-op (apart from
 * adding `.hidden`) if the element wasn't opened via openModal — keeps
 * legacy code paths that call closeModal directly from a callback safe.
 */
export function closeModal(el: HTMLElement): void {
  if (el.dataset.requireAck === 'true') return;
  const state = openModals.get(el);
  teardown(el);
  el.classList.add('hidden');

  // Runs on every close path -- Escape, an explicit close button that
  // routes through this function, or any future path -- so a caller-
  // supplied cleanup (e.g. discarding purchase-specific state) can never
  // be skipped by one of them (issue #331).
  state?.onClose?.();

  const trigger = state?.trigger as HTMLElement | null | undefined;
  if (trigger && typeof trigger.focus === 'function' && document.contains(trigger)) {
    trigger.focus();
  }
}

function teardown(el: HTMLElement): void {
  const state = openModals.get(el);
  if (!state) return;
  el.removeEventListener('keydown', state.keydownHandler);
  el.removeEventListener('click', state.clickHandler);
  openModals.delete(el);
}
