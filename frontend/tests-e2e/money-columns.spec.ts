import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #739: money and count columns right-align with tabular digits, and
// a yearly figure groups its thousands instead of printing "$123456".
test('Opportunities money columns are right-aligned and yearly amounts keep separators', async ({ page }) => {
  await mockApi(page);
  await seedAuth(page);
  const big = {
    id: 'big', provider: 'aws', cloud_account_id: 'acct-001', service: 'ec2', resource_type: 'm5.24xlarge',
    region: 'us-east-1', count: 12, term: 3, payment: 'all-upfront', savings: 10288, upfront_cost: 250000,
    monthly_cost: 0, on_demand_monthly: 18000, effective_savings_pct: 57,
  };
  await page.route(/\/api\/recommendations(?:\?.*)?$/, route => route.fulfill({ json: {
    recommendations: [big], summary: { total_recommendations: 1, total_upfront_cost: 250000, potential_monthly_savings: 10288, avg_payback_months: 24 }, regions: ['us-east-1'],
  } }));
  await page.goto('/opportunities');
  const row = page.locator('tr.recommendation-row[data-rec-id="big"]');
  await expect(row).toBeVisible();

  for (const cell of [row.locator('td.savings'), row.locator('td.num').first()]) {
    await expect(cell).toHaveCSS('text-align', 'right');
    await expect(cell).toHaveCSS('font-variant-numeric', 'tabular-nums');
  }
  await expect(page.locator('th.sortable[data-sort="upfront_cost"]')).toHaveCSS('text-align', 'right');
  await expect(page.locator('th.sortable[data-sort="region"]')).toHaveCSS('text-align', 'left');

  await page.locator('#cost-period-select').selectOption('yearly');
  await expect(page.locator('tr.recommendation-row[data-rec-id="big"] td.savings')).toHaveText('$123,456');
});
