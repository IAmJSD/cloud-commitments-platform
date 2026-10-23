/**
 * Wide-table scroll affordance tests (issue #185).
 *
 * jsdom has no layout engine, so each test stubs the size and scroll
 * properties the module reads. The real rendering (fade, sticky strip above
 * the action box) is covered by tests-e2e/table-scroll-affordance.spec.ts.
 */

import {
  HINT_LEFT,
  HINT_RIGHT,
  STRIP_CLASS,
  handleScroll,
  keepFocusClear,
  refreshTableScroll,
  scrollerFor,
  syncScrollStrip,
  updateScrollHint,
} from '../lib/table-scroll';

interface Size {
  scrollWidth?: number;
  clientWidth?: number;
  offsetWidth?: number;
  offsetHeight?: number;
  clientHeight?: number;
}

function stubSize(el: HTMLElement, size: Size): void {
  for (const [key, value] of Object.entries(size)) {
    Object.defineProperty(el, key, { configurable: true, value });
  }
}

function hints(el: HTMLElement): string[] {
  return [HINT_LEFT, HINT_RIGHT].filter((c) => el.classList.contains(c));
}

function mountRecommendationsTable(): HTMLTableElement {
  document.body.innerHTML = '<section id="recommendations-list"><table><tbody><tr><td>x</td></tr></tbody></table></section>';
  const table = document.querySelector('table')!;
  table.style.overflowX = 'auto';
  return table;
}

describe('scrollerFor', () => {
  afterEach(() => { document.body.innerHTML = ''; });

  test('returns the table when the table itself scrolls', () => {
    document.body.innerHTML = '<div><table></table></div>';
    const table = document.querySelector('table')!;
    table.style.overflowX = 'auto';
    expect(scrollerFor(table)).toBe(table);
  });

  test('returns the direct container when it scrolls', () => {
    document.body.innerHTML = '<div style="overflow-x: auto"><table></table></div>';
    const table = document.querySelector('table')!;
    expect(scrollerFor(table)).toBe(table.parentElement);
  });

  test('returns null when neither scrolls', () => {
    document.body.innerHTML = '<div><table></table></div>';
    expect(scrollerFor(document.querySelector('table')!)).toBeNull();
  });
});

describe('updateScrollHint', () => {
  test('a table that fits gets no hint', () => {
    const el = document.createElement('div');
    stubSize(el, { scrollWidth: 500, clientWidth: 500 });
    updateScrollHint(el);
    expect(hints(el)).toEqual([]);
  });

  test('at the left edge only the right side is hinted', () => {
    const el = document.createElement('div');
    stubSize(el, { scrollWidth: 1400, clientWidth: 1000, offsetWidth: 1000, offsetHeight: 300, clientHeight: 288 });
    updateScrollHint(el);
    expect(hints(el)).toEqual([HINT_RIGHT]);
    // A horizontal scrollbar's height is exported so the fade skips it.
    expect(el.style.getPropertyValue('--scroll-hint-sb-h')).toBe('12px');
    expect(el.style.getPropertyValue('--scroll-hint-sb-w')).toBe('0px');
  });

  test('in the middle both sides are hinted, at the end only the left', () => {
    const el = document.createElement('div');
    stubSize(el, { scrollWidth: 1400, clientWidth: 1000 });
    el.scrollLeft = 200;
    updateScrollHint(el);
    expect(hints(el)).toEqual([HINT_LEFT, HINT_RIGHT]);
    el.scrollLeft = 400;
    updateScrollHint(el);
    expect(hints(el)).toEqual([HINT_LEFT]);
  });

  test('hints are dropped once the table fits again', () => {
    const el = document.createElement('div');
    stubSize(el, { scrollWidth: 1400, clientWidth: 1000 });
    updateScrollHint(el);
    stubSize(el, { scrollWidth: 1400, clientWidth: 1400 });
    updateScrollHint(el);
    expect(hints(el)).toEqual([]);
  });
});

describe('syncScrollStrip', () => {
  afterEach(() => { document.body.innerHTML = ''; });

  test('adds one aria-hidden strip right after the table, spanning the same scroll range', () => {
    const table = mountRecommendationsTable();
    stubSize(table, { scrollWidth: 1400, clientWidth: 1000 });
    const strip = syncScrollStrip(table);
    stubSize(strip, { clientWidth: 1000 });
    syncScrollStrip(table);

    expect(table.nextElementSibling).toBe(strip);
    expect(document.querySelectorAll(`.${STRIP_CLASS}`)).toHaveLength(1);
    expect(strip.getAttribute('aria-hidden')).toBe('true');
    expect(strip.tabIndex).toBe(-1);
    expect(strip.hidden).toBe(false);
    expect((strip.firstElementChild as HTMLElement).style.width).toBe('1400px');
  });

  test('hides the strip while the table fits', () => {
    const table = mountRecommendationsTable();
    stubSize(table, { scrollWidth: 1000, clientWidth: 1000 });
    expect(syncScrollStrip(table).hidden).toBe(true);
  });

  test('scrolling the strip scrolls the table and the other way round', () => {
    const table = mountRecommendationsTable();
    stubSize(table, { scrollWidth: 1400, clientWidth: 1000 });
    const strip = syncScrollStrip(table);

    strip.scrollLeft = 150;
    handleScroll(strip);
    expect(table.scrollLeft).toBe(150);

    table.scrollLeft = 320;
    handleScroll(table);
    expect(strip.scrollLeft).toBe(320);
  });

  test('a scroll on a hinted table refreshes its edge hints', () => {
    const table = mountRecommendationsTable();
    stubSize(table, { scrollWidth: 1400, clientWidth: 1000 });
    updateScrollHint(table);
    table.scrollLeft = 400;
    handleScroll(table);
    expect(hints(table)).toEqual([HINT_LEFT]);
  });
});

