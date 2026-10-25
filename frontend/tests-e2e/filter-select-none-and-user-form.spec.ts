import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #727 UX-28: a categorical column filter's footer button selects
// nothing (hiding every row), so it reads "Select none", not "Clear".
test('categorical column filter footer reads "Select none" and selects nothing', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.goto('/opportunities');
  const btn = page.locator('th .column-filter-btn[data-column="provider"]');
  await btn.click();
  const popover = page.locator('.column-filter-popover');
  await expect(popover.locator('.column-filter-clear')).toHaveText('Select none');
  await popover.locator('.column-filter-clear').click();
  await expect(popover.locator('input[type="checkbox"]:checked')).toHaveCount(0);
});

// Issue #727 UX-33: the user form explains problems next to the fields.
test('user form shows field-level errors and focuses the first one', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('cudly:purchaser-prompt-dismissed', '1'));
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/users', route => route.fulfill({ json: { users: [] } }));
  await page.route('**/api/groups', route => route.fulfill({ json: { groups: [
    { id: 'g1', name: 'Finance', description: '', permissions: [{ action: 'view', resource: 'plans' }], allowed_accounts: [] },
  ] } }));
  await page.goto('/admin/users');
  await page.locator('#create-user-btn').click();
  const modal = page.locator('#user-modal');
  await expect(modal).toBeVisible();
  await page.locator('#user-email').fill('not-an-email');
  await modal.locator('button[type="submit"]').click();

  await expect(page.locator('#user-email')).toHaveAttribute('aria-invalid', 'true');
  await expect(page.locator('#user-email')).toBeFocused();
  await expect(modal.locator('.field-error').first()).toHaveText('Enter a valid email address, like name@example.com.');
  await expect(page.locator('#user-groups')).toHaveAttribute('aria-invalid', 'true');
  await expect(modal.locator('.field-error')).toHaveCount(2);

  await page.locator('#user-email').fill('ann@example.com');
  await expect(page.locator('#user-email')).not.toHaveAttribute('aria-invalid', 'true');
  await expect(modal.locator('.field-error')).toHaveCount(1);
});
