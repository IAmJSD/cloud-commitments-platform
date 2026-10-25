import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #919: system-managed groups are refused by the API on edit and
// delete, so their cards must not offer those actions.
test('system-managed group cards hide Edit and Delete', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('cudly:purchaser-prompt-dismissed', '1'));
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/users', route => route.fulfill({ json: { users: [] } }));
  await page.route('**/api/groups', route => route.fulfill({ json: { groups: [
    { id: '00000000-0000-5000-8000-000000000001', name: 'Administrators', description: '',
      permissions: [{ action: 'admin', resource: '*' }], allowed_accounts: [], system_managed: true },
    { id: 'g-custom', name: 'Finance', description: '',
      permissions: [{ action: 'view', resource: 'plans' }], allowed_accounts: [] },
  ] } }));

  await page.goto('/admin/users');
  const system = page.locator('.group-card', { has: page.locator('h4', { hasText: 'Administrators' }) });
  const custom = page.locator('.group-card', { has: page.locator('h4', { hasText: 'Finance' }) });
  await expect(system.locator('.system-group-badge')).toBeVisible();
  await expect(system.locator('.edit-group-btn')).toHaveCount(0);
  await expect(system.locator('.delete-group-btn')).toHaveCount(0);
  await expect(system.locator('.duplicate-group-btn')).toBeVisible();
  await expect(custom.locator('.edit-group-btn')).toBeVisible();
  await expect(custom.locator('.delete-group-btn')).toBeVisible();
});
