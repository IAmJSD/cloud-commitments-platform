/**
 * Toast notification system (AutoSpotting-style bottom-right stack).
 *
 * Usage:
 *   import { showToast } from './toast';
 *   showToast({ message: 'Saved', kind: 'success' });
 *   const { dismiss } = showToast({ message: 'Uploading...', timeout: null });
 *   // later:
 *   dismiss();
 *
 * Defaults: 30s timeout across all kinds (matches the user's spec). Pass
 * `timeout: null` for a sticky toast that stays until × clicked, or
 * `timeout: 5000` for short confirms. Toasts stack bottom-up, newest at
 * the bottom.
 *
 * Dismissal: × button click, timeout expiry, or Esc while the toast has
 * focus. The countdown pauses while the pointer is over the toast or
 * focus is inside it, and restarts with the time that was left when both
 * leave, so a toast being read or acted on never vanishes (WCAG 2.2.1,
 * issue #728). The dismiss() return value lets callers clear a toast
 * programmatically (e.g. tie an "uploading..." toast to the completion
 * of the upload).
 *
 * Open dialogs (issue #742): a toast stays up for 30s, and the default
 * bottom-right corner is where a dialog's Cancel / Save row sits on a
 * laptop-sized viewport, so an error toast used to swallow the click on
 * Save until it was dismissed. layoutToastStack() moves the stack only when
 * its default corner would cover a visible button row of the topmost open
 * dialog: beside the dialog when the gap to its right is wide enough for a
 * readable toast, otherwise above the dialog or its button row. The
 * positions are CSS custom properties set through the CSSOM (CSP forbids
 * style attributes in markup).
 */

import { createIcon, type IconName } from './icons';

export type ToastKind = 'success' | 'info' | 'warning' | 'error';

export interface ToastAction {
  label: string;
  onClick: () => void;
}

export interface ToastOptions {
  message: string;
  kind?: ToastKind;
  /** ms; null/0 = sticky. Defaults to 30_000. */
  timeout?: number | null;
  actions?: ToastAction[];
}

export interface ToastHandle {
  dismiss: () => void;
}

const DEFAULT_TIMEOUT_MS = 30_000;
const CONTAINER_ID = 'toast-container';

const KIND_ICONS: Record<ToastKind, IconName> = {
  success: 'check-circle',
  info: 'info',
  warning: 'alert-triangle',
  error: 'x-circle',
};

// Dialog boxes the stack must not cover, and the button rows inside them
// that must stay clickable.
const DIALOG_BOX_SELECTOR = '.modal-content, .modal-confirm';
const DIALOG_FOOTER_SELECTOR = '.modal-footer, .modal-buttons, .modal-confirm-actions';
// Matches the container's 1rem inset and 400px max-width in components.css.
const EDGE_PX = 16;
const DEFAULT_MAX_WIDTH_PX = 400;
const FOOTER_GAP_PX = 8;
// Narrower than this, a toast beside the dialog is too cramped to read, so
// the stack is lifted above the dialog (or its button row) instead.
const MIN_SIDE_WIDTH_PX = 240;

interface Box { left: number; top: number; right: number; bottom: number }

function setVar(el: HTMLElement, name: string, value: string | null): void {
  if (value === null) el.style.removeProperty(name);
  else if (el.style.getPropertyValue(name) !== value) el.style.setProperty(name, value);
}

function visibleRect(el: Element): DOMRect | null {
  const rect = el.getBoundingClientRect();
  return rect.width > 0 && rect.height > 0 ? rect : null;
}

function intersect(a: Box, b: Box): Box | null {
  const box = {
    left: Math.max(a.left, b.left),
    top: Math.max(a.top, b.top),
    right: Math.min(a.right, b.right),
    bottom: Math.min(a.bottom, b.bottom),
  };
  return box.right > box.left && box.bottom > box.top ? box : null;
}

/**
 * The dialog the user is looking at: a confirm dialog (z-index 2000) sits
 * over any .modal (1000), and among equals the later one in the DOM paints
 * on top. Dialogs underneath are behind a backdrop and can't be clicked.
 */
function topmostDialog(): { el: Element; rect: DOMRect } | null {
  let top: { el: Element; rect: DOMRect; rank: number } | null = null;
  for (const el of Array.from(document.querySelectorAll(DIALOG_BOX_SELECTOR))) {
    const rect = visibleRect(el);
    const rank = el.classList.contains('modal-confirm') ? 1 : 0;
    if (rect && (!top || rank >= top.rank)) top = { el, rect, rank };
  }
  return top;
}

