/**
 * Issue #744: the Home charts are <canvas role="img">, so without a text
 * alternative a screen-reader user can't read any value. Each chart now has a
 * data-driven aria-label and a keyboard-operable "View as table" disclosure.
 */
import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth, RECS } from './fixtures/recs';

async function openHome(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/dashboard/upcoming', (route) => route.fulfill({ json: { purchases: [] } }));
  await page.route('**/api/dashboard/summary**', (route) =>
    route.fulfill({
      json: {
        potential_monthly_savings: 0, total_recommendations: RECS.length, active_commitments: 0,
        committed_monthly: 0, current_coverage: 0, target_coverage: 80, ytd_savings: 0, by_service: {},
      },
    }),
  );
  await page.route('**/api/history/analytics**', (route) =>
    route.fulfill({
      json: {
        summary: {},
        data_points: [
          { timestamp: new Date(Date.now() - 2 * 86400_000).toISOString(), total_savings: 40, cumulative_savings: 40 },
          { timestamp: new Date(Date.now() - 86400_000).toISOString(), total_savings: 60, cumulative_savings: 100 },
        ],
      },
    }),
  );
  await page.goto('/home');
}

test('the per-service chart has a value summary and a keyboard-operable table', async ({ page }) => {
  await openHome(page);
  const services = new Set(RECS.map((r) => r.service));

  const canvas = page.locator('#savings-by-service-chart');
  await expect(canvas).toHaveAttribute('aria-label', new RegExp(`for ${services.size} services`));

  const disclosure = page.locator('#savings-by-service-table');
  const table = disclosure.locator('table');
  await expect(table).toBeHidden();

  await disclosure.locator('summary').focus();
  await page.keyboard.press('Enter');
  await expect(table).toBeVisible();
  await expect(table.locator('tbody tr')).toHaveCount(services.size);
  for (const svc of services) {
    await expect(table.getByRole('rowheader', { name: svc, exact: true })).toBeVisible();
  }
});

test('the savings trend chart has a value summary and a table', async ({ page }) => {
  await openHome(page);
  await expect(page.locator('#savings-trend-chart')).toHaveAttribute('aria-label', /\$100 as of .*2 data points/);
  const disclosure = page.locator('#savings-trend-table');
  await disclosure.locator('summary').click();
  await expect(disclosure.locator('tbody tr')).toHaveCount(2);
});
