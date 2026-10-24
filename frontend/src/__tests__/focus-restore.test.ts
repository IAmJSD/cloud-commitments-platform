/**
 * Issue #191: unit tests for lib/focus-restore (capture the focused control
 * before an innerHTML rebuild, focus its replacement afterwards).
 */

import { captureFocus, restoreFocus } from '../lib/focus-restore';

let container: HTMLDivElement;

function render(html: string): void {
  container.innerHTML = html;
}

beforeEach(() => {
  container = document.createElement('div');
  document.body.replaceChildren(container);
});

describe('captureFocus', () => {
  test('returns null when focus is outside the container', () => {
    const outside = document.createElement('button');
    document.body.appendChild(outside);
    outside.focus();
    expect(captureFocus(container)).toBeNull();
  });

  test('returns null when nothing is focused', () => {
    render('<input type="checkbox" data-rec-id="a">');
    expect(captureFocus(container)).toBeNull();
  });

  test('returns null for an element with only a type attribute', () => {
    render('<input type="checkbox">');
    container.querySelector('input')!.focus();
    expect(captureFocus(container)).toBeNull();
  });

  test('records tag and identifying attributes of the focused control', () => {
    render('<button type="button" data-rec-id="r1" data-action="plan" class="x">Plan</button>');
    container.querySelector('button')!.focus();
    expect(captureFocus(container)).toEqual({
      tag: 'BUTTON',
      attrs: [['type', 'button'], ['data-rec-id', 'r1'], ['data-action', 'plan']],
    });
  });
});

describe('restoreFocus', () => {
  const markup = (checked: string) =>
    `<input type="checkbox" data-rec-id="r1">` +
    `<input type="checkbox" data-rec-id="r2" ${checked}>` +
    `<button type="button" data-rec-id="r2" data-action="plan">Plan</button>`;

  test('focuses the matching control after a rebuild', () => {
    render(markup(''));
    container.querySelector<HTMLInputElement>('input[data-rec-id="r2"]')!.focus();
    const snap = captureFocus(container);

    render(markup('checked'));
    expect(restoreFocus(container, snap)).toBe(true);
    expect(document.activeElement).toBe(container.querySelector('input[data-rec-id="r2"]'));
  });

  test('distinguishes controls sharing a data-rec-id by tag and data-action', () => {
    render(markup(''));
    container.querySelector<HTMLButtonElement>('button')!.focus();
    const snap = captureFocus(container);

    render(markup(''));
    restoreFocus(container, snap);
    expect(document.activeElement).toBe(container.querySelector('button'));
  });

  test('matches attribute values that contain CSS-special characters', () => {
    const key = 'aws|ec2|m5.large|"us-east-1"';
    container.innerHTML = '';
    const btn = document.createElement('button');
    btn.setAttribute('data-cell-key', key);
    container.appendChild(btn);
    btn.focus();
    const snap = captureFocus(container);

    const fresh = document.createElement('button');
    fresh.setAttribute('data-cell-key', key);
    container.replaceChildren(fresh);
    expect(restoreFocus(container, snap)).toBe(true);
    expect(document.activeElement).toBe(fresh);
  });

  test('leaves focus alone when the control no longer exists', () => {
    render(markup(''));
    container.querySelector<HTMLInputElement>('input[data-rec-id="r2"]')!.focus();
    const snap = captureFocus(container);

    render('<input type="checkbox" data-rec-id="r1">');
    expect(restoreFocus(container, snap)).toBe(false);
    expect(document.activeElement).toBe(document.body);
  });

  test('does not steal focus that already moved elsewhere', () => {
    render(markup(''));
    container.querySelector<HTMLInputElement>('input[data-rec-id="r2"]')!.focus();
    const snap = captureFocus(container);

    render(markup(''));
    const dialogBtn = document.createElement('button');
    document.body.appendChild(dialogBtn);
    dialogBtn.focus();
    expect(restoreFocus(container, snap)).toBe(false);
    expect(document.activeElement).toBe(dialogBtn);
  });

  test('null snapshot is a no-op', () => {
    expect(restoreFocus(container, null)).toBe(false);
  });
});
