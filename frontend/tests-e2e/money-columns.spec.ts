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

// The planned-purchases table restates text-align: left for its own cells,
// so its money and count columns need their own right-align (issue #739).
test('Plans money and count columns are right-aligned', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [{
    id: 'pp-1', plan_id: 'plan-1', plan_name: 'Ramp', scheduled_date: '2026-11-01T00:00:00Z',
    provider: 'aws', service: 'ec2', resource_type: 'm5.large', region: 'us-east-1', count: 3, term: 1,
    payment: 'all-upfront', estimated_savings: 1234, upfront_cost: 25000, has_recommendations: true,
    status: 'pending', step_number: 1, total_steps: 2, created_by_user_id: 'user-smoke',
  }] } }));
  await page.goto('/plans');
  const row = page.locator('#planned-purchases-list tbody tr.planned-purchase-row');
  await expect(row).toHaveCount(1);
  await expect(row.locator('td.num')).toHaveCount(3);
  for (const cell of await row.locator('td.num').all()) {
    await expect(cell).toHaveCSS('text-align', 'right');
    await expect(cell).toHaveCSS('font-variant-numeric', 'tabular-nums');
  }
  await expect(page.locator('#planned-purchases-list th.num')).toHaveCount(3);
  await expect(page.locator('#planned-purchases-list th.num').first()).toHaveCSS('text-align', 'right');
  await expect(row.locator('td').nth(2)).toHaveCSS('text-align', 'left');
});
