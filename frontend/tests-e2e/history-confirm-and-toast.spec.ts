import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #728: History Cancel names the purchase it cancels (UX-29), and a
// toast stays while the pointer is over it (VIS-25, WCAG 2.2.1).

const ROWS = [
  { id: '11111111-1111-1111-1111-111111111111', resource_type: 'm5.large', count: 1, upfront_cost: 1200 },
  { id: '22222222-2222-2222-2222-222222222222', resource_type: 'c5.2xlarge', count: 4, upfront_cost: 9800 },
].map(r => ({
  purchase_id: r.id, timestamp: '2026-10-20T10:00:00Z', provider: 'aws', service: 'ec2', resource_type: r.resource_type,
  region: 'us-east-1', count: r.count, term: 1, upfront_cost: r.upfront_cost, monthly_cost: 0, estimated_savings: 10,
  status: 'pending', account_id: 'acct-001', created_by_user_id: 'user-smoke',
}));

async function historyPage(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: { permissions: [{ action: 'admin', resource: '*' }] } }));
  await page.route('**/api/info/deployment', route => route.fulfill({ json: {} }));
  await page.route('**/api/history**', route => route.fulfill({ json: { summary: {}, purchases: ROWS } }));
  await page.route('**/api/purchases/cancel/**', route => route.fulfill({ json: {} }));
  await page.goto('/purchases/history');
}

test('Cancel confirm names the purchase on the clicked row', async ({ page }) => {
  await historyPage(page);
  await page.locator(`#history-list .history-cancel-btn[data-cancel-id="${ROWS[1]!.purchase_id}"]`).click();

  const summary = page.getByRole('dialog').locator('.history-confirm-summary');
  await expect(summary.locator('strong')).toHaveText('AWS ec2 c5.2xlarge × 4 in us-east-1');
  await expect(summary).toContainText('Upfront: $9,800');
});

test('a toast does not expire while hovered and does once the pointer leaves', async ({ page }) => {
  await page.clock.install();
  await historyPage(page);
  await page.locator('#history-list .history-cancel-btn').first().click();
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel purchase' }).click();

  const toast = page.locator('.toast--success', { hasText: 'Purchase cancelled' });
  await expect(toast).toBeVisible();
  await toast.hover();
  await page.clock.runFor(20_000);
  await expect(toast).toBeVisible();

  await page.mouse.move(5, 5);
  await page.clock.runFor(6_000);
  await expect(toast).toHaveCount(0);
});
