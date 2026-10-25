/**
 * Accessible dialog semantics for the sign-in gates rendered by auth.ts:
 * login, first-admin setup and password reset (#731).
 *
 * These are built with innerHTML and appended to <body>, so they never got
 * the markup-level `role="dialog"` the static modals in index.html carry,
 * and focus stayed on <body> behind the overlay. openAuthDialog names the
 * dialog after its heading and hands it to the shared openModal helper,
 * which moves focus inside and traps Tab.
 *
 * The gates are not dismissible: nothing behind them works until the user
 * signs in. `data-require-ack` makes closeModal (and so Escape) a no-op;
 * auth.ts removes the element itself once the flow completes.
 */

import { openModal } from './modal';

export function openAuthDialog(modal: HTMLElement, initialFocus?: string): void {
  // The role goes on the visible card, not the zero-size wrapper or the
  // full-screen overlay; the focus trap goes on the wrapper, which survives
  // the innerHTML re-renders of the reset flow.
  const dialog = modal.querySelector<HTMLElement>('.modal-content') ?? modal;
  dialog.setAttribute('role', 'dialog');
  dialog.setAttribute('aria-modal', 'true');
  const heading = dialog.querySelector('h2');
  if (heading) {
    if (!heading.id) heading.id = `${modal.id}-title`;
    dialog.setAttribute('aria-labelledby', heading.id);
  }
  modal.dataset['requireAck'] = 'true';
  openModal(modal, initialFocus ? { initialFocus } : undefined);
}
