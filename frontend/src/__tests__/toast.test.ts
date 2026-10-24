/**
 * Tests for the shared toast notification system.
 */

import { showToast, layoutToastStack } from '../toast';

describe('showToast', () => {
  beforeEach(() => {
    jest.useFakeTimers();
  });

  afterEach(() => {
    jest.useRealTimers();
    const body = document.body;
    while (body.firstChild) body.removeChild(body.firstChild);
  });

  it('mounts a toast into a lazily-created container', () => {
    showToast({ message: 'Hello' });
    const container = document.getElementById('toast-container');
    expect(container).not.toBeNull();
    expect(container?.querySelectorAll('.toast').length).toBe(1);
  });

  it('maps kind to a --kind class', () => {
    showToast({ message: 'S', kind: 'success' });
    showToast({ message: 'I', kind: 'info' });
    showToast({ message: 'W', kind: 'warning' });
    showToast({ message: 'E', kind: 'error' });
    expect(document.querySelector('.toast.toast--success')).not.toBeNull();
    expect(document.querySelector('.toast.toast--info')).not.toBeNull();
    expect(document.querySelector('.toast.toast--warning')).not.toBeNull();
    expect(document.querySelector('.toast.toast--error')).not.toBeNull();
  });

  it('defaults to 30s timeout and removes the toast when it expires', () => {
    showToast({ message: 'bye' });
    expect(document.querySelector('.toast')).not.toBeNull();
    jest.advanceTimersByTime(29_999);
    expect(document.querySelector('.toast')).not.toBeNull();
    jest.advanceTimersByTime(2);
    // After timeout, leaving class is applied; the inline setTimeout(200)
    // fallback runs next to actually remove the node.
    jest.advanceTimersByTime(200);
    expect(document.querySelector('.toast')).toBeNull();
  });

  it('sticky mode (timeout: null) does not auto-dismiss', () => {
    showToast({ message: 'stay', timeout: null });
    jest.advanceTimersByTime(60_000);
    expect(document.querySelector('.toast')).not.toBeNull();
  });

  it('× button dismisses the toast', () => {
    showToast({ message: 'close me' });
    const closeBtn = document.querySelector<HTMLButtonElement>('.toast-close');
    expect(closeBtn).not.toBeNull();
    closeBtn!.click();
    jest.advanceTimersByTime(200);
    expect(document.querySelector('.toast')).toBeNull();
  });

  it('Esc while toast has focus dismisses it', () => {
    showToast({ message: 'esc me' });
    const toast = document.querySelector<HTMLDivElement>('.toast')!;
    toast.focus();
    toast.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    jest.advanceTimersByTime(200);
    expect(document.querySelector('.toast')).toBeNull();
  });

  it('stacks multiple toasts in the container', () => {
    showToast({ message: 'one' });
    showToast({ message: 'two' });
    showToast({ message: 'three' });
    const toasts = document.querySelectorAll('.toast');
    expect(toasts.length).toBe(3);
  });

  it('action button invokes its handler and dismisses the toast', () => {
    const handler = jest.fn();
    showToast({ message: 'with action', actions: [{ label: 'Undo', onClick: handler }] });
    const actionBtn = document.querySelector<HTMLButtonElement>('.toast-actions button');
    expect(actionBtn?.textContent).toBe('Undo');
    actionBtn!.click();
    expect(handler).toHaveBeenCalledTimes(1);
    jest.advanceTimersByTime(200);
    expect(document.querySelector('.toast')).toBeNull();
  });

  it('programmatic dismiss() removes the toast', () => {
    const { dismiss } = showToast({ message: 'prog', timeout: null });
    expect(document.querySelector('.toast')).not.toBeNull();
    dismiss();
    jest.advanceTimersByTime(200);
    expect(document.querySelector('.toast')).toBeNull();
  });

  it('error-kind has role="alert" for assistive-tech urgency', () => {
    showToast({ message: 'bad', kind: 'error' });
    expect(document.querySelector('.toast--error')?.getAttribute('role')).toBe('alert');
  });

  it('non-error kind has role="status" (polite)', () => {
    showToast({ message: 'ok', kind: 'success' });
    expect(document.querySelector('.toast--success')?.getAttribute('role')).toBe('status');
  });

  describe('pause while hovered or focused (issue #728, WCAG 2.2.1)', () => {
    const toastEl = (): HTMLElement => document.querySelector('.toast') as HTMLElement;

    it('hover pauses the countdown and leaving resumes it with the time left', () => {
      showToast({ message: 'read me', timeout: 5_000 });
      jest.advanceTimersByTime(3_000);
      toastEl().dispatchEvent(new MouseEvent('mouseenter'));
      jest.advanceTimersByTime(60_000);
      expect(toastEl()).not.toBeNull();

      toastEl().dispatchEvent(new MouseEvent('mouseleave'));
      jest.advanceTimersByTime(1_999);
      expect(toastEl().classList.contains('toast--leaving')).toBe(false);
      jest.advanceTimersByTime(2 + 200);
      expect(document.querySelector('.toast')).toBeNull();
    });

    it('focus inside the toast pauses it until focus leaves the toast', () => {
      const handler = jest.fn();
      showToast({ message: 'undo?', timeout: 5_000, actions: [{ label: 'Undo', onClick: handler }] });
      const undo = toastEl().querySelector('button') as HTMLButtonElement;
      undo.focus();
      jest.advanceTimersByTime(60_000);
      expect(toastEl()).not.toBeNull();

      // Moving focus within the toast keeps it paused.
      (toastEl().querySelector('.toast-close') as HTMLButtonElement).focus();
      jest.advanceTimersByTime(60_000);
      expect(toastEl()).not.toBeNull();

      (document.activeElement as HTMLElement).blur();
      jest.advanceTimersByTime(5_000 + 200);
      expect(document.querySelector('.toast')).toBeNull();
    });

    it('resumes with at least one second so it does not vanish on mouse-out', () => {
      showToast({ message: 'almost gone', timeout: 5_000 });
      jest.advanceTimersByTime(4_990);
      toastEl().dispatchEvent(new MouseEvent('mouseenter'));
      toastEl().dispatchEvent(new MouseEvent('mouseleave'));
      jest.advanceTimersByTime(900);
      expect(toastEl().classList.contains('toast--leaving')).toBe(false);
    });

    it('sticky toasts stay sticky after hover', () => {
      showToast({ message: 'stay', timeout: null });
      toastEl().dispatchEvent(new MouseEvent('mouseenter'));
      toastEl().dispatchEvent(new MouseEvent('mouseleave'));
      jest.advanceTimersByTime(120_000);
      expect(toastEl()).not.toBeNull();
    });
  });
});

