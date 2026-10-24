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
});

test('an explicit admin_exists: false still opens first-run setup', async ({ page }) => {
  await mockApi(page);
  await page.route('**/api/info', route => route.fulfill({ json: { version: 'test', admin_exists: false } }));
  await page.goto('/');

  await expect(page.getByRole('heading', { name: 'Welcome to CUDly' })).toBeVisible();
  await expect(page.locator('#login-modal')).toHaveCount(0);
});
