/**
 * Admin -> Purchasing policies section links and collapsed provider grids
 * (issue #190).
 */
import fs from 'fs';
import path from 'path';

import {
  initPurchasingNav,
  refreshPurchasingNav,
  isSectionAvailable,
  resetPurchasingNavForTests,
} from '../purchasing-nav';

const flushObserver = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

function buildPanel(): void {
  document.body.innerHTML = `
    <section id="purchasing-panel">
      <nav id="purchasing-section-nav">
        <a href="#purchasing-global-defaults">Defaults</a>
        <a href="#aws-settings">AWS services</a>
        <a href="#azure-settings">Azure services</a>
        <a href="#ri-exchange-automation-settings">Exchange automation</a>
        <a href="#does-not-exist">Gone</a>
      </nav>
      <fieldset id="purchasing-global-defaults"><legend>Global</legend></fieldset>
      <fieldset id="aws-settings" class="provider-settings">
        <legend>AWS</legend>
        <details class="service-defaults-details"><summary>Per-service</summary><select id="aws-ec2-term"></select></details>
      </fieldset>
      <fieldset id="azure-settings" class="provider-settings hidden"><legend>Azure</legend></fieldset>
      <div id="ri-exchange-automation-settings"></div>
    </section>`;
}

const link = (href: string): HTMLAnchorElement =>
  document.querySelector<HTMLAnchorElement>(`#purchasing-section-nav a[href="${href}"]`)!;

describe('purchasing section nav', () => {
  let scrollSpy: jest.Mock;

  beforeEach(() => {
    resetPurchasingNavForTests();
    buildPanel();
    scrollSpy = jest.fn();
    Element.prototype.scrollIntoView = scrollSpy;
    window.history.replaceState(null, '', '/admin/purchasing');
  });

  test('hides links to hidden, empty or missing sections', () => {
    initPurchasingNav();
    expect(link('#purchasing-global-defaults').hidden).toBe(false);
    expect(link('#aws-settings').hidden).toBe(false);
    expect(link('#azure-settings').hidden).toBe(true);
    expect(link('#ri-exchange-automation-settings').hidden).toBe(true);
    expect(link('#does-not-exist').hidden).toBe(true);
  });

  test('tracks provider toggles and late-rendered sections without extra calls', async () => {
    initPurchasingNav();
    document.getElementById('azure-settings')!.classList.remove('hidden');
    document.getElementById('ri-exchange-automation-settings')!.appendChild(document.createElement('fieldset'));
    await flushObserver();
    expect(link('#azure-settings').hidden).toBe(false);
    expect(link('#ri-exchange-automation-settings').hidden).toBe(false);

    // Exchange automation is cleared for a caller without access (#1413).
    document.getElementById('ri-exchange-automation-settings')!.replaceChildren();
    document.getElementById('aws-settings')!.classList.add('hidden');
    await flushObserver();
    expect(link('#ri-exchange-automation-settings').hidden).toBe(true);
    expect(link('#aws-settings').hidden).toBe(true);
  });

  test('a click scrolls to the section, opens its collapsed grid, moves focus and leaves the URL alone', () => {
    initPurchasingNav();
    const aws = document.getElementById('aws-settings')!;
    const details = aws.querySelector('details')!;
    expect(details.open).toBe(false);

    const ev = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0 });
    link('#aws-settings').dispatchEvent(ev);

    expect(ev.defaultPrevented).toBe(true);
    expect(details.open).toBe(true);
    expect(scrollSpy).toHaveBeenCalledTimes(1);
    expect(scrollSpy.mock.instances[0]).toBe(aws);
    expect(document.activeElement).toBe(aws);
    expect(aws.getAttribute('tabindex')).toBe('-1');
    expect(window.location.hash).toBe('');
    expect(window.location.pathname).toBe('/admin/purchasing');
  });

  test('modified clicks fall through to the browser', () => {
    initPurchasingNav();
    const ev = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, ctrlKey: true });
    link('#aws-settings').dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(false);
    expect(scrollSpy).not.toHaveBeenCalled();
  });

  test('repeated init binds the click handler once', () => {
    initPurchasingNav();
    initPurchasingNav();
    initPurchasingNav();
    link('#purchasing-global-defaults').click();
    expect(scrollSpy).toHaveBeenCalledTimes(1);
  });

  test('is a no-op without the nav', () => {
    document.body.innerHTML = '';
    expect(() => { initPurchasingNav(); refreshPurchasingNav(); }).not.toThrow();
    expect(isSectionAvailable(null)).toBe(false);
  });
});

describe('index.html purchasing panel (issue #190)', () => {
  const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
  const doc = new DOMParser().parseFromString(html, 'text/html');
  const panel = doc.getElementById('purchasing-panel')!;

  test('every section link points at an element inside the panel', () => {
    const links = Array.from(panel.querySelectorAll<HTMLAnchorElement>('#purchasing-section-nav a'));
    expect(links.length).toBeGreaterThanOrEqual(10);
    for (const a of links) {
      const id = a.getAttribute('href')!.slice(1);
      expect(panel.querySelector(`#${id}`)).not.toBeNull();
    }
  });

  test.each(['aws-settings', 'azure-settings', 'gcp-settings'])(
    '%s keeps its service grid in a collapsed <details> inside the form panel',
    (id) => {
      const details = panel.querySelector(`#${id} details.service-defaults-details`) as HTMLDetailsElement | null;
      expect(details).not.toBeNull();
      expect(details!.open).toBe(false);
      expect(details!.querySelector('summary')).not.toBeNull();
      expect(details!.querySelector('.service-defaults-grid select')).not.toBeNull();
    },
  );
});
