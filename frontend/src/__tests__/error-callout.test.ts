/**
 * Section load-failure callout: keeps the failure line as p.error, is
 * announced, and offers "Try again" that re-runs the section's load.
 */
import { buildErrorCallout, renderErrorCallout, ERROR_CALLOUT_HINT } from '../lib/error-callout';

describe('error callout', () => {
  test('keeps the exact failure line in p.error and is announced', () => {
    const el = buildErrorCallout({ message: 'Failed to load plans: <b>boom</b>' });
    expect(el.getAttribute('role')).toBe('alert');
    const line = el.querySelector('p.error');
    // textContent, not innerHTML: markup in the reason stays text.
    expect(line?.textContent).toBe('Failed to load plans: <b>boom</b>');
    expect(line?.querySelector('b')).toBeNull();
    expect(el.querySelector('.error-callout-hint')?.textContent).toBe(ERROR_CALLOUT_HINT);
    expect(el.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true');
  });

  test('has no button without a retry handler', () => {
    const el = buildErrorCallout({ message: 'Failed to load plans: x' });
    expect(el.querySelector('button')).toBeNull();
  });

  test('"Try again" re-runs the load', () => {
    const onRetry = jest.fn();
    const host = document.createElement('div');
    host.innerHTML = '<p>stale</p>';
    renderErrorCallout(host, { message: 'Failed to load recommendations: internal error', onRetry });
    expect(host.children.length).toBe(1);
    const btn = host.querySelector('button')!;
    expect(btn.textContent).toBe('Try again');
    expect(btn.type).toBe('button');
    btn.click();
    expect(onRetry).toHaveBeenCalledTimes(1);
  });
});
