/**
 * Signed-out screens are standalone cards, so the app shell must never paint
 * while signed out. index.html marks <body class="auth-pending"> and CSS hides
 * #app until init() resolves auth state; before that, the shell showed for a
 * frame before .auth-screen existed.
 */
import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

test('signed out: the shell is hidden before the auth check resolves', async ({ page }) => {
  await mockApi(page);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  await page.route('**/api/info', async (route) => {
    await gate;
    await route.fulfill({ json: { admin_exists: true } });
  });

  await page.goto('/', { waitUntil: 'domcontentloaded' });
  // /api/info is still pending: no auth screen yet, and no shell either.
  await expect(page.locator('.auth-screen')).toHaveCount(0);
  await expect(page.locator('#app')).toBeHidden();
  await expect(page.locator('.app-topbar')).toBeHidden();

  release();
  await expect(page.locator('.auth-screen .modal-content')).toBeVisible();
  await expect(page.locator('#app')).toBeHidden();
  await expect(page.locator('body.auth-pending')).toHaveCount(0);
});

test('signed in: the shell shows once auth resolves and the pending marker is gone', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/auth/me', (r) =>
    r.fulfill({ json: { id: 'user-smoke', email: 'smoke@example.com', groups: [], mfa_enabled: false } }));
  await page.goto('/home');
  await expect(page.locator('.app-topbar')).toBeVisible();
  await expect(page.locator('#user-email-display')).toBeVisible();
  await expect(page.locator('body.auth-pending')).toHaveCount(0);
});

test('init error: the marker is cleared so the error card is not blank', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/auth/me', (r) =>
    r.fulfill({ status: 503, json: { error: 'service unavailable' } }));
  await page.goto('/plans');
  await expect(page.locator('#init-error')).toBeVisible();
  await expect(page.locator('body.auth-pending')).toHaveCount(0);
});
