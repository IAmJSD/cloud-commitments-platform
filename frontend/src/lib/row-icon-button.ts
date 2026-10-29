/**
 * Compact icon-only row action for the Admin account and registration
 * tables, matching the Users / Groups / API keys rows (.icon-btn).
 *
 * The accessible name comes from aria-label and the tooltip from title.
 * The short verb is also kept as visually hidden text so code and tests
 * that find a row action by its text ("Delete", "Overrides") keep working.
 */
import { createIcon, type IconName } from '../icons';

export interface RowIconButtonOptions {
  icon: IconName;
  /** Short verb kept as visually hidden text, e.g. "Delete". */
  text: string;
  /** Tooltip, e.g. "Delete account". */
  title: string;
  /** Accessible name naming the row, e.g. "Delete Prod (1234)". */
  ariaLabel: string;
  /** Extra classes, e.g. the danger variant. */
  className?: string;
}

export function rowIconButton(opts: RowIconButtonOptions): HTMLButtonElement {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = `btn btn-small icon-btn${opts.className ? ` ${opts.className}` : ''}`;
  btn.title = opts.title;
  btn.setAttribute('aria-label', opts.ariaLabel);
  btn.appendChild(createIcon(opts.icon));
  const text = document.createElement('span');
  text.className = 'sr-only';
  text.textContent = opts.text;
  btn.appendChild(text);
  return btn;
}
