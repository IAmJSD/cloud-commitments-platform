import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

test('unknown upfront remains unknown through history and inventory amortization toggles', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await seedAuth(page);
  await mockApi(page);
  const purchase = {
    purchase_id: '11111111-1111-1111-1111-111111111111', timestamp: new Date().toISOString(),
    provider: 'aws', service: 'ec2', resource_type: 'm5.large', region: 'us-east-1',
    count: 1, term: 1, upfront_cost: null, monthly_cost: 50, estimated_savings: 10,
    offering_class: 'standard', status: 'completed', account_id: 'account-1',
  };
  await page.route('**/api/history**', route => route.fulfill({ json: { summary: { total_upfront: null }, purchases: [purchase] } }));
  await page.route('**/api/inventory/commitments**', route => route.fulfill({ json: { commitments: [{
    ...purchase, id: 'account-1:purchase', term_years: 1, start_date: purchase.timestamp,
    end_date: '2027-09-29T00:00:00Z', status: 'active',
  }] } }));

  await page.goto('/purchases/history');
  const history = page.locator('#history-list tbody tr').first();
  await expect(history.locator('td').nth(8)).toHaveText('\u2014');
  await expect(history.locator('td').nth(9)).toHaveText('$50');
  await page.locator('#history-amortize-checkbox').check();
  await expect(history.locator('td').nth(9)).toHaveText('\u2014');
  await page.locator('#history-amortize-checkbox').uncheck();
  await expect(history.locator('td').nth(9)).toHaveText('$50');

  await page.goto('/inventory/active-commitments');
  const inventory = page.locator('#active-commitments-list tbody tr').first();
  await expect(inventory.locator('td').nth(8)).toHaveText('$50');
  await page.locator('#inventory-amortize-checkbox').check();
  await expect(inventory.locator('td').nth(8)).toHaveText('\u2014');
  expect(errors).toEqual([]);
});
