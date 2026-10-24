/**
 * Home "Savings over time" sparse-data state (issue #188).
 *
 * With one data point the chart painted a single dot on an otherwise blank
 * 300px canvas. The widget now hides the canvas and says there is not enough
 * history yet, while still showing the value it has. Two or more points draw
 * the chart as before.
 */
import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const DAY = 86400_000;

async function homeWithPoints(page: Page, count: number): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/dashboard/summary**', route => route.fulfill({ json: {
    potential_monthly_savings: 100, total_recommendations: 3, active_commitments: 1, committed_monthly: 1.1,
    current_coverage: 10, target_coverage: 80, ytd_savings: 1.1, by_service: {},
  } }));
  await page.route('**/api/dashboard/upcoming', route => route.fulfill({ json: { purchases: [] } }));
  await page.route('**/api/history**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname !== '/api/history/analytics') {
      await route.fulfill({ json: [] });
      return;
    }
    const firstDay = Math.floor(Date.now() / DAY) * DAY - 3 * DAY + 12 * 3600_000;
    await route.fulfill({ json: {
      start: url.searchParams.get('start'), end: url.searchParams.get('end'), interval: url.searchParams.get('interval'),
      summary: { total_monthly_savings: 1.1 * count, total_annual_savings: 13.2 * count, total_purchases: count },
      data_points: Array.from({ length: count }, (_, i) => ({
        timestamp: new Date(firstDay + i * DAY).toISOString(),
        total_savings: 1.1, cumulative_savings: 1.1 * (i + 1), total_upfront: 0, purchase_count: 1,
      })),
    } });
  });
  await page.goto('/home');
}

test('a single data point shows the sparse-data message instead of a blank chart', async ({ page }) => {
  await homeWithPoints(page, 1);
  const message = page.locator('#savings-trend-empty');
  await expect(message).toBeVisible();
  await expect(message).toContainText('Not enough history yet to show a trend. So far: $1.10 in cumulative savings as of');
  await expect(page.locator('#savings-trend-chart')).toBeHidden();
  // The message keeps the card's height so the layout below doesn't jump.
  const box = await page.locator('#savings-trend-section').boundingBox();
  expect(box?.height ?? 0).toBeGreaterThan(200);
});

test('two data points draw the chart and hide the sparse-data message', async ({ page }) => {
  await homeWithPoints(page, 2);
  await expect(page.locator('#savings-trend-chart')).toBeVisible();
  await expect(page.locator('#savings-trend-empty')).toBeHidden();
});
