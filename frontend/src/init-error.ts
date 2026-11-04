/**
 * Visible failure state for app start-up (issue #206).
 *
 * index.html ships with the Home panel pre-rendered and marked active. If
 * init() throws before switchTab() runs, that markup is all the user sees: a
 * Home dashboard with empty charts, under whatever URL they asked for, with
 * no event listeners bound. That looks like a routing bug and fixes itself on
 * the next (full-page) click, so it is nearly unreportable. Replacing the
 * panels with an explicit error card makes the failure visible instead.
 */

import { buildAlertIcon } from './lib/error-callout';

export const INIT_ERROR_ID = 'init-error';
export const INIT_ERROR_TITLE = 'CUDly — Could not load';

/** Best-effort, human-readable detail for the error card. */
function describeError(error: unknown): string {
  const err = error as { status?: unknown; message?: unknown } | null | undefined;
  const message = typeof err?.message === 'string' ? err.message.trim() : '';
  const status = typeof err?.status === 'number' && err.status > 0 ? err.status : undefined;
  if (status !== undefined && message && message !== `HTTP ${status}`) {
    return `HTTP ${status}: ${message}`;
  }
  if (message) return message;
  if (status !== undefined) return `HTTP ${status}`;
  return 'Unknown error';
}

/**
 * Hide every tab panel and show an error card with a "Try again" button in
 * the main content area. Safe to call more than once; a later call replaces
 * the earlier card.
 *
 * Retry is a full reload rather than a second init() call: init() may have
 * bound some listeners before it threw, and running it again would bind them
 * twice. The sidebar links are plain anchors at this point, so they also
 * reload, and either path re-runs init() from a clean page.
 */
export function renderInitError(
  error: unknown,
  reload: () => void = () => window.location.reload(),
): void {
  const main = document.getElementById('main-content');
  if (!main) return;

  document.querySelectorAll<HTMLElement>('.tab-content').forEach(panel => {
    panel.classList.remove('active');
  });

  const card = document.createElement('section');
  card.id = INIT_ERROR_ID;
  card.className = 'empty-state init-error-card';
  card.setAttribute('role', 'alert');

  const heading = document.createElement('h3');
  heading.textContent = 'CUDly could not finish loading';

  const body = document.createElement('p');
  body.textContent =
    'Something went wrong while loading this page, so it is not showing your data. ' +
    'Try again; if the problem continues, contact your administrator.';

  const detail = document.createElement('p');
  detail.className = 'text-muted mt-2';
  detail.textContent = `Details: ${describeError(error)}`;

  const retry = document.createElement('button');
  retry.type = 'button';
  retry.className = 'btn btn-primary mt-3';
  retry.textContent = 'Try again';
  retry.addEventListener('click', () => reload());

  const icon = buildAlertIcon();
  icon.classList.add('init-error-icon');

  card.append(icon, heading, body, detail, retry);

  document.getElementById(INIT_ERROR_ID)?.remove();
  main.prepend(card);
  document.title = INIT_ERROR_TITLE;
}
