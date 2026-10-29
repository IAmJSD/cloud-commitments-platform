/**
 * Wide-table scroll affordances (issue #185).
 *
 * The Opportunities table is wider than the content area at common laptop
 * widths, and it is page-tall, so its own horizontal scrollbar used to sit
 * off-screen under the sticky action box with nothing showing that columns
 * were cut off. This checks, in a real browser, that the clipped edge fades,
 * that a scrollbar strip sits on screen just above the action box and stays
 * in step with the table, and that keyboard focus keeps rows clear of the
 * action box.
 */

import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth, RECS } from './fixtures/recs';

// 40 rows so the table runs well past the bottom of the viewport.
const MANY = Array.from({ length: 40 }, (_, i) => ({
  ...RECS[i % RECS.length]!,
  id: `wide-${i}`,
  region: i % 2 ? 'ap-southeast-2' : RECS[i % RECS.length]!.region,
  count: i + 1,
}));

async function openOpportunities(page: Page, width: number): Promise<void> {
  await page.setViewportSize({ width, height: 900 });
  await seedAuth(page);
  await mockApi(page);
  await page.route(
    (url) => url.pathname === '/api/recommendations',
    (route) => (route.request().method() === 'GET'
      ? route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          recommendations: MANY,
          summary: { total_recommendations: MANY.length, total_upfront_cost: 0, potential_monthly_savings: 0, avg_payback_months: 6 },
        }),
      })
      : route.fallback()),
  );
  await page.goto('/opportunities');
  await page.waitForLoadState('networkidle');
}

const table = '#recommendations-list > table';
const strip = '#recommendations-list > .table-scroll-strip';

for (const width of [1280, 390]) {
  test(`opportunities table shows where its hidden columns are at ${width}px`, async ({ page }) => {
    await openOpportunities(page, width);

    const tbl = page.locator(table);
    await expect(tbl).toHaveClass(/scroll-hint-right/);
    await expect(tbl).not.toHaveClass(/scroll-hint-left/);

    // On a phone the page header and the KPI tiles fill the first screen, so
    // the table starts under the action box; scroll it up as a reader would.
    if (width < 768) await tbl.evaluate((el) => el.scrollIntoView({ block: 'start' }));

    // The strip is on screen, right above the action box.
    const bar = page.locator(strip);
    await expect(bar).toBeVisible();
    const barBox = (await bar.boundingBox())!;
    const actionBox = (await page.locator('#recommendations-action-box').boundingBox())!;
    expect(barBox.y + barBox.height).toBeLessThanOrEqual(actionBox.y + 1);
    expect(barBox.y + barBox.height).toBeGreaterThan(actionBox.y - 2);
    expect(barBox.y).toBeGreaterThan(0);

    // Scrolling the strip scrolls the table, and both edges fade mid-way.
    await bar.evaluate((el) => { el.scrollLeft = 120; });
    await expect.poll(() => tbl.evaluate((el) => el.scrollLeft)).toBe(120);
    await expect(tbl).toHaveClass(/scroll-hint-left/);
    await expect(tbl).toHaveClass(/scroll-hint-right/);

    // Scrolling the table (trackpad, Shift+wheel) moves the strip too; at the
    // far end only the left edge fades.
    await tbl.evaluate((el) => { el.scrollLeft = el.scrollWidth; });
    const end = await tbl.evaluate((el) => el.scrollLeft);
    await expect.poll(() => bar.evaluate((el) => el.scrollLeft)).toBe(end);
    await expect(tbl).not.toHaveClass(/scroll-hint-right/);

    // Still no page-level horizontal scroll (#703).
    expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0);
  });
}

test('keyboard focus scrolls a row clear of the action box', async ({ page }) => {
  await openOpportunities(page, 1280);
  const actionTop = async (): Promise<number> => (await page.locator('#recommendations-action-box').boundingBox())!.y;

  // A row checkbox that starts underneath the action box.
  const boxes = page.locator(`${table} tbody input[type="checkbox"]`);
  const top = await actionTop();
  let target = -1;
  for (let i = 0; i < await boxes.count(); i++) {
    const b = await boxes.nth(i).boundingBox();
    if (b && b.y > top) { target = i; break; }
  }
  expect(target).toBeGreaterThan(0);

  await boxes.nth(target - 1).focus();
  await page.keyboard.press('Tab');
  // Tab may land on the row's Plan button first; either way the focused
  // control must end up above the strip and the action box (the page
  // scrolls smoothly, hence the poll).
  await expect.poll(async () => {
    const focusedBottom = await page.evaluate(() => document.activeElement!.getBoundingClientRect().bottom);
    const stripTop = (await page.locator(strip).boundingBox())!.y;
    return focusedBottom <= Math.min(stripTop, await actionTop());
  }).toBe(true);
});

test('a table that fits gets no fade and no strip', async ({ page }) => {
  await openOpportunities(page, 1920);
  const tbl = page.locator(table);
  expect(await tbl.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  await expect(page.locator('.scroll-hint-left, .scroll-hint-right')).toHaveCount(0);
  await expect(page.locator(strip)).toBeHidden();
});
