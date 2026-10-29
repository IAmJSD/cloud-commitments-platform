import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #725 (VIS-20 / VIS-24), measured in a real browser.
test.beforeEach(async ({ page }) => {
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1' ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
});

test('the rounded table clips its header corners', async ({ page }) => {
  await page.goto('/inventory/coverage');
  const table = page.locator('#inventory-coverage table').first();
  await expect(table).toBeVisible();
  const styles = await table.evaluate((el) => {
    const t = getComputedStyle(el);
    const th = getComputedStyle(el.querySelector('th')!);
    return { collapse: t.borderCollapse, overflow: t.overflowX, radius: t.borderTopLeftRadius, transform: th.textTransform };
  });
  // border-radius only takes effect in the separate model, and the clip keeps
  // the header's square background inside the rounded corner.
  // Headers render in the case they are written in (Title Case); uppercase
  // only ever reached the non-sortable ones, so tables mixed the two.
  expect(styles).toEqual({ collapse: 'separate', overflow: 'clip', radius: '8px', transform: 'none' });
  // The top-left corner pixel of the table sits outside the rounded edge, so
  // the header background must not paint there.
  const box = (await table.boundingBox())!;
  const corner = await page.screenshot({ clip: { x: box.x, y: box.y, width: 1, height: 1 } });
  const inside = await page.screenshot({ clip: { x: box.x + 12, y: box.y + 12, width: 1, height: 1 } });
  expect(corner.equals(inside)).toBe(false);
});

test('the selected sidebar item keeps the same weight as the others', async ({ page }) => {
  await page.goto('/opportunities');
  const active = page.locator('#opportunities-tab-btn');
  await expect(active).toHaveClass(/active/);
  const weights = await page.evaluate(() => ['#opportunities-tab-btn', '#plans-tab-btn'].map(
    (id) => getComputedStyle(document.querySelector(id)!).fontWeight,
  ));
  expect(weights[0]).toBe(weights[1]);
});

test('high-savings rows keep their left stripe', async ({ page }) => {
  await page.goto('/opportunities');
  const cell = page.locator('#recommendations-list tr.high-savings > :first-child').first();
  await expect(cell).toBeVisible();
  expect(await cell.evaluate((el) => getComputedStyle(el).boxShadow)).toContain('inset');
});
