import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #213: editing a group that holds a blank-resource permission must
// explain why Save is refused (instead of reloading the page or widening
// the row to All (*)), and must save once the row is fixed.
const GROUP = {
  id: 'g2', name: 'Finance', description: 'Finance team', allowed_accounts: [],
  permissions: [
    { action: 'view', resource: 'plans' },
    { action: 'view', resource: '', constraints: { accounts: ['acct-A'] } },
  ],
};

test('blank permission resource: Save explains, then saves the fixed row', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('cudly:purchaser-prompt-dismissed', '1'));
  await seedAuth(page);
  await mockApi(page);
  const puts: unknown[] = [];
  await page.route('**/api/users', route => route.fulfill({ json: { users: [] } }));
  await page.route('**/api/groups', route => route.fulfill({ json: { groups: [GROUP] } }));
  await page.route('**/api/groups/g2', route => {
    if (route.request().method() === 'PUT') puts.push(route.request().postDataJSON());
    return route.fulfill({ json: GROUP });
  });

  await page.goto('/admin/users');
  await page.locator('#groups-list .edit-group-btn').first().click();
  const modal = page.locator('#group-modal');
  await expect(modal).toBeVisible();
  const blankRow = modal.locator('.permission-item').nth(1);
  await expect(blankRow.locator('.perm-resource')).toHaveValue('');

  await page.locator('#group-name').fill('Finance (renamed)');
  await modal.locator('button[type="submit"]').click();
  await expect(modal).toBeVisible();
  await expect(blankRow.locator('.field-error'))
    .toHaveText('This permission has no resource. Choose one, or remove the permission.');
  await expect(blankRow.locator('.perm-resource')).toBeFocused();
  await expect(blankRow.locator('.perm-resource')).toHaveAttribute('aria-invalid', 'true');
  expect(puts).toHaveLength(0);

  await blankRow.locator('.perm-resource').selectOption('purchases');
  await expect(blankRow.locator('.field-error')).toHaveCount(0);
  await modal.locator('button[type="submit"]').click();
  await expect(modal).toBeHidden();
  expect(puts).toEqual([{
    name: 'Finance (renamed)',
    description: 'Finance team',
    permissions: [
      { action: 'view', resource: 'plans' },
      { action: 'view', resource: 'purchases', constraints: { accounts: ['acct-A'] } },
    ],
  }]);
});
