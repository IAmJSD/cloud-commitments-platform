import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #183: the Approval queue is the only approval surface. A pending row
// in Purchase History links to its queue row instead of repeating the buttons.
test('pending history row links to its Approval queue row', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await seedAuth(page);
  await mockApi(page);
  const ID = '22222222-2222-2222-2222-222222222222';
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: { permissions:
    ['admin', 'approve-any', 'cancel-any'].map(verb => ({ action: verb, resource: verb === 'admin' ? '*' : 'purchases' })),
  } }));
  await page.route('**/api/info/deployment', route => route.fulfill({ json: {} }));
  await page.route('**/api/history**', async route => {
    if (new URL(route.request().url()).pathname !== '/api/history') return route.fallback();
    await route.fulfill({ json: { summary: {}, purchases: [{
      purchase_id: ID, timestamp: new Date().toISOString(), provider: 'aws', service: 'ec2', resource_type: 'm5.large',
      region: 'us-east-1', count: 1, term: 1, upfront_cost: 0, monthly_cost: 50, estimated_savings: 10,
      status: 'pending', account_id: 'acct-001', created_by_user_id: 'user-smoke',
    }] } });
  });

  await page.goto('/purchases/history');
  const historyRow = page.locator(`#history-list tr[data-execution-id="${ID}"]`);
  await expect(historyRow).toHaveCount(1);
  await expect(historyRow.locator('.history-approve-btn, .history-cancel-btn')).toHaveCount(0);
  const queueRow = page.locator(`#purchases-approval-queue tr[data-execution-id="${ID}"]`);
  await expect(queueRow.locator('.history-approve-btn')).toHaveCount(1);
  await expect(queueRow.locator('.history-cancel-btn')).toHaveCount(1);

  await historyRow.getByRole('button', { name: /Review in approval queue/ }).click();
  await expect(queueRow.locator('.history-approve-btn')).toBeFocused();
  await expect(queueRow).toHaveClass(/history-row-highlight/);
  expect(errors).toEqual([]);
});
