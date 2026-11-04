import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// A percentage max-width is ignored by auto table layout, so a 100-char plan
// name must be capped with a plain length to keep the table in its wrapper (#1680).
test('a 100-char plan name keeps Planned Purchases inside its wrapper', async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1000 });
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [{
    id: 'pp-1', plan_id: 'plan-1', plan_name: 'RDS-'.repeat(25), scheduled_date: '2026-11-01T00:00:00Z',
    provider: 'aws', service: 'ec2', resource_type: 'm5.large', region: 'us-east-1', count: 3, term: 1,
    payment: 'all-upfront', estimated_savings: 1234, upfront_cost: 25000, has_recommendations: true,
    status: 'pending', step_number: 1, total_steps: 2, created_by_user_id: 'user-smoke',
  }] } }));
  await page.goto('/plans');
  await expect(page.locator('#planned-purchases-list tbody tr.planned-purchase-row')).toHaveCount(1);
  const fits = await page.evaluate(() => {
    const table = document.querySelector('#planned-purchases-list table') as HTMLElement;
    const wrap = table.parentElement as HTMLElement;
    return { scroll: wrap.scrollWidth, client: wrap.clientWidth };
  });
  expect(fits.scroll).toBeLessThanOrEqual(fits.client + 1);
});
