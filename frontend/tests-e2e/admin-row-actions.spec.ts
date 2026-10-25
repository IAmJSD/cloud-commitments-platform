import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #751 review feedback: the admin page's row actions are compact SVG
// icon buttons. API key Revoke/Delete sit side by side instead of stacking,
// and the group editor's Remove permission button lines up with the selects
// instead of stretching to the height of label + select.

const GROUP = {
  id: 'g-ops', name: 'Ops', description: '', allowed_accounts: [],
  permissions: [{ action: 'view', resource: 'plans' }, { action: 'update', resource: 'purchases' }],
};

async function openAdminUsers(page: Page): Promise<void> {
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1' ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/api-keys', route => route.fulfill({ json: { api_keys: [
    { id: 'k1', name: 'ci', key_prefix: 'cud_a1', is_active: true, created_at: '2026-09-01T10:00:00Z' },
  ] } }));
  await page.route('**/api/api-keys/usage-stats', route => route.fulfill({ json: { total_active: 1, total_requests_window: 0, total_requests_lifetime: 0, top_keys: [] } }));
  await page.route('**/api/users', route => route.fulfill({ json: { users: [] } }));
  await page.route('**/api/groups', route => route.fulfill({ json: { groups: [GROUP] } }));
  await page.route('**/api/groups/g-ops', route => route.fulfill({ json: GROUP }));
  await page.goto('/admin/users');
}

test('API key Revoke and Delete are icon buttons side by side', async ({ page }) => {
  await openAdminUsers(page);
  const revoke = page.getByRole('button', { name: 'Revoke API key' });
  const del = page.getByRole('button', { name: 'Delete API key' });
  await expect(revoke).toBeVisible();
  await expect(revoke.locator('svg.icon-slash')).toHaveCount(1);
  await expect(del.locator('svg.icon-trash-2')).toHaveCount(1);
  const r = (await revoke.boundingBox())!;
  const d = (await del.boundingBox())!;
  expect(Math.abs(r.y - d.y)).toBeLessThanOrEqual(1);
  expect(d.x).toBeGreaterThan(r.x + r.width);
  expect(d.x - (r.x + r.width)).toBeLessThanOrEqual(8);
  expect(r.height).toBeLessThanOrEqual(32);
});

test('Remove permission is a small icon button aligned with the selects', async ({ page }) => {
  await openAdminUsers(page);
  await page.locator('.edit-group-btn[data-group-id="g-ops"]').click();
  const rows = page.locator('#permissions-list .permission-item');
  await expect(rows).toHaveCount(2);
  const removes = page.getByRole('button', { name: 'Remove permission' });
  await expect(removes).toHaveCount(2);
  for (const row of await rows.all()) {
    const sel = (await row.locator('select.perm-resource').boundingBox())!;
    const btn = (await row.getByRole('button', { name: 'Remove permission' }).boundingBox())!;
    expect(Math.abs(btn.y - sel.y)).toBeLessThanOrEqual(1);
    expect(Math.abs(btn.height - sel.height)).toBeLessThanOrEqual(1);
    expect(btn.width).toBeLessThanOrEqual(btn.height + 1);
    expect(btn.x).toBeGreaterThan(sel.x + sel.width);
  }
  await removes.first().click();
  await expect(rows).toHaveCount(1);
});
