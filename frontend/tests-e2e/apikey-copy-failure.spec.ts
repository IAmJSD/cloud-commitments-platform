import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const CREATED_KEY = 'cudly_e2e_key_123';

async function mockApiKeysPage(page: Page): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
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
}

async function createKey(page: Page): Promise<void> {
  await page.goto('/admin/users');
  await page.locator('#create-apikey-btn').click();
  await page.locator('#apikey-name').fill('CI key');
  await page.locator('#apikey-password').fill('correct horse battery');
  await page.locator('#apikey-permissions-list input[type="checkbox"]').first().check();
  await page.locator('#create-apikey-submit-btn').click();
  await expect(page.locator('#apikey-created-modal')).toBeVisible();
}

// Issue #705: a failed clipboard write on the one-time API key modal used a
// native alert(). It now shows an inline role="alert" message and selects the
// key so the user can copy it by hand without leaving the modal.
test('API key copy failure is reported inline, not with a native alert', async ({ page }) => {
  await mockApiKeysPage(page);
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: () => Promise.reject(new Error('Clipboard blocked')) },
    });
  });
  const nativeDialogs: string[] = [];
  page.on('dialog', async dialog => {
    nativeDialogs.push(dialog.message());
    await dialog.dismiss();
  });

  await createKey(page);

  await page.locator('#copy-apikey-btn').click();

  const copyError = page.locator('#apikey-copy-error');
  await expect(copyError).toBeVisible();
  await expect(copyError).toHaveAttribute('role', 'alert');
  await expect(copyError).toContainText('The key is selected');
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(CREATED_KEY);
  expect(nativeDialogs).toEqual([]);
});

// The curl example in the same modal used to run past the modal's right edge
// and get clipped (#705). It now wraps, so it must stay inside the modal.
for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
  test(`API key curl example stays inside the modal at ${viewport.width}x${viewport.height}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await mockApiKeysPage(page);
    await createKey(page);

    const modalBox = await page.locator('#apikey-created-modal .modal-content').boundingBox();
    const codeBox = await page.locator('#apikey-created-modal .code-example').boundingBox();
    expect(modalBox).not.toBeNull();
    expect(codeBox).not.toBeNull();
    expect(codeBox!.x).toBeGreaterThanOrEqual(modalBox!.x);
    expect(codeBox!.x + codeBox!.width).toBeLessThanOrEqual(modalBox!.x + modalBox!.width);
    // The <pre> box itself always fits; what overflowed was its text. Check the
    // rendered text's right edge and that nothing scrolls sideways.
    const layout = await page.evaluate(() => {
      const modal = document.querySelector('#apikey-created-modal .modal-content')!;
      const code = document.querySelector('#apikey-created-modal .code-example')!;
      const range = document.createRange();
      range.selectNodeContents(code);
      return {
        textRight: range.getBoundingClientRect().right,
        modalRight: modal.getBoundingClientRect().right,
        codeOverflow: code.scrollWidth - code.clientWidth,
        modalOverflow: modal.scrollWidth - modal.clientWidth,
      };
    });
    expect(layout.textRight).toBeLessThanOrEqual(layout.modalRight);
    expect(layout.codeOverflow).toBeLessThanOrEqual(0);
    expect(layout.modalOverflow).toBeLessThanOrEqual(0);
    await expect(page.locator('#apikey-created-modal .code-example')).toContainText('https://your-api-endpoint.com/api/...');
  });
}
