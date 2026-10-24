import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #182: when /api/users or /api/groups fails, the Admin > Users
// sections must say so in place (with a Retry) instead of sitting as empty
// boxes that look like "no users", and Retry must bring the lists back.
test('admin users sections show an inline error with Retry, then the lists', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  let fail = true;
  await page.route('**/api/users', route => fail
    ? route.fulfill({ status: 500, json: { error: 'database unavailable' } })
    : route.fulfill({ json: { users: [{ id: 'u2', email: 'alice@example.com', groups: ['g2'], mfa_enabled: true }] } }));
  await page.route('**/api/groups', route => route.fulfill({ json: { groups: [
    { id: 'g2', name: 'Finance', permissions: [{ action: 'view', resource: 'plans' }], allowed_accounts: [] },
  ] } }));

  await page.goto('/admin/users');
  await expect(page.locator('#users-list .error')).toHaveText('Could not load users: database unavailable');
  await expect(page.locator('#groups-list .error')).toHaveText('Could not load groups: database unavailable');
  await expect(page.locator('#permission-matrix .error'))
    .toHaveText('Could not load the permission overview: database unavailable');
  // Each Retry sits below its error box with a visible gap, not flush on it.
  for (const id of ['users-list', 'groups-list', 'permission-matrix']) {
    const box = await page.locator(`#${id} .error`).boundingBox();
    const btn = await page.locator(`#${id} button[data-action="retry-load-users"]`).boundingBox();
    expect(box && btn).toBeTruthy();
    expect(btn!.y - (box!.y + box!.height)).toBeGreaterThanOrEqual(12);
  }

  fail = false;
  await page.locator('#groups-list button[data-action="retry-load-users"]').click();
  await expect(page.locator('#users-list')).toContainText('alice@example.com');
  await expect(page.locator('#groups-list .group-card')).toContainText('Finance');
  await expect(page.locator('#permission-matrix table')).toBeVisible();
  await expect(page.locator('#users-section .error')).toHaveCount(0);
});
