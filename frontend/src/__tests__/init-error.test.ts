/**
 * Init failure state (issue #206): an init() failure must replace the
 * pre-rendered Home markup with a visible error and a retry, not leave a
 * silently wrong page.
 */
import { renderInitError, INIT_ERROR_ID, INIT_ERROR_TITLE } from '../init-error';

function mountShell(): void {
  document.title = 'CUDly - Cloud Commitment Optimizer';
  document.body.innerHTML = `
    <main id="main-content">
      <div id="home-tab" class="tab-content active"></div>
      <div id="plans-tab" class="tab-content"></div>
    </main>`;
}

describe('renderInitError', () => {
  beforeEach(mountShell);

  test('hides every tab panel, including the pre-rendered Home one', () => {
    renderInitError(new Error('boom'));
    expect(document.querySelectorAll('.tab-content.active')).toHaveLength(0);
  });

  test('shows an alert card with a heading, the error detail and a retry button', () => {
    renderInitError(Object.assign(new Error('upstream unavailable'), { status: 503 }));
    const card = document.getElementById(INIT_ERROR_ID);
    expect(card).not.toBeNull();
    expect(card!.getAttribute('role')).toBe('alert');
    expect(card!.querySelector('h3')!.textContent).toBe('CUDly could not finish loading');
    expect(card!.textContent).toContain('Details: HTTP 503: upstream unavailable');
    expect(card!.querySelector('button')!.textContent).toBe('Try again');
  });

  test('does not repeat the status when the message is the generic HTTP line', () => {
    renderInitError(Object.assign(new Error('HTTP 500'), { status: 500 }));
    expect(document.getElementById(INIT_ERROR_ID)!.textContent).toContain('Details: HTTP 500');
    expect(document.getElementById(INIT_ERROR_ID)!.textContent).not.toContain('HTTP 500: HTTP 500');
  });

  test('falls back to a generic detail for non-Error values', () => {
    renderInitError(undefined);
    expect(document.getElementById(INIT_ERROR_ID)!.textContent).toContain('Details: Unknown error');
  });

  test('renders the error text as text, not markup', () => {
    renderInitError(new Error('<img src=x onerror=alert(1)>'));
    const card = document.getElementById(INIT_ERROR_ID)!;
    expect(card.querySelector('img')).toBeNull();
    expect(card.textContent).toContain('<img src=x onerror=alert(1)>');
  });

  test('replaces the generic document title', () => {
    renderInitError(new Error('boom'));
    expect(document.title).toBe(INIT_ERROR_TITLE);
  });

  test('the retry button reloads the page', () => {
    const reload = jest.fn();
    renderInitError(new Error('boom'), reload);
    document.querySelector<HTMLButtonElement>(`#${INIT_ERROR_ID} button`)!.click();
    expect(reload).toHaveBeenCalledTimes(1);
  });

  test('a second call replaces the first card instead of stacking', () => {
    renderInitError(new Error('first'));
    renderInitError(new Error('second'));
    const cards = document.querySelectorAll(`#${INIT_ERROR_ID}`);
    expect(cards).toHaveLength(1);
    expect(cards[0]!.textContent).toContain('second');
  });

  test('is a no-op without the main content container', () => {
    document.body.innerHTML = '<div id="home-tab" class="tab-content active"></div>';
    expect(() => renderInitError(new Error('boom'))).not.toThrow();
    expect(document.getElementById(INIT_ERROR_ID)).toBeNull();
  });
});
