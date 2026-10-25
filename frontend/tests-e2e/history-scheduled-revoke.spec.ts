import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #924: a scheduled execution has not called any cloud SDK yet, so the
// backend revokes it for free on every provider. AWS and GCP scheduled rows
// must offer Revoke too; completed AWS and GCP rows still must not.

const FUTURE = () => new Date(Date.now() + 86_400_000).toISOString();

function row(id: string, provider: string, status: string) {
  return {
    purchase_id: id, timestamp: new Date().toISOString(), provider,
    service: 'compute', resource_type: 'm5.large', region: 'us-east-1', count: 1, term: 1,
    upfront_cost: 1200, monthly_cost: 50, estimated_savings: 10,
    status, account_id: 'acct-001', created_by_user_id: 'user-smoke',
    revocation_window_closes_at: FUTURE(),
  };
}

async function fixture(page: Page) {
  await seedAuth(page);
  await mockApi(page);
  const rows = [
    row('aaaaaaaa-0000-0000-0000-000000000001', 'aws', 'scheduled'),
    row('aaaaaaaa-0000-0000-0000-000000000002', 'gcp', 'scheduled'),
    row('aaaaaaaa-0000-0000-0000-000000000003', 'aws', 'completed'),
  ];
  const posts: string[] = [];
  const quoteGets: string[] = [];
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: { permissions: [
    { action: 'admin', resource: '*' }, { action: 'revoke-any', resource: 'purchases' },
  ] } }));
  await page.route('**/api/info/deployment', route => route.fulfill({ json: {} }));
  await page.route('**/api/history**', route => route.fulfill({ json: { summary: {}, purchases: rows } }));
  await page.route('**/api/purchases/**', async route => {
    if (route.request().method() === 'POST') {
      posts.push(route.request().url());
      await route.fulfill({ json: { status: 'canceled' } });
    } else {
      quoteGets.push(route.request().url());
      await route.fulfill({ json: { refund_amount: 1, refund_currency: 'USD' } });
    }
  });
  return { posts, quoteGets };
}

test('scheduled AWS and GCP rows offer Revoke; completed AWS rows do not', async ({ page }) => {
  await fixture(page);
  await page.goto('/purchases/history');
  const ids = page.locator('#history-list .history-revoke-btn');
  await expect(ids).toHaveCount(2);
  expect(await ids.evaluateAll(btns => btns.map(b => (b as HTMLElement).dataset['revokeId']))).toEqual([
    'aaaaaaaa-0000-0000-0000-000000000001',
    'aaaaaaaa-0000-0000-0000-000000000002',
  ]);
});

test('revoking a scheduled AWS row cancels it without a refund quote or Azure copy', async ({ page }) => {
  const { posts, quoteGets } = await fixture(page);
  await page.goto('/purchases/history');
  await page.locator('#history-list .history-revoke-btn[data-revoke-id="aaaaaaaa-0000-0000-0000-000000000001"]').click();
  const dialog = page.locator('.modal-confirm-backdrop');
  await expect(dialog).toContainText('cancel the scheduled purchase');
  await expect(dialog).not.toContainText('Azure');
  await page.locator('.modal-confirm-actions button').last().click();
  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0]).toContain('/purchases/aaaaaaaa-0000-0000-0000-000000000001/revoke');
  expect(quoteGets.filter(u => u.includes('/calculate'))).toEqual([]);
});