describe('refreshTableScroll', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    document.documentElement.style.removeProperty('--action-box-offset');
  });

  test('hints scrollers, adds the Opportunities strip and records the action box height', () => {
    const table = mountRecommendationsTable();
    stubSize(table, { scrollWidth: 1400, clientWidth: 1000 });
    const box = document.createElement('div');
    box.id = 'recommendations-action-box';
    stubSize(box, { offsetHeight: 136 });
    document.body.appendChild(box);

    refreshTableScroll();

    expect(hints(table)).toEqual([HINT_RIGHT]);
    expect(table.nextElementSibling?.classList.contains(STRIP_CLASS)).toBe(true);
    expect(document.documentElement.style.getPropertyValue('--action-box-offset')).toBe('136px');
  });

  test('other tables get the hint but no strip', () => {
    document.body.innerHTML = '<div id="history-list" style="overflow-x: auto"><table></table></div>';
    const wrap = document.getElementById('history-list')!;
    stubSize(wrap, { scrollWidth: 900, clientWidth: 400 });
    refreshTableScroll();
    expect(hints(wrap)).toEqual([HINT_RIGHT]);
    expect(document.querySelector(`.${STRIP_CLASS}`)).toBeNull();
  });

  test('a hidden action box leaves the last measured offset alone', () => {
    document.documentElement.style.setProperty('--action-box-offset', '93px');
    const box = document.createElement('div');
    box.id = 'recommendations-action-box';
    stubSize(box, { offsetHeight: 0 });
    document.body.appendChild(box);
    refreshTableScroll();
    expect(document.documentElement.style.getPropertyValue('--action-box-offset')).toBe('93px');
  });
});

describe('keepFocusClear', () => {
  let scrollBy: jest.SpyInstance;

  function rect(top: number, bottom: number): DOMRect {
    return { top, bottom, left: 0, right: 0, width: 0, height: bottom - top, x: 0, y: top, toJSON: () => ({}) } as DOMRect;
  }

  function mount(rowTop: number, keyboard = true): HTMLInputElement {
    document.body.innerHTML = `
      <header class="app-topbar"></header>
      <section id="recommendations-list"><table><tbody><tr><td><input type="checkbox"></td></tr></tbody></table>
        <div class="table-scroll-strip"><div></div></div></section>
      <div id="recommendations-action-box"></div>`;
    const input = document.querySelector('input')!;
    jest.spyOn(input, 'getBoundingClientRect').mockReturnValue(rect(rowTop, rowTop + 20));
    jest.spyOn(input, 'matches').mockImplementation((sel: string) => (sel === ':focus-visible' ? keyboard : false));
    jest.spyOn(document.querySelector('.app-topbar')!, 'getBoundingClientRect').mockReturnValue(rect(0, 56));
    jest.spyOn(document.querySelector('.table-scroll-strip')!, 'getBoundingClientRect').mockReturnValue(rect(706, 720));
    const box = document.getElementById('recommendations-action-box')!;
    stubSize(box, { offsetHeight: 80 });
    jest.spyOn(box, 'getBoundingClientRect').mockReturnValue(rect(720, 800));
    return input;
  }

  beforeEach(() => {
    Object.defineProperty(window, 'innerHeight', { configurable: true, value: 800 });
    scrollBy = jest.spyOn(window, 'scrollBy').mockImplementation(() => undefined);
  });
  afterEach(() => {
    scrollBy.mockRestore();
    document.body.innerHTML = '';
  });

  test('a row focused under the strip and action box is scrolled above them', () => {
    keepFocusClear(mount(700));
    // Row bottom 720 vs strip top 706: 14px plus the 16px margin.
    expect(scrollBy).toHaveBeenCalledWith(0, 30);
  });

  test('a row focused under the topbar is scrolled below it', () => {
    keepFocusClear(mount(40));
    expect(scrollBy).toHaveBeenCalledWith(0, -32);
  });

  test('a row in the clear, a mouse click, or focus outside the list does nothing', () => {
    keepFocusClear(mount(300));
    keepFocusClear(mount(700, false));
    const outside = document.createElement('button');
    document.body.appendChild(outside);
    keepFocusClear(outside);
    expect(scrollBy).not.toHaveBeenCalled();
  });
});
