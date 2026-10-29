/**
 * Sub-tab bars on narrow screens: the selected tab is scrolled into view
 * (horizontally only) and the bar gets the wide-table edge fade.
 * jsdom has no layout, so the sizes are stubbed.
 */
import { revealSelectedTab, initSubtabScroll, resetSubtabScrollForTests } from '../nav/subtab-scroll';

function stub(el: HTMLElement, props: Record<string, number>): void {
  for (const [k, v] of Object.entries(props)) {
    Object.defineProperty(el, k, { configurable: true, get: () => v });
  }
}

function buildBar(): { bar: HTMLElement; tabs: HTMLElement[] } {
  document.body.innerHTML = `
    <div class="settings-tabs" role="tablist">
      <button role="tab" aria-selected="false">General</button>
      <button role="tab" aria-selected="false">Purchasing policies</button>
      <button role="tab" aria-selected="false">Accounts</button>
      <button role="tab" aria-selected="true">Users, roles &amp; API keys</button>
    </div>`;
  const bar = document.querySelector<HTMLElement>('.settings-tabs')!;
  const tabs = Array.from(bar.querySelectorAll<HTMLElement>('[role="tab"]'));
  stub(bar, { offsetLeft: 16, clientWidth: 300, scrollWidth: 640, offsetWidth: 300, offsetHeight: 40, clientHeight: 40 });
  tabs.forEach((t, i) => stub(t, { offsetLeft: 16 + i * 160, offsetWidth: 160 }));
  return { bar, tabs };
}

describe('sub-tab bar scrolling', () => {
  beforeEach(() => resetSubtabScrollForTests());

  test('scrolls a selected tab past the right edge into view', () => {
    const { bar } = buildBar();
    bar.scrollLeft = 0;
    revealSelectedTab(bar);
    // Last tab spans 480..640 inside the bar; the 300px viewport ends there.
    expect(bar.scrollLeft).toBe(340);
  });

  test('brings a cut-off middle tab to the start edge, where snapping settles', () => {
    const { bar, tabs } = buildBar();
    tabs[3]!.setAttribute('aria-selected', 'false');
    tabs[2]!.setAttribute('aria-selected', 'true');
    bar.scrollLeft = 0;
    revealSelectedTab(bar);
    expect(bar.scrollLeft).toBe(320);
  });

  test('leaves the bar alone when the selected tab is already visible', () => {
    const { bar, tabs } = buildBar();
    tabs[3]!.setAttribute('aria-selected', 'false');
    tabs[0]!.setAttribute('aria-selected', 'true');
    bar.scrollLeft = 0;
    revealSelectedTab(bar);
    expect(bar.scrollLeft).toBe(0);
  });

  test('marks a bar with hidden tabs on the right with the edge fade', () => {
    const { bar, tabs } = buildBar();
    tabs[3]!.setAttribute('aria-selected', 'false');
    tabs[0]!.setAttribute('aria-selected', 'true');
    bar.scrollLeft = 0;
    initSubtabScroll();
    expect(bar.classList.contains('scroll-hint-right')).toBe(true);
    expect(bar.classList.contains('scroll-hint-left')).toBe(false);
  });
});
