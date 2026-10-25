/**
 * Init failure state (issue #206).
 *
 * index.html ships with the Home panel pre-rendered and active. A non-auth
 * failure in init() used to leave exactly that markup on screen, under the
 * requested URL and with no listeners bound. It must instead show an explicit
 * error card, and its retry must load the page the user asked for.
 */

import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

test('a failed /api/auth/me shows an error state instead of a silent Home page', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  let failMe = true;
  // Registered after mockApi, so it wins for this URL.
  await page.route('**/api/auth/me', async (route) => {
    if (!failMe) {
      await route.fallback();
      return;
    }
    await route.fulfill({
      status: 503,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'service unavailable' }),
    });
  });

  await page.goto('/plans');

  const card = page.locator('#init-error');
  await expect(card).toBeVisible();
  await expect(card).toHaveAttribute('role', 'alert');
  await expect(card).toContainText('CUDly could not finish loading');
  await expect(card).toContainText('HTTP 503: service unavailable');
  await expect(page.locator('#home-tab')).toBeHidden();
  await expect(page.locator('.tab-content.active')).toHaveCount(0);
  await expect(page).toHaveTitle('CUDly — Could not load');

  failMe = false;
  await card.getByRole('button', { name: 'Try again' }).click();

  await expect(page.locator('.tab-content.active#plans-tab')).toBeVisible();
  await expect(page.locator('#init-error')).toHaveCount(0);
  await expect(page).toHaveTitle('CUDly — Plans');
});
