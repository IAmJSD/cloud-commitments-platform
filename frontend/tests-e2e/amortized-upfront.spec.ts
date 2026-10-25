import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #318: with amortize on, the upfront is already spread into Monthly
// Cost, so the Upfront cell is muted and labeled instead of reading as a
// separate lump sum on top of the amortized monthly figure.
test('amortize toggle labels the upfront as included in the monthly cost', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  const purchase = {
    purchase_id: '11111111-1111-1111-1111-111111111111', timestamp: new Date().toISOString(),
    provider: 'aws', service: 'ec2', resource_type: 'm5.large', region: 'us-east-1',
    count: 1, term: 1, upfront_cost: 1200, monthly_cost: 100, estimated_savings: 300,
    offering_class: 'convertible', status: 'completed', account_id: 'account-1',
  };
  await page.route('**/api/history**', route => route.fulfill({ json: { summary: {}, purchases: [purchase] } }));

  await page.goto('/purchases/history');
  const row = page.locator('#history-list tbody tr').first();
  const upfront = row.locator('td').nth(8);
  await expect(upfront).toHaveText('$1,200');
  await expect(upfront).not.toHaveClass(/upfront-in-monthly/);

  await page.locator('#history-amortize-checkbox').check();
  await expect(row.locator('td').nth(9)).toHaveText('$200');
  await expect(upfront).toHaveClass(/upfront-in-monthly/);
  await expect(upfront).toHaveAttribute('title', 'Included in the amortized monthly cost');
  await expect(upfront).toContainText('(in monthly)');

  await page.locator('#history-amortize-checkbox').uncheck();
  await expect(upfront).toHaveText('$1,200');
});