// Issue #742: a toast must not sit on top of an open dialog's Save button,
// and must not move onto a dialog it wasn't covering. jsdom has no layout,
// so the dialog, button-row and toast-stack geometry is stubbed.
describe('toast stack and open dialogs', () => {
  interface Box { left: number; top: number; right: number; bottom: number }
  const rect = (left: number, top: number, width: number, height: number): DOMRect =>
    ({ left, top, width, height, right: left + width, bottom: top + height, x: left, y: top, toJSON: () => ({}) }) as DOMRect;
  const NONE = rect(0, 0, 0, 0);
  const overlaps = (a: Box, b: Box): boolean =>
    a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;

  let vw = 0;
  let vh = 0;
  // The stack's size at its default corner (right 16px, bottom 16px).
  let stackW = 0;
  let stackH = 0;

  function setViewport(width: number, height: number): void {
    vw = width;
    vh = height;
    jest.spyOn(document.documentElement, 'clientWidth', 'get').mockImplementation(() => vw);
    jest.spyOn(document.documentElement, 'clientHeight', 'get').mockImplementation(() => vh);
  }

  // Pre-create the container showToast() reuses, so its rect can be stubbed.
  function setStack(width: number, height: number): void {
    stackW = width;
    stackH = height;
    let el = document.getElementById('toast-container');
    if (!el) {
      el = document.createElement('div');
      el.id = 'toast-container';
      document.body.appendChild(el);
    }
    jest.spyOn(el, 'getBoundingClientRect').mockImplementation(() =>
      rect(vw - 16 - stackW, vh - 16 - stackH, stackW, stackH));
  }

  function openDialog(box: DOMRect, footers: DOMRect[], cls = 'modal-content'): HTMLElement {
    const modal = document.createElement('div');
    modal.className = 'modal';
    const rows = footers.map(() => '<div class="modal-buttons"><button>Save</button></div>').join('');
    modal.innerHTML = `<div class="${cls}"><div class="modal-body"></div>${rows}</div>`;
    document.body.appendChild(modal);
    jest.spyOn(modal.querySelector(`.${cls}`)!, 'getBoundingClientRect').mockReturnValue(box);
    modal.querySelectorAll('.modal-buttons').forEach((row, i) =>
      jest.spyOn(row, 'getBoundingClientRect').mockReturnValue(footers[i]!));
    return modal;
  }

  function openConfirm(box: DOMRect, actions: DOMRect): HTMLElement {
    const backdrop = document.createElement('div');
    backdrop.className = 'modal-confirm-backdrop';
    backdrop.innerHTML = '<div class="modal-confirm"><h3 class="modal-confirm-title">Enable Auto Approve?</h3>'
      + '<div class="modal-confirm-body">Ladder purchases will run without approval.</div>'
      + '<div class="modal-confirm-actions"><button>Cancel</button><button>Enable</button></div></div>';
    document.body.appendChild(backdrop);
    jest.spyOn(backdrop.querySelector('.modal-confirm')!, 'getBoundingClientRect').mockReturnValue(box);
    jest.spyOn(backdrop.querySelector('.modal-confirm-actions')!, 'getBoundingClientRect').mockReturnValue(actions);
    return backdrop;
  }

  const container = (): HTMLElement => document.getElementById('toast-container')!;
  const prop = (name: string): string => container().style.getPropertyValue(name);
  // Where the stack ends up, from the properties layoutToastStack() set.
  function stackBox(): Box {
    const bottom = parseInt(prop('--toast-bottom') || '16', 10);
    const width = Math.min(stackW, parseInt(prop('--toast-max-width') || '400', 10));
    return { left: vw - 16 - width, right: vw - 16, top: vh - bottom - stackH, bottom: vh - bottom };
  }
  const nextFrame = (): Promise<void> => new Promise(resolve => requestAnimationFrame(() => resolve()));

  afterEach(() => {
    jest.restoreAllMocks();
    document.body.replaceChildren();
  });

  it('keeps the default corner when no dialog is open', () => {
    setViewport(1280, 720);
    setStack(400, 60);
    showToast({ message: 'saved' });
    expect(prop('--toast-bottom')).toBe('');
    expect(prop('--toast-max-width')).toBe('');
  });

  it('fits the stack beside a dialog when its corner would cover the button row', () => {
    setViewport(1280, 720);
    setStack(400, 60);
    openDialog(rect(300, 36, 680, 648), [rect(300, 620, 680, 64)]);
    showToast({ message: 'Buffer fraction is required.', kind: 'error' });
    // 1280 - 980 - 2 * 16 = 268px beside the dialog; the bottom stays put.
    expect(prop('--toast-max-width')).toBe('268px');
    expect(prop('--toast-bottom')).toBe('');
  });

  it('never widens the stack past its default 400px beside a dialog', () => {
    // A 480px confirm at 1920 wide leaves a 688px gap.
    setViewport(1920, 1080);
    setStack(400, 60);
    openConfirm(rect(720, 400, 480, 280), rect(744, 616, 432, 40));
    showToast({ message: 'A long error message that would stretch across the whole gap', kind: 'error' });
    expect(prop('--toast-max-width')).toBe('');
  });

  it("lifts the stack above the dialog's button row when there is no room beside or above it", () => {
    setViewport(390, 844);
    setStack(358, 70);
    openDialog(rect(10, 40, 370, 760), [rect(10, 736, 370, 64)]);
    showToast({ message: 'Failed to save', kind: 'error' });
    // 844 - 736 + 8 = 116px from the bottom: above the footer, not on it.
    expect(prop('--toast-bottom')).toBe('116px');
    expect(prop('--toast-max-width')).toBe('');
  });

  it('lifts the stack above a short bottom-sheet dialog instead of onto its body', () => {
    setViewport(390, 844);
    setStack(358, 70);
    openConfirm(rect(0, 600, 390, 244), rect(24, 780, 342, 40));
    showToast({ message: 'Failed', kind: 'error' });
    // 844 - 600 + 8: the whole sheet stays clear.
    expect(prop('--toast-bottom')).toBe('252px');
    expect(overlaps(stackBox(), rect(0, 600, 390, 244))).toBe(false);
  });

  it('keeps the default corner under a centred short dialog with room below it', () => {
    // A .modal-wide dialog (800px) at 1280x720 leaves a 208px gap, but its
    // buttons sit mid-screen, far above the toast's corner.
    setViewport(1280, 720);
    setStack(400, 60);
    const dialog = rect(240, 170, 800, 380);
    openDialog(dialog, [rect(272, 480, 736, 40)]);
    showToast({ message: 'Failed to save overrides', kind: 'error' });
    expect(prop('--toast-bottom')).toBe('');
    expect(prop('--toast-max-width')).toBe('');
    expect(overlaps(stackBox(), dialog)).toBe(false);
  });

  it('keeps the default corner when the stack is too narrow to reach the button row', () => {
    setViewport(1280, 720);
    setStack(200, 60);
    // The footer ends at x=980; a 200px stack starts at 1280 - 16 - 200 = 1064.
    openDialog(rect(300, 36, 680, 648), [rect(300, 620, 680, 64)]);
    showToast({ message: 'Saved', kind: 'success' });
    expect(prop('--toast-bottom')).toBe('');
    expect(prop('--toast-max-width')).toBe('');
  });

  it('follows only the topmost dialog: a confirm over the ladder modal keeps the stack off its body', () => {
    setViewport(1100, 720);
    setStack(400, 60);
    // The ladder modal's footer is under the toast's corner, but a confirm
    // (and its backdrop) now covers that modal.
    openDialog(rect(210, 36, 680, 648), [rect(210, 620, 680, 64)]);
    const confirmBox = rect(310, 250, 480, 220);
    openConfirm(confirmBox, rect(334, 410, 432, 40));
    showToast({ message: 'Max actions per run must be between 1 and 50', kind: 'error' });
    expect(prop('--toast-bottom')).toBe('');
    expect(overlaps(stackBox(), confirmBox)).toBe(false);
  });

  it('moves the stack beside a tall confirm whose buttons it would cover, never onto the confirm', () => {
    setViewport(1100, 720);
    setStack(400, 60);
    openDialog(rect(210, 36, 680, 648), [rect(210, 620, 680, 64)]);
    const confirmBox = rect(310, 60, 480, 600);
    openConfirm(confirmBox, rect(334, 620, 432, 40));
    showToast({ message: 'x', kind: 'error' });
    expect(prop('--toast-max-width')).toBe('278px');
    expect(overlaps(stackBox(), confirmBox)).toBe(false);
  });

  it('uses the visible button row, not the last one in the DOM', () => {
    // Profile modal shape: a Save Changes row, then a hidden MFA row.
    setViewport(390, 844);
    setStack(358, 70);
    openDialog(rect(10, 40, 370, 760), [rect(10, 736, 370, 64), NONE]);
    showToast({ message: 'Failed to save profile', kind: 'error' });
    expect(prop('--toast-bottom')).toBe('116px');
  });

  it('re-lays out when a button row scrolls into view under the stack', async () => {
    setViewport(1100, 720);
    setStack(400, 60);
    const modal = openDialog(rect(210, 36, 680, 648), [rect(210, 760, 680, 64)]);
    showToast({ message: 'Failed', kind: 'error' });
    // The row is below the fold of the scrolling dialog: nothing to avoid yet.
    await nextFrame();
    expect(prop('--toast-bottom')).toBe('');

    jest.spyOn(modal.querySelector('.modal-buttons')!, 'getBoundingClientRect').mockReturnValue(rect(210, 600, 680, 64));
    modal.querySelector('.modal-body')!.dispatchEvent(new Event('scroll'));
    await nextFrame();
    expect(prop('--toast-bottom')).toBe(`${720 - 600 + 8}px`);
  });

  it('re-lays out when a dialog opens or closes under a showing toast', async () => {
    setViewport(390, 844);
    setStack(358, 70);
    showToast({ message: 'saved', kind: 'success' });
    expect(prop('--toast-bottom')).toBe('');

    const modal = openDialog(rect(10, 40, 370, 760), [rect(10, 736, 370, 64)]);
    await nextFrame();
    expect(prop('--toast-bottom')).toBe('116px');

    jest.spyOn(modal.querySelector('.modal-content')!, 'getBoundingClientRect').mockReturnValue(NONE);
    modal.classList.add('hidden');
    await nextFrame();
    expect(prop('--toast-bottom')).toBe('');
  });

  it('ignores dialogs that are not rendered', () => {
    setViewport(390, 844);
    setStack(358, 70);
    openDialog(NONE, [NONE]);
    showToast({ message: 'saved' });
    layoutToastStack();
    expect(prop('--toast-bottom')).toBe('');
  });

  it('stops watching for dialogs and scrolling once the last toast is gone', () => {
    jest.useFakeTimers();
    try {
      const disconnect = jest.spyOn(MutationObserver.prototype, 'disconnect');
      const removed = jest.spyOn(document, 'removeEventListener');
      const a = showToast({ message: 'a', timeout: null });
      const b = showToast({ message: 'b', timeout: null });
      a.dismiss();
      jest.advanceTimersByTime(200);
      expect(disconnect).not.toHaveBeenCalled();
      b.dismiss();
      jest.advanceTimersByTime(200);
      expect(disconnect).toHaveBeenCalled();
      expect(removed).toHaveBeenCalledWith('scroll', expect.any(Function), { capture: true });
    } finally {
      jest.useRealTimers();
    }
  });
});
