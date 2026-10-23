import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const CREATED_KEY = 'cudly_e2e_key_123';

// Issue #705: a failed clipboard write on the one-time API key modal used a
// native alert(). It now shows an inline role="alert" message and selects the
// key so the user can copy it by hand without leaving the modal.
test('API key copy failure is reported inline, not with a native alert', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: () => Promise.reject(new Error('Clipboard blocked')) },
    });
  });
  await page.route('**/api/users*', route => route.fulfill({ json: { users: [] } }));
  await page.route('**/api/groups*', route => route.fulfill({ json: { groups: [] } }));
  await page.route('**/api/api-keys/usage-stats', route => route.fulfill({ json: {
    total_active: 0, total_requests_window: 0, total_requests_lifetime: 0, lifetime_partial: false, top_keys: [],
  } }));
  await page.route('**/api/api-keys', route => {
    if (route.request().method() === 'POST') {
      return route.fulfill({ json: { api_key: CREATED_KEY, key_id: 'k1', info: { id: 'k1', name: 'CI key' } } });
    }
    return route.fulfill({ json: { api_keys: [] } });
  });
  await page.route('**/api/auth/me/permissions', route => route.fulfill({ json: {
    permissions: [{ action: 'view', resource: 'recommendations' }], is_admin: true,
  } }));
  const nativeDialogs: string[] = [];
  page.on('dialog', async dialog => {
    nativeDialogs.push(dialog.message());
    await dialog.dismiss();
  });

  await page.goto('/admin/users');
  await page.locator('#create-apikey-btn').click();
  await page.locator('#apikey-name').fill('CI key');
  await page.locator('#apikey-password').fill('correct horse battery');
  await page.locator('#apikey-permissions-list input[type="checkbox"]').first().check();
  await page.locator('#create-apikey-submit-btn').click();
  await expect(page.locator('#apikey-created-modal')).toBeVisible();

  await page.locator('#copy-apikey-btn').click();

  const copyError = page.locator('#apikey-copy-error');
  await expect(copyError).toBeVisible();
  await expect(copyError).toHaveAttribute('role', 'alert');
  await expect(copyError).toContainText('The key is selected');
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(CREATED_KEY);
  expect(nativeDialogs).toEqual([]);
});
