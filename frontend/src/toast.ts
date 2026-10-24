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
 * focus. The dismiss() return value lets callers clear a toast
 * programmatically (e.g. tie an "uploading..." toast to the completion
 * of the upload).
 *
 * Open dialogs (issue #742): a toast stays up for 30s, and the default
 * bottom-right corner is where a dialog's Cancel / Save row sits on a
 * laptop-sized viewport, so an error toast used to swallow the click on
 * Save until it was dismissed. While a dialog box is on screen,
 * layoutToastStack() keeps the stack off it: beside the dialog when the
 * gap to its right is wide enough for a readable toast, otherwise lifted
 * above the dialog's button row. The positions are CSS custom properties
 * set through the CSSOM (CSP forbids style attributes in markup).
 */

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

const KIND_ICONS: Record<ToastKind, string> = {
  success: '\u2713',  // ✓
  info: '\u2139',     // ℹ
  warning: '\u26A0',  // ⚠
  error: '\u2715',    // ✕
};

// Dialog boxes the stack must not cover, and the button rows inside them
// that must stay clickable.
const DIALOG_BOX_SELECTOR = '.modal-content, .modal-confirm';
const DIALOG_FOOTER_SELECTOR = '.modal-footer, .modal-buttons, .modal-confirm-actions';
// Matches the container's 1rem inset in components.css.
const EDGE_PX = 16;
const FOOTER_GAP_PX = 8;
// Narrower than this, a toast beside the dialog is too cramped to read, so
// the stack is lifted above the button row instead.
const MIN_SIDE_WIDTH_PX = 240;

function setVar(el: HTMLElement, name: string, value: string | null): void {
  if (value === null) el.style.removeProperty(name);
  else if (el.style.getPropertyValue(name) !== value) el.style.setProperty(name, value);
}

function visibleRect(el: Element): DOMRect | null {
  const rect = el.getBoundingClientRect();
  return rect.width > 0 && rect.height > 0 ? rect : null;
}

/**
 * Keep the toast stack off any dialog box that is on screen. Exported for
 * tests; runs on every new toast and, while toasts are showing, whenever a
 * dialog opens or closes or the viewport is resized.
 */
export function layoutToastStack(): void {
  const container = document.getElementById(CONTAINER_ID);
  if (!container) return;
  let maxWidth: string | null = null;
  let bottom: string | null = null;

  const boxes: Array<{ el: Element; rect: DOMRect }> = [];
  document.querySelectorAll(DIALOG_BOX_SELECTOR).forEach((el) => {
    const rect = visibleRect(el);
    if (rect) boxes.push({ el, rect });
  });
  if (boxes.length > 0) {
    const viewportWidth = document.documentElement.clientWidth || window.innerWidth;
    const viewportHeight = document.documentElement.clientHeight || window.innerHeight;
    const dialogRight = Math.max(...boxes.map(b => b.rect.right));
    const side = Math.floor(viewportWidth - dialogRight - 2 * EDGE_PX);
    if (side >= MIN_SIDE_WIDTH_PX) {
      maxWidth = `${side}px`;
    } else {
      const footerTops: number[] = [];
      for (const { el } of boxes) {
        const footer = Array.from(el.querySelectorAll(DIALOG_FOOTER_SELECTOR)).pop();
        const rect = footer ? visibleRect(footer) : null;
        if (rect) footerTops.push(rect.top);
      }
      if (footerTops.length > 0) {
        const lift = Math.ceil(viewportHeight - Math.min(...footerTops) + FOOTER_GAP_PX);
        if (lift > EDGE_PX) bottom = `${lift}px`;
      }
    }
  }
  setVar(container, '--toast-max-width', maxWidth);
  setVar(container, '--toast-bottom', bottom);
}

// While toasts are showing, re-run the layout when a dialog opens or closes
// (class / hidden toggles, or a confirm dialog added to <body>) or the
// viewport changes. Nothing is watched while the stack is empty.
let watcher: MutationObserver | null = null;
let layoutQueued = false;

function queueLayout(): void {
  if (layoutQueued) return;
  layoutQueued = true;
  requestAnimationFrame(() => {
    layoutQueued = false;
    layoutToastStack();
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
}

function stopWatchingIfEmpty(container: HTMLElement): void {
  if (container.querySelector('.toast')) return;
  watcher?.disconnect();
  watcher = null;
  window.removeEventListener('resize', queueLayout);
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
  icon.textContent = KIND_ICONS[kind];
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
  closeBtn.textContent = '\u00D7'; // ×
  closeBtn.addEventListener('click', () => handle.dismiss());
  toast.appendChild(closeBtn);

  let dismissed = false;
  let timeoutId: ReturnType<typeof setTimeout> | null = null;

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

  container.appendChild(toast);
  layoutToastStack();
  startWatching();

  if (timeout !== null) {
    timeoutId = setTimeout(() => handle.dismiss(), timeout);
  }

  return handle;
}
