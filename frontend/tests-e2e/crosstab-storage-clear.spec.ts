import { test, expect } from '@playwright/test';
import { mockApi } from './fixtures/recs';

// #317: localStorage.clear() in another tab fires a storage event with
// key === null. The cross-tab sign-out listener used to ignore that shape, so
// this tab kept rendering a signed-in view after every key was wiped.
test('localStorage.clear() in another tab signs this tab out', async ({ page, context }) => {
  await mockApi(page);
  await page.route('**/api/info', route => route.fulfill({ json: { version: 'test', admin_exists: true } }));
  // Sign in with a one-off write rather than an init script, which would
  // re-seed the token on the reload this test expects.
  await page.goto('/');
  await page.evaluate(() => { localStorage.setItem('authToken', 'smoke-token'); localStorage.setItem('csrfToken', 'smoke-csrf'); });
  await page.reload();
  await expect(page.getByRole('button', { name: 'Log out' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'CUDly Login' })).toHaveCount(0);

  const other = await context.newPage();
  await mockApi(other);
  await other.goto('/');
  await other.evaluate(() => localStorage.clear());
  await other.close();

  await expect(page.getByRole('heading', { name: 'CUDly Login' })).toBeVisible();
});
