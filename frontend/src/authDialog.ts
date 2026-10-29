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

const SVG_NS = 'http://www.w3.org/2000/svg';

/**
 * Decorative CUDly logo + name shown above the card on the signed-out
 * screens. aria-hidden: the dialog's heading already names the screen.
 */
function buildAuthBrand(): HTMLElement {
  const brand = document.createElement('div');
  brand.className = 'auth-brand';
  brand.setAttribute('aria-hidden', 'true');

  const mark = document.createElementNS(SVG_NS, 'svg');
  mark.setAttribute('class', 'auth-brand-mark');
  mark.setAttribute('viewBox', '0 0 24 24');
  mark.setAttribute('width', '28');
  mark.setAttribute('height', '28');
  mark.setAttribute('fill', 'none');
  mark.setAttribute('stroke', 'currentColor');
  mark.setAttribute('stroke-width', '2');
  mark.setAttribute('stroke-linecap', 'round');
  mark.setAttribute('stroke-linejoin', 'round');
  const path = document.createElementNS(SVG_NS, 'path');
  path.setAttribute('d', 'M18 10h-1.26A8 8 0 1 0 9 20h9a5 5 0 0 0 0-10z');
  mark.appendChild(path);

  const name = document.createElement('span');
  name.className = 'auth-brand-name';
  name.textContent = 'CUDly';

  const tagline = document.createElement('span');
  tagline.className = 'auth-brand-tagline';
  tagline.textContent = 'Cloud commitment optimizer';

  brand.append(mark, name, tagline);
  return brand;
}

export function openAuthDialog(modal: HTMLElement, initialFocus?: string): void {
  // Signed-out screens render as a standalone page: shell.css hides the app
  // chrome while an .auth-screen is in the document and centres the card on
  // a plain branded background, with the logo above it.
  modal.classList.add('auth-screen');
  const overlay = modal.querySelector<HTMLElement>('.modal-overlay');
  if (overlay && !overlay.querySelector(':scope > .auth-brand')) {
    overlay.prepend(buildAuthBrand());
  }

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
