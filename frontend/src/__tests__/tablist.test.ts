/**
 * Sub-tab keyboard model and sidebar link semantics (issue #730).
 *
 * The in-page sub-tab lists keep role="tablist" and so must implement its
 * keyboard model (roving tabindex, arrows, Home/End). The sidebar is page
 * navigation, so it is plain links with aria-current instead of ARIA tabs.
 */
import fs from 'fs';
import path from 'path';

import { syncTabStops, wireTablistKeyboard } from '../lib/tablist';

function buildTablist(orientation?: 'vertical'): HTMLElement {
  document.body.innerHTML = `
    <style>.admin-only { display: none; } .admin-only.visible { display: revert; }</style>
    <div role="tablist" aria-label="Sub" ${orientation ? `aria-orientation="${orientation}"` : ''}>
      <button role="tab" id="t1" aria-selected="true">One</button>
      <button role="tab" id="t2" aria-selected="false">Two</button>
      <button role="tab" id="t3" aria-selected="false" class="admin-only">Three</button>
      <button role="tab" id="t4" aria-selected="false">Four</button>
    </div>
    <button id="outside">Outside</button>
  `;
  return document.querySelector<HTMLElement>('[role="tablist"]')!;
}

const tab = (id: string): HTMLElement => document.getElementById(id)!;

function press(key: string, init: KeyboardEventInit = {}): KeyboardEvent {
  const evt = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...init });
  (document.activeElement ?? document.body).dispatchEvent(evt);
  return evt;
}

describe('syncTabStops', () => {
  test('gives the selected tab the only Tab stop', () => {
    const list = buildTablist();
    syncTabStops(list);
    expect(['t1', 't2', 't3', 't4'].map(id => tab(id).getAttribute('tabindex'))).toEqual(['0', '-1', '-1', '-1']);

    tab('t1').setAttribute('aria-selected', 'false');
    tab('t4').setAttribute('aria-selected', 'true');
    syncTabStops(list);
    expect(['t1', 't2', 't3', 't4'].map(id => tab(id).getAttribute('tabindex'))).toEqual(['-1', '-1', '-1', '0']);
  });

  test('falls back to the first reachable tab when the selected one is hidden', () => {
    const list = buildTablist();
    tab('t1').setAttribute('aria-selected', 'false');
    tab('t1').hidden = true;
    tab('t3').setAttribute('aria-selected', 'true'); // admin-only, not visible
    syncTabStops(list);
    expect(tab('t2').getAttribute('tabindex')).toBe('0');
    expect(tab('t3').getAttribute('tabindex')).toBe('-1');
  });

  test('is a no-op for a missing tablist', () => {
    expect(() => syncTabStops(null)).not.toThrow();
  });
});

describe('wireTablistKeyboard', () => {
  test('ArrowRight/ArrowLeft move focus, wrap, and skip hidden tabs', () => {
    wireTablistKeyboard(buildTablist());
    tab('t1').focus();

    expect(press('ArrowRight').defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(tab('t2'));
    press('ArrowRight'); // t3 is admin-only and hidden
    expect(document.activeElement).toBe(tab('t4'));
    press('ArrowRight'); // wraps
    expect(document.activeElement).toBe(tab('t1'));
    press('ArrowLeft'); // wraps backwards
    expect(document.activeElement).toBe(tab('t4'));
  });

  test('includes admin-only tabs once they are shown', () => {
    wireTablistKeyboard(buildTablist());
    tab('t3').classList.add('visible');
    tab('t2').focus();
    press('ArrowRight');
    expect(document.activeElement).toBe(tab('t3'));
  });

  test('Home and End jump to the first and last tab', () => {
    wireTablistKeyboard(buildTablist());
    tab('t2').focus();
    press('End');
    expect(document.activeElement).toBe(tab('t4'));
    press('Home');
    expect(document.activeElement).toBe(tab('t1'));
  });

  test('arrows move focus only; selection waits for Enter/Space (manual activation)', () => {
    wireTablistKeyboard(buildTablist());
    const clicks = jest.fn();
    tab('t2').addEventListener('click', clicks);
    tab('t1').focus();
    press('ArrowRight');
    expect(document.activeElement).toBe(tab('t2'));
    expect(clicks).not.toHaveBeenCalled();
    expect(tab('t2').getAttribute('aria-selected')).toBe('false');
  });

  test('vertical lists use Up/Down and ignore Left/Right', () => {
    wireTablistKeyboard(buildTablist('vertical'));
    tab('t1').focus();
    expect(press('ArrowRight').defaultPrevented).toBe(false);
    expect(document.activeElement).toBe(tab('t1'));
    press('ArrowDown');
    expect(document.activeElement).toBe(tab('t2'));
    press('ArrowUp');
    expect(document.activeElement).toBe(tab('t1'));
  });

  test('leaves modified keys and other keys alone', () => {
    wireTablistKeyboard(buildTablist());
    tab('t1').focus();
    expect(press('ArrowRight', { altKey: true }).defaultPrevented).toBe(false);
    expect(press('ArrowRight', { shiftKey: true }).defaultPrevented).toBe(false);
    expect(press('a').defaultPrevented).toBe(false);
    expect(document.activeElement).toBe(tab('t1'));
  });

  test('ignores keys from outside the tablist', () => {
    wireTablistKeyboard(buildTablist());
    tab('outside').focus();
    expect(press('ArrowRight').defaultPrevented).toBe(false);
    expect(document.activeElement).toBe(tab('outside'));
  });
});

describe('index.html tab semantics (issue #730)', () => {
  let html: string;
  beforeAll(() => {
    html = fs.readFileSync(path.resolve(__dirname, '../index.html'), 'utf8');
  });
  beforeEach(() => {
    document.body.innerHTML = html;
  });

  test('the sidebar is page navigation, not an ARIA tablist', () => {
    const nav = document.querySelector('.app-sidebar-nav')!;
    expect(nav.tagName).toBe('NAV');
    expect(nav.hasAttribute('role')).toBe(false);
    expect(nav.querySelectorAll('[role="tab"], [aria-selected], [aria-controls]').length).toBe(0);
    const current = nav.querySelectorAll('[aria-current="page"]');
    expect(current.length).toBe(1);
    expect((current[0] as HTMLElement).dataset['tab']).toBe('home');
  });

  test('top-level pages are not tabpanels and each has a focusable page heading', () => {
    const pages = document.querySelectorAll<HTMLElement>('.tab-content');
    expect(pages.length).toBe(6);
    pages.forEach(page => {
      expect(page.getAttribute('role')).toBeNull();
      expect(page.getAttribute('aria-labelledby')).toBeNull();
      const heading = page.firstElementChild as HTMLElement;
      expect(heading.matches('h2.page-heading.sr-only')).toBe(true);
      expect(heading.getAttribute('tabindex')).toBe('-1');
      expect(heading.textContent?.trim()).toBeTruthy();
    });
  });

  test('every remaining tab has a roving tabindex and a tablist parent', () => {
    const tabs = Array.from(document.querySelectorAll<HTMLElement>('[role="tab"]'));
    expect(tabs.length).toBe(7);
    tabs.forEach(t => {
      expect(t.parentElement?.getAttribute('role')).toBe('tablist');
      expect(t.getAttribute('tabindex')).toBe(t.getAttribute('aria-selected') === 'true' ? '0' : '-1');
    });
  });
});
