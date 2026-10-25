import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #579: the RI-exchange approval email links to an SPA route that asks
// for confirmation and then POSTs the token. Landing on the link must not act.
const ID = '22222222-2222-2222-2222-222222222222';
const RECORD = {
  id: ID, account_id: 'acct-001', exchange_id: '', region: 'us-east-1', source_ri_ids: ['ri-1'],
  source_instance_type: 'm5.large', source_count: 2, target_offering_id: 'off-1', target_instance_type: 'm6i.large',
  target_count: 2, payment_due: '12.34', status: 'pending', mode: 'manual',
  created_at: '2026-10-20T10:00:00Z', updated_at: '2026-10-20T10:00:00Z',
};

async function fixture(page: Page): Promise<{ posts: string[] }> {
  await seedAuth(page); await mockApi(page);
  const posts: string[] = [];
  await page.route('**/api/ri-exchange/history**', route => route.fulfill({ json: { records: [RECORD] } }));
  await page.route(/\/api\/ri-exchange\/(approve|reject)\//, async route => {
    if (route.request().method() !== 'POST') return route.fulfill({ status: 405, json: { error: 'method not allowed' } });
    posts.push(route.request().url());
    return route.fulfill({ json: { status: 'ok' } });
  });
  return { posts };
}

for (const [action, label, toast] of [
  ['approve', 'Approve exchange', 'RI exchange approved and executing.'],
  ['reject', 'Reject exchange', 'RI exchange rejected.'],
] as const) {
  test(`${action} link: confirm first, then POST the token and land on the RI Exchange tab`, async ({ page }) => {
    const { posts } = await fixture(page);
    await page.goto(`/inventory/ri-exchange/${action}/${ID}?token=tok-${action}`);

    const dialog = page.locator('.modal-confirm-backdrop');
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText('2x m5.large -> 2x m6i.large in us-east-1');
    expect(posts).toHaveLength(0);

    await dialog.getByRole('button', { name: label }).click();
    await expect(page.getByText(toast)).toBeVisible();
    expect(posts).toHaveLength(1);
    const url = new URL(posts[0]!);
    expect(url.pathname).toBe(`/api/ri-exchange/${action}/${ID}`);
    expect(url.searchParams.get('token')).toBe(`tok-${action}`);
    await expect(page).toHaveURL(/\/inventory\/ri-exchange$/);
    await expect(page.locator('#inventory-ri-exchange')).toBeVisible();
  });
}

test('dismissing the dialog sends nothing', async ({ page }) => {
  const { posts } = await fixture(page);
  await page.goto(`/inventory/ri-exchange/approve/${ID}?token=tok-1`);
  await page.locator('.modal-confirm-backdrop').getByRole('button', { name: 'Not now' }).click();
  await expect(page).toHaveURL(/\/inventory\/ri-exchange$/);
  expect(posts).toHaveLength(0);
});

test('a signed-out visitor keeps the deep link across the login prompt', async ({ page }) => {
  await mockApi(page);
  await page.route('**/api/info', route => route.fulfill({ json: { admin_exists: true } }));
  await page.goto(`/inventory/ri-exchange/approve/${ID}?token=tok-1`);
  await expect(page.locator('#login-form')).toBeVisible();
  // The router must not rewrite the path before login, or the reload after
  // signing in would lose the action and id.
  expect(new URL(page.url()).pathname).toBe(`/inventory/ri-exchange/approve/${ID}`);
  expect(new URL(page.url()).searchParams.get('token')).toBe('tok-1');
});
