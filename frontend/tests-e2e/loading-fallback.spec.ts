import { test, expect } from '@playwright/test';
import { mockApi, seedAuth, RECS, SUMMARY } from './fixtures/recs';

// Issue #186: the Opportunities skeleton is visible, a skeleton that sits for
// a long time gets a "Still loading" note so a slow response does not look
// like a hang, and the note goes away with the skeleton once data arrives.

test('slow recommendations load shows a still-loading note, then the table', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  let release: () => void = () => undefined;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  await page.route(/\/api\/recommendations(?:\?.*)?$/, async (route) => {
    await gate;
    await route.fulfill({ json: { recommendations: RECS, summary: SUMMARY, regions: [] } });
  });
  await page.clock.install();
  await page.goto('/opportunities');

  const list = page.locator('#recommendations-list');
  await expect(list.locator('.skeleton-row')).toHaveCount(5);
  // The shimmer must actually be visible: the Opportunities table is
  // display:block for scrolling, which used to collapse skeleton cells to 0px.
  const box = await list.locator('.skeleton-row .skeleton').first().boundingBox();
  expect(box!.width).toBeGreaterThan(16);
  await expect(list.locator('.skeleton-status')).toHaveText('Loading…');
  await expect(list.locator('.skeleton-slow-note')).toHaveCount(0);

  await page.clock.fastForward(8_000);
  await expect(list.locator('.skeleton-slow-note')).toHaveText('Still loading. This is taking longer than usual.');

  release();
  await expect(list.locator('tr.recommendation-row').first()).toBeVisible();
  await expect(list.locator('.skeleton-slow-note')).toHaveCount(0);
  await expect(list.locator('.skeleton-status')).toHaveCount(0);
});
