import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #676: when GET /api/history reports has_more, Load more fetches the
// next page with the cursor and appends it under the first page's rows.
function row(id: string, status: string, daysAgo: number) {
  return {
    purchase_id: `${id}-0000-0000-0000-000000000000`, timestamp: new Date(Date.now() - daysAgo * 86400000).toISOString(),
    provider: 'aws', service: 'ec2', resource_type: 'm5.large', region: 'us-east-1', count: 1, term: 1,
    upfront_cost: 0, monthly_cost: 10, estimated_savings: 2, status, account_id: 'acct-001',
  };
}

test('Load more appends the next history page and hides once has_more is false', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await seedAuth(page);
  await mockApi(page);
  const cursors: (string | null)[] = [];
  await page.route('**/api/history**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname !== '/api/history') return route.fallback();
    const cursor = url.searchParams.get('cursor');
    cursors.push(cursor);
    await route.fulfill({ json: cursor === 'page-2'
      ? { summary: { total_purchases: 1 }, purchases: [row('cccccccc', 'failed', 3)], truncated: false, has_more: false }
      : { summary: { total_purchases: 3 }, purchases: [row('aaaaaaaa', 'pending', 1), row('bbbbbbbb', 'completed', 2)],
        truncated: true, limit: 2, executions_limit: 2, has_more: true, next_cursor: 'page-2' } });
  });

  await page.goto('/purchases/history');
  const rows = page.locator('#history-list tbody tr');
  await expect(rows).toHaveCount(2);
  const button = page.locator('#history-load-more-btn');
  await expect(button).toBeVisible();
  await expect(page.locator('#history-truncation-notice')).toBeHidden();

  await button.click();
  await expect(rows).toHaveCount(3);
  await expect(page.locator('#history-list tbody tr[data-execution-id^="cccccccc"]')).toHaveCount(1);
  await expect(button).toBeHidden();
  expect(cursors.filter(c => c !== null)).toEqual(['page-2']);
  await expect(page.locator('#history-summary')).toContainText('3');
  expect(errors).toEqual([]);
});
