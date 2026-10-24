/**
 * Issue #20: the Opportunities action box evaluated its permission gating
 * once, when the box was first mounted. An admin who added themselves to the
 * Purchaser group in Admin -> Users and came back to Opportunities still saw
 * the no-Purchaser banner and no Purchase button until a full reload.
 */

import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const ADMINS = '00000000-0000-5000-8000-000000000001';
const PURCHASER = '00000000-0000-5000-8000-000000000007';

async function json(page: Page, url: string, body: () => unknown): Promise<void> {
  await page.route(url, route => route.fulfill({
    status: 200, contentType: 'application/json', body: JSON.stringify(body()),
  }));
}

/** Admin who is not yet a Purchaser; flips to Purchaser once the PUT lands. */
async function mockAdminWithoutPurchaser(page: Page): Promise<void> {
  let purchaser = false;
  const groups = () => (purchaser ? [ADMINS, PURCHASER] : [ADMINS]);
  await page.addInitScript(() => {
    // Suppress the one-time "Purchaser group: separation of duties" prompt.
    localStorage.setItem('cudly:purchaser-prompt-dismissed', '1');
  });
  await json(page, '**/api/auth/me', () => ({ id: 'user-smoke', email: 'smoke@example.com', groups: groups() }));
  await json(page, '**/api/auth/me/permissions', () => ({
    permissions: purchaser
      ? [{ action: 'admin', resource: '*' }, { action: 'execute', resource: 'purchases' }]
      : [{ action: 'admin', resource: '*' }],
    is_admin: true,
  }));
  await json(page, '**/api/users', () => ({
    users: [{ id: 'user-smoke', email: 'smoke@example.com', groups: groups(), mfa_enabled: false }],
  }));
  await json(page, '**/api/groups', () => ({
    groups: [
      { id: ADMINS, name: 'Administrators', description: '', permissions: [{ action: 'admin', resource: '*' }], allowed_accounts: [] },
      { id: PURCHASER, name: 'Purchaser', description: '', permissions: [{ action: 'execute', resource: 'purchases' }], allowed_accounts: [] },
    ],
  }));
  await page.route('**/api/users/user-smoke', async route => {
    if (route.request().method() === 'PUT') purchaser = true;
    await route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });
}

test('adding yourself to Purchaser reveals Purchase on Opportunities without a reload', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await mockAdminWithoutPurchaser(page);

  await page.goto('/opportunities');
  const box = page.locator('#recommendations-action-box');
  await expect(box).toBeVisible();
  await expect(box.locator('.info-banner')).toHaveCount(1);
  await expect(page.locator('#bulk-purchase-btn')).toBeHidden();

  await page.locator('#admin-tab-btn').click();
  await page.locator('.sub-tab-btn[data-settings-tab="users"]').click();
  await page.locator('.user-expand-btn[data-user-id="user-smoke"]').click();
  await page.locator(`.group-assign-checkbox[data-user-id="user-smoke"][data-group-id="${PURCHASER}"]`).check();
  await page.locator('.modal-confirm').getByRole('button', { name: 'Add to group' }).click();
  await expect(page.locator('.toast-success').filter({ hasText: 'Added smoke@example.com' })).toBeVisible();

  await page.locator('#opportunities-tab-btn').click();
  await expect(page.locator('#bulk-purchase-btn')).toBeVisible();
  await expect(box.locator('.info-banner')).toHaveCount(0);
  // Same document throughout: the fix must not depend on a reload.
  expect(await page.evaluate(() => performance.getEntriesByType('navigation').length)).toBe(1);
});
