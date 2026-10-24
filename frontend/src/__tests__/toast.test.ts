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
});

// Issue #742: a toast must not sit on top of an open dialog's Save button.
// jsdom has no layout, so the dialog geometry is stubbed.
describe('toast stack and open dialogs', () => {
  const rect = (left: number, top: number, width: number, height: number): DOMRect =>
    ({ left, top, width, height, right: left + width, bottom: top + height, x: left, y: top, toJSON: () => ({}) }) as DOMRect;

  function openDialog(box: DOMRect, footer: DOMRect): HTMLElement {
    const modal = document.createElement('div');
    modal.className = 'modal';
    modal.innerHTML = '<div class="modal-content"><div class="modal-body"></div><div class="modal-footer"><button>Save</button></div></div>';
    document.body.appendChild(modal);
    jest.spyOn(modal.querySelector('.modal-content')!, 'getBoundingClientRect').mockReturnValue(box);
    jest.spyOn(modal.querySelector('.modal-footer')!, 'getBoundingClientRect').mockReturnValue(footer);
    return modal;
  }

  function setViewport(width: number, height: number): void {
    jest.spyOn(document.documentElement, 'clientWidth', 'get').mockReturnValue(width);
    jest.spyOn(document.documentElement, 'clientHeight', 'get').mockReturnValue(height);
  }

  const container = (): HTMLElement => document.getElementById('toast-container')!;

  afterEach(() => {
    jest.restoreAllMocks();
    document.body.replaceChildren();
  });

  it('keeps the default corner when no dialog is open', () => {
    setViewport(1280, 720);
    showToast({ message: 'saved' });
    expect(container().style.getPropertyValue('--toast-bottom')).toBe('');
    expect(container().style.getPropertyValue('--toast-max-width')).toBe('');
  });

  it('fits the stack beside a dialog when the gap to its right is wide enough', () => {
    setViewport(1280, 720);
    openDialog(rect(300, 36, 680, 648), rect(300, 620, 680, 64));
    showToast({ message: 'Buffer fraction is required.', kind: 'error' });
    // 1280 - 980 - 2 * 16 = 268px beside the dialog; the bottom stays put.
    expect(container().style.getPropertyValue('--toast-max-width')).toBe('268px');
    expect(container().style.getPropertyValue('--toast-bottom')).toBe('');
  });

  it("lifts the stack above the dialog's button row when there is no room beside it", () => {
    setViewport(390, 844);
    openDialog(rect(10, 40, 370, 760), rect(10, 736, 370, 64));
    showToast({ message: 'Failed to save', kind: 'error' });
    // 844 - 736 + 8 = 116px from the bottom: above the footer, not on it.
    expect(container().style.getPropertyValue('--toast-bottom')).toBe('116px');
    expect(container().style.getPropertyValue('--toast-max-width')).toBe('');
  });

  it('re-lays out when a dialog opens or closes under a showing toast', async () => {
    setViewport(390, 844);
    showToast({ message: 'saved', kind: 'success' });
    expect(container().style.getPropertyValue('--toast-bottom')).toBe('');

    const modal = openDialog(rect(10, 40, 370, 760), rect(10, 736, 370, 64));
    await new Promise(resolve => requestAnimationFrame(resolve));
    expect(container().style.getPropertyValue('--toast-bottom')).toBe('116px');

    jest.spyOn(modal.querySelector('.modal-content')!, 'getBoundingClientRect').mockReturnValue(rect(0, 0, 0, 0));
    modal.classList.add('hidden');
    await new Promise(resolve => requestAnimationFrame(resolve));
    expect(container().style.getPropertyValue('--toast-bottom')).toBe('');
  });

  it('ignores dialogs that are not rendered', () => {
    setViewport(390, 844);
    openDialog(rect(0, 0, 0, 0), rect(0, 0, 0, 0));
    showToast({ message: 'saved' });
    layoutToastStack();
    expect(container().style.getPropertyValue('--toast-bottom')).toBe('');
  });
});