/**
 * Keep the toast stack off the topmost open dialog's button rows. The stack
 * stays in its default corner unless that corner overlaps a button row that
 * is actually on screen. Then it moves, in order of preference: beside the
 * dialog (when the gap is at least 240px), above the whole dialog (when
 * there is room), or just above the button row it would cover. The last
 * case can put the stack over the lower part of a dialog that fills the
 * screen; the buttons stay clickable and the toast can be dismissed.
 * Exported for tests; runs on every new toast and, while toasts are
 * showing, whenever a dialog opens or closes, something scrolls, or the
 * viewport is resized.
 */
export function layoutToastStack(): void {
  const container = document.getElementById(CONTAINER_ID);
  if (!container) return;
  // Measure the stack where it sits by default; the rect is its real size.
  setVar(container, '--toast-max-width', null);
  setVar(container, '--toast-bottom', null);
  const stack = visibleRect(container);
  const dialog = topmostDialog();
  if (!stack || !dialog) return;

  const vw = document.documentElement.clientWidth || window.innerWidth;
  const vh = document.documentElement.clientHeight || window.innerHeight;
  const onScreen = intersect(dialog.rect, { left: 0, top: 0, right: vw, bottom: vh });
  const rows: Box[] = [];
  dialog.el.querySelectorAll(DIALOG_FOOTER_SELECTOR).forEach((el) => {
    const rect = visibleRect(el);
    const shown = rect && onScreen ? intersect(rect, onScreen) : null;
    if (shown) rows.push(shown);
  });
  const width = Math.min(stack.width, DEFAULT_MAX_WIDTH_PX);
  const boxAt = (bottom: number): Box =>
    ({ left: vw - EDGE_PX - width, right: vw - EDGE_PX, top: vh - bottom - stack.height, bottom: vh - bottom });
  const covered = (bottom: number): Box | undefined =>
    rows.find(row => intersect(boxAt(bottom), row) !== null);

  let row = covered(EDGE_PX);
  if (!row) return;

  const side = Math.floor(vw - dialog.rect.right - 2 * EDGE_PX);
  if (side >= MIN_SIDE_WIDTH_PX) {
    setVar(container, '--toast-max-width', `${Math.min(side, DEFAULT_MAX_WIDTH_PX)}px`);
    return;
  }
  const aboveDialog = Math.ceil(vh - dialog.rect.top + FOOTER_GAP_PX);
  if (dialog.rect.top - FOOTER_GAP_PX - stack.height >= EDGE_PX) {
    setVar(container, '--toast-bottom', `${aboveDialog}px`);
    return;
  }
  // Step up past each button row the stack would cover, lowest first.
  let bottom = EDGE_PX;
  for (let i = 0; row && i <= rows.length; i++) {
    bottom = Math.ceil(vh - row.top + FOOTER_GAP_PX);
    row = covered(bottom);
  }
  setVar(container, '--toast-bottom', `${bottom}px`);
}

// While toasts are showing, re-run the layout when a dialog opens or closes
// (class / hidden toggles, or a confirm dialog added to <body>), anything
// scrolls (a dialog body brings its buttons into view), or the viewport
// changes. Nothing is watched once the last toast is gone.
let watcher: MutationObserver | null = null;
let layoutQueued = false;

function queueLayout(): void {
  if (layoutQueued) return;
  layoutQueued = true;
  requestAnimationFrame(() => {
    layoutQueued = false;
    const container = document.getElementById(CONTAINER_ID);
    if (container?.querySelector('.toast')) layoutToastStack();
    else stopWatchingIfEmpty(container);
  });
}

function startWatching(): void {
  if (watcher) return;
  watcher = new MutationObserver(queueLayout);
  watcher.observe(document.body, {
    subtree: true,
    childList: true,
    attributes: true,
    attributeFilter: ['class', 'hidden'],
  });
  window.addEventListener('resize', queueLayout);
  // scroll doesn't bubble; capture sees scrolling inside any element.
  document.addEventListener('scroll', queueLayout, { capture: true, passive: true });
}

function stopWatchingIfEmpty(container: HTMLElement | null): void {
  if (container?.querySelector('.toast')) return;
  watcher?.disconnect();
  watcher = null;
  window.removeEventListener('resize', queueLayout);
  document.removeEventListener('scroll', queueLayout, { capture: true });
}

