/**
 * Inventory & Coverage -> Trends (issue #30) against the real bundle.
 *
 * jsdom covers the data shaping and DOM; this checks what only a browser
 * can: the sub-tab is reachable and routed, the chart canvas actually gets
 * laid out, and the account chip reaches GET /api/analytics/trends.
 */

import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

test('Trends sub-tab renders the series, breakdowns and a laid-out chart', async ({ page }) => {
  await seedAuth(page);
  const { calls } = await mockApi(page);
  await page.goto('/inventory/coverage');

  await page.locator('#inv-subtab-trends').click();
  await expect(page).toHaveURL(/\/inventory\/trends$/);
  await expect(page.locator('#inv-subtab-trends')).toHaveAttribute('aria-selected', 'true');

  // 6 months by default; the third-latest month is a gap, not $0.
  const monthly = page.locator('#trends-monthly-table tbody tr');
  await expect(monthly).toHaveCount(6);
  await expect(monthly.nth(5).locator('td').nth(1)).toHaveText('$2,040');
  await expect(monthly.nth(3).locator('td').nth(1)).toHaveText('--');
  await expect(page.locator('#trends-provider-table tbody tr')).toHaveCount(3);
  await expect(page.locator('#trends-service-table tbody tr')).toHaveCount(4);

  const box = await page.locator('#trends-savings-chart').boundingBox();
  expect(box?.height ?? 0).toBeGreaterThan(100);
  expect(box?.width ?? 0).toBeGreaterThan(200);

  await page.locator('#trends-period').selectOption('12');
  await expect(monthly).toHaveCount(12);

  const trendCalls = calls.filter((c) => c.url.includes('/api/analytics/trends'));
  expect(trendCalls.length).toBeGreaterThanOrEqual(2);
  const qs = new URL(trendCalls[trendCalls.length - 1]!.url).searchParams;
  expect(qs.get('start')).toMatch(/-01T00:00:00\.000Z$/);
  expect(qs.has('provider')).toBe(false);
});

test('Trends shows the backend refusal inline instead of an empty page', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/analytics/trends**', (route) =>
    route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'analytics snapshots not configured' }) }),
  );
  await page.goto('/inventory/trends');
  await expect(page.locator('#trends-content p.error')).toContainText('not available on this deployment');
});
