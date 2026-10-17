import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

for (const width of [320, 390]) test(`plan card actions stay inside the viewport at ${width}px`, async ({ page }, testInfo) => {
  await page.setViewportSize({ width, height: 900 });
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1' ? route.fallback() : route.abort());
  await seedAuth(page); await mockApi(page);
  const plan = { id: 'p1', name: 'Plan', enabled: true, auto_purchase: false, notification_days_before: 3, services: {},
    ramp_schedule: { type: 'weekly', current_step: 1, total_steps: 4, step_interval_days: 7, percent_per_step: 25 } };
  await page.route('**/api/plans**', route => route.fulfill({ json: { plans: [plan] } }));
  await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [] } }));
  await page.goto('/plans');
  const buttons = page.locator('.plan-actions button');
  await expect(buttons).toHaveCount(4);
  await page.locator('.plan-actions').scrollIntoViewIfNeeded();
  await page.screenshot({ path: testInfo.outputPath('plan-actions.png') });
  for (const button of await buttons.all()) {
    const box = (await button.boundingBox())!;
    expect(box.x).toBeGreaterThanOrEqual(0); expect(box.x + box.width).toBeLessThanOrEqual(width);
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  const card = (await page.locator('.plan-card').boundingBox())!;
  for (const button of await buttons.all()) expect((await button.boundingBox())!.x + (await button.boundingBox())!.width).toBeLessThanOrEqual(card.x + card.width);
});
