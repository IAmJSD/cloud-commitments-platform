/**
 * Phone action bar (PR #1616 follow-up): with a full selection the Purchase
 * and Plan labels ("Purchase 19 selected") must wrap onto a second line at
 * 390px instead of ending in an ellipsis, and the bar must not cover the
 * last row once the page is scrolled to the end.
 */

import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

test('phone action-bar labels wrap rather than truncate, last row stays clear', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await seedAuth(page);
  await mockApi(page);
  await page.goto('/opportunities');
  await page.waitForLoadState('networkidle');

  await page.locator('#select-all-recs').check();
  const purchase = page.locator('#bulk-purchase-btn');
  const plan = page.locator('#create-plan-btn');
  await expect(purchase).toHaveText('Purchase 19 selected');
  await expect(plan).toHaveText('Plan from 19 selected');

  for (const btn of [purchase, plan]) {
    const m = await btn.evaluate((el) => {
      const cs = getComputedStyle(el);
      return {
        scrollW: el.scrollWidth, clientW: el.clientWidth, height: el.getBoundingClientRect().height,
        ellipsis: cs.textOverflow === 'ellipsis' && cs.overflow === 'hidden' && el.scrollWidth > el.clientWidth,
      };
    });
    expect(m.scrollW).toBeLessThanOrEqual(m.clientW);
    expect(m.ellipsis).toBe(false);
    expect(m.height).toBeGreaterThanOrEqual(36);
  }

  await page.evaluate(() => window.scrollTo({ top: document.documentElement.scrollHeight, behavior: 'instant' as ScrollBehavior }));
  const lastRow = page.locator('#recommendations-list tbody tr').last();
  await lastRow.scrollIntoViewIfNeeded();
  await page.evaluate(() => window.scrollTo({ top: document.documentElement.scrollHeight, behavior: 'instant' as ScrollBehavior }));
  const rowBox = (await lastRow.boundingBox())!;
  const barBox = (await page.locator('#recommendations-action-box').boundingBox())!;
  expect(rowBox.y + rowBox.height).toBeLessThanOrEqual(barBox.y + 1);
});
