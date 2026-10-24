import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #8 stage 1: each per-account service override row shows when it was
// last updated, under the service name, without widening the page.
const OVERRIDES = [
  { id: 'o1', account_id: 'acct-001', provider: 'aws', service: 'ec2', enabled: true, term: 3, payment: 'no-upfront', coverage: 70,
    created_at: '2026-03-02T09:10:00Z', updated_at: '2026-10-21T14:05:00Z' },
  { id: 'o2', account_id: 'acct-001', provider: 'aws', service: 'rds', enabled: true, term: 1, coverage: 85 },
];

for (const width of [1280, 390]) test(`override rows show their last-updated time at ${width}px`, async ({ page }) => {
  await page.setViewportSize({ width, height: 800 });
  await seedAuth(page); await mockApi(page);
  await page.route('**/api/accounts/*/service-overrides', route => route.fulfill({ json: OVERRIDES }));
  await page.goto('/admin/accounts');
  await page.locator('#aws-accounts-list button', { hasText: 'Overrides' }).first().click();

  const rows = page.locator('#account-overrides-modal-body table.overrides-table tbody tr');
  await expect(rows).toHaveCount(2);
  const stamp = rows.nth(0).locator('td').first().locator('time.last-updated-stamp');
  await expect(stamp).toBeVisible();
  await expect(stamp).toHaveText(/^Updated Oct 2[12], 2026/);
  await expect(stamp).toHaveAttribute('datetime', '2026-10-21T14:05:00.000Z');
  await expect(stamp).toHaveAttribute('title', 'Last updated 2026-10-21 14:05 UTC');
  // A row the API returned without a timestamp gets no stamp.
  await expect(rows.nth(1).locator('.last-updated-stamp')).toHaveCount(0);

  if (width === 1280) {
    // One line where there is room: no taller than a single line of its own text.
    const box = (await stamp.boundingBox())!;
    const lineHeight = await stamp.evaluate(el => parseFloat(getComputedStyle(el).lineHeight) || parseFloat(getComputedStyle(el).fontSize) * 1.5);
    expect(box.height).toBeLessThanOrEqual(lineHeight + 1);
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});
