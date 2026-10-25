import { test, expect } from '@playwright/test';
import { mockApi } from './fixtures/recs';

// #134: a non-OK /api/info used to resolve to a fabricated
// `admin_exists: false`, so a 502/503 during a deploy showed every signed-out
// visitor the first-run "Welcome to CUDly" admin-setup screen.
test('a failed /api/info shows the login form with the failure, not first-run setup', async ({ page }) => {
  await mockApi(page);
  await page.route('**/api/info', route => route.fulfill({ status: 503, contentType: 'text/html', body: '<html>Service Unavailable</html>' }));
  await page.goto('/');

  await expect(page.getByRole('heading', { name: 'CUDly Login' })).toBeVisible();
  await expect(page.locator('#admin-setup-modal')).toHaveCount(0);
  await expect(page.locator('#login-error')).toHaveText(
    'Could not reach the CUDly API (HTTP 503). Sign-in may fail until it is available again.',
  );
  // The Login button sits below the error box with a visible gap, not flush on it.
  const box = await page.locator('#login-error').boundingBox();
  const btn = await page.locator('#login-form button[type="submit"]').boundingBox();
  expect(box && btn).toBeTruthy();
  expect(btn!.y - (box!.y + box!.height)).toBeGreaterThanOrEqual(12);
});

test('with no error, the hidden error box adds no gap above Login', async ({ page }) => {
  await mockApi(page);
  await page.route('**/api/info', route => route.fulfill({ json: { version: 'test', admin_exists: true } }));
  await page.goto('/');

  await expect(page.getByRole('heading', { name: 'CUDly Login' })).toBeVisible();
  await expect(page.locator('#login-error')).toBeHidden();
  const link = await page.locator('#forgot-password-link').boundingBox();
  const btn = await page.locator('#login-form button[type="submit"]').boundingBox();
  expect(link && btn).toBeTruthy();
  // 12px today (the help text's own spacing); a stray box margin would add 16px more.
  expect(btn!.y - (link!.y + link!.height)).toBeLessThan(20);
});

test('an explicit admin_exists: false still opens first-run setup', async ({ page }) => {
  await mockApi(page);
  await page.route('**/api/info', route => route.fulfill({ json: { version: 'test', admin_exists: false } }));
  await page.goto('/');

  await expect(page.getByRole('heading', { name: 'Welcome to CUDly' })).toBeVisible();
  await expect(page.locator('#login-modal')).toHaveCount(0);
});