function ensureContainer(): HTMLElement {
  let container = document.getElementById(CONTAINER_ID);
  if (!container) {
    container = document.createElement('div');
    container.id = CONTAINER_ID;
    container.className = 'toast-container';
    container.setAttribute('aria-live', 'polite');
    container.setAttribute('aria-atomic', 'false');
    document.body.appendChild(container);
  }
  return container;
}

export function showToast(opts: ToastOptions): ToastHandle {
  const kind: ToastKind = opts.kind ?? 'info';
  const timeout = opts.timeout === null || opts.timeout === 0
    ? null
    : (opts.timeout ?? DEFAULT_TIMEOUT_MS);

  const container = ensureContainer();

  const toast = document.createElement('div');
  // Double-class: the new BEM-style `toast--<kind>` for the rewritten
  // styles, plus the legacy `toast-<kind>` that older tests + any
  // lingering stylesheet still target. Both are safe to keep.
  toast.className = `toast toast--${kind} toast-${kind}`;
  toast.setAttribute('role', kind === 'error' ? 'alert' : 'status');
  toast.tabIndex = 0;

  const icon = document.createElement('span');
  icon.className = 'toast-icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.appendChild(createIcon(KIND_ICONS[kind]));
  toast.appendChild(icon);

  const body = document.createElement('div');
  body.className = 'toast-message';
  body.textContent = opts.message;
  toast.appendChild(body);

  if (opts.actions && opts.actions.length > 0) {
    const actions = document.createElement('div');
    actions.className = 'toast-actions';
    opts.actions.forEach((a) => {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'btn btn-small';
      btn.textContent = a.label;
      btn.addEventListener('click', (e) => {
        e.stopPropagation();
        try {
          a.onClick();
        } finally {
          handle.dismiss();
        }
      });
      actions.appendChild(btn);
    });
    toast.appendChild(actions);
  }

  const closeBtn = document.createElement('button');
  closeBtn.type = 'button';
  closeBtn.className = 'toast-close';
  closeBtn.setAttribute('aria-label', 'Dismiss notification');
  closeBtn.appendChild(createIcon('x'));
  closeBtn.addEventListener('click', () => handle.dismiss());
  toast.appendChild(closeBtn);

  let dismissed = false;
  let timeoutId: ReturnType<typeof setTimeout> | null = null;
  let remaining = timeout;
  let startedAt = 0;
  let hovered = false;
  let focused = false;

  const handle: ToastHandle = {
    dismiss: () => {
      if (dismissed) return;
      dismissed = true;
      if (timeoutId !== null) clearTimeout(timeoutId);
      toast.removeEventListener('keydown', onKey);
      toast.classList.add('toast--leaving');
      // Allow CSS transition to play out; fall back to immediate remove
      // if the element has no transition (tests run without computed
      // transitions).
      const remove = (): void => {
        toast.remove();
        stopWatchingIfEmpty(container);
      };
      toast.addEventListener('transitionend', remove, { once: true });
      setTimeout(remove, 200);
    },
  };

  const onKey = (e: KeyboardEvent): void => {
    if (e.key === 'Escape') {
      e.preventDefault();
      handle.dismiss();
    }
  };
  toast.addEventListener('keydown', onKey);

  const startTimer = (): void => {
    if (dismissed || remaining === null || timeoutId !== null || hovered || focused) return;
    startedAt = Date.now();
    timeoutId = setTimeout(() => handle.dismiss(), remaining);
  };
  const pauseTimer = (): void => {
    if (timeoutId === null || remaining === null) return;
    clearTimeout(timeoutId);
    timeoutId = null;
    remaining = Math.max(remaining - (Date.now() - startedAt), 0);
  };
  // Restart with at least a second left so the toast does not vanish the
  // instant the pointer moves off it.
  const resumeTimer = (): void => {
    if (remaining !== null) remaining = Math.max(remaining, 1_000);
    startTimer();
  };
  toast.addEventListener('mouseenter', () => { hovered = true; pauseTimer(); });
  toast.addEventListener('mouseleave', () => { hovered = false; resumeTimer(); });
  toast.addEventListener('focusin', () => { focused = true; pauseTimer(); });
  toast.addEventListener('focusout', (e: FocusEvent) => {
    if (e.relatedTarget instanceof Node && toast.contains(e.relatedTarget)) return;
    focused = false;
    resumeTimer();
  });

  container.appendChild(toast);
  layoutToastStack();
  startWatching();
  startTimer();

  return handle;
}
