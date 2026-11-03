import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const RESOURCES = [
  'recommendations', 'plans', 'purchases', 'accounts', 'users', 'groups', 'api-keys',
  'config', 'history', 'ladder', 'coverage', 'exchanges', 'reports', 'budgets', 'audit',
];
const LONG = 'resource-with-a-very-long-name-that-forces-the-row-label-to-wrap-onto-several-lines-in-a-narrow-modal-';

async function openCreateKey(page: Page, resources: string[]): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/users*', route => route.fulfill({ json: { users: [] } }));
  await page.route('**/api/groups*', route => route.fulfill({ json: { groups: [] } }));
  await page.route('**/api/api-keys/usage-stats', route => route.fulfill({ json: {
    total_active: 0, total_requests_window: 0, total_requests_lifetime: 0, lifetime_partial: false, top_keys: [],
  } }));
  await page.route('**/api/api-keys', route => route.fulfill({ json: { api_keys: [] } }));
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: {
    permissions: resources.map(resource => ({ action: 'view', resource })), is_admin: true,
  } }));
  await page.goto('/admin/users');
  await page.locator('#create-apikey-btn').click();
  await expect(page.locator('#apikey-permissions-list input[type="checkbox"]')).toHaveCount(resources.length);
}

// The permission box is a fixed-height scroll area. Overlay scrollbars give no
// cue that more rows exist, and a cue based on cutting a row mid-way breaks with
// touch-sized or wrapped rows. The box must show a shadow on its bottom edge
// while rows remain below, and none once scrolled to the end.
// Rows are hidden so only the box background is compared. Strips sit 3px inside
// the border and clear of the scrollbar, so sub-pixel layout cannot pull either in.
async function bottomStrip(page: Page, scrollTo: 'top' | 'end'): Promise<Buffer> {
  const clip = await page.evaluate((to) => {
    const list = document.getElementById('apikey-permissions-list')!;
    list.querySelectorAll<HTMLElement>('.apikey-permission-option').forEach(r => { r.style.visibility = 'hidden'; });
    list.scrollTo({ top: to === 'end' ? list.scrollHeight : 0, behavior: 'instant' });
    const b = list.getBoundingClientRect();
    return { overflows: list.scrollHeight > list.clientHeight, x: b.x + 24, width: b.width - 48, y: b.bottom - 12, height: 8 };
  }, scrollTo);
  expect(clip.overflows).toBe(true);
  return page.screenshot({ clip: { x: clip.x, y: clip.y, width: clip.width, height: clip.height } });
}

async function topStripAtTop(page: Page): Promise<Buffer> {
  const clip = await page.evaluate(() => {
    const list = document.getElementById('apikey-permissions-list')!;
    list.scrollTo({ top: 0, behavior: 'instant' });
    const b = list.getBoundingClientRect();
    return { x: b.x + 24, width: b.width - 48, y: b.top + 4, height: 8 };
  });
  return page.screenshot({ clip });
}

const cases = [
  { name: '1440x900 desktop', viewport: { width: 1440, height: 900 }, touch: false, resources: RESOURCES },
  { name: '390x844 phone', viewport: { width: 390, height: 844 }, touch: false, resources: RESOURCES },
  { name: '390x844 touch phone (44px rows)', viewport: { width: 390, height: 844 }, touch: true, resources: RESOURCES },
  { name: '390x844 touch phone, wrapped rows', viewport: { width: 390, height: 844 }, touch: true,
    resources: RESOURCES.map((r, i) => (i % 2 ? `${LONG}${r}` : r)) },
];

for (const fontPx of [15, 16, 17]) for (const c of cases) {
  test.describe(`${c.name} at ${fontPx}px root font`, () => {
    test.use({ viewport: c.viewport, hasTouch: c.touch, isMobile: c.touch });

    test('API key permission list shows a bottom scroll shadow only while rows remain below', async ({ page }) => {
      await openCreateKey(page, c.resources);
      await page.evaluate(px => { document.documentElement.style.fontSize = `${px}px`; }, fontPx);
      const plain = await topStripAtTop(page);
      const atTop = await bottomStrip(page, 'top');
      const atEnd = await bottomStrip(page, 'end');
      expect(atTop.equals(atEnd)).toBe(false);
      expect(atEnd.equals(plain)).toBe(true);
    });
  });
}

// Park a checkbox flush with each edge of the scroll box, Tab onto it and
// check its 2px outline plus 2px offset has room inside the box. scroll-padding
// makes the browser leave a margin instead of scrolling the ring flush.
test.describe('focus ring inside the scroll box', () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  for (const edge of ['bottom', 'top'] as const) {
    test(`Tab onto a checkbox parked flush with the ${edge} edge keeps its focus ring inside the list`, async ({ page }) => {
      await openCreateKey(page, RESOURCES);
      const k = 9;
      const m = await page.evaluate(({ k, edge }) => {
        const list = document.getElementById('apikey-permissions-list')!;
        const box = (i: number) => document.getElementById(`apikey-perm-${i}`)!;
        const prev = box(edge === 'bottom' ? k - 1 : k + 1);
        prev.focus();
        const target = box(k);
        const l = list.getBoundingClientRect();
        const border = parseFloat(getComputedStyle(list).borderTopWidth);
        const r = target.getBoundingClientRect();
        const shift = edge === 'bottom'
          ? r.bottom - (l.top + border + list.clientHeight)
          : r.top - (l.top + border);
        list.scrollTo({ top: list.scrollTop + shift, behavior: 'instant' });
        return { shift };
      }, { k, edge });
      expect(Math.abs(m.shift)).toBeGreaterThan(0);
      await page.keyboard.press(edge === 'bottom' ? 'Tab' : 'Shift+Tab');
      const room = await page.evaluate(() => {
        const list = document.getElementById('apikey-permissions-list')!;
        const el = document.activeElement as HTMLElement;
        const cs = getComputedStyle(el);
        const ring = parseFloat(cs.outlineWidth) + parseFloat(cs.outlineOffset);
        const r = el.getBoundingClientRect();
        const l = list.getBoundingClientRect();
        const b = parseFloat(getComputedStyle(list).borderBottomWidth);
        return {
          id: el.id,
          focusVisible: el.matches(':focus-visible'),
          ring,
          spaceBelow: (l.bottom - b) - (r.bottom + ring),
          spaceAbove: (r.top - ring) - (l.top + b),
        };
      });
      expect(room.id).toBe(`apikey-perm-${k}`);
      expect(room.focusVisible).toBe(true);
      expect(room.ring).toBeGreaterThan(0);
      // Without scroll padding Chromium scrolls the ring exactly flush with the
      // edge (0px to spare); require breathing room so it is not clipped by
      // sub-pixel rounding or a thicker ring.
      expect(room.spaceBelow).toBeGreaterThanOrEqual(4);
      expect(room.spaceAbove).toBeGreaterThanOrEqual(4);
    });
  }
});
