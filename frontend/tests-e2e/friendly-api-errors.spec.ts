import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #748: transport failures reach the user as a sentence they can act
// on, not "HTTP 502", "Failed to fetch" or "Unexpected token '<'".

const summary = (page: import('@playwright/test').Page) => page.locator('#summary .error');

test.beforeEach(async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
});

test('a proxy 502 HTML page on the dashboard says CUDly is unavailable', async ({ page }) => {
  await page.route('**/api/dashboard/summary**', route => route.fulfill({
    status: 502, contentType: 'text/html', body: '<html><body><h1>502 Bad Gateway</h1></body></html>',
  }));
  await page.goto('/home');

  await expect(summary(page)).toHaveText('Failed to load dashboard: CUDly is temporarily unavailable. Try again in a moment.');
  await expect(summary(page)).not.toContainText('HTTP 502');
});

test('a dropped connection on the dashboard says CUDly cannot be reached', async ({ page }) => {
  await page.route('**/api/dashboard/summary**', route => route.abort('internetdisconnected'));
  await page.goto('/home');

  await expect(summary(page)).toHaveText("Failed to load dashboard: Can't reach CUDly. Check your connection and try again.");
});

test('an HTML page served with 200 is reported as an unexpected response, not a JSON parse error', async ({ page }) => {
  await page.route('**/api/dashboard/summary**', route => route.fulfill({
    status: 200, contentType: 'text/html', body: '<!doctype html><html></html>',
  }));
  await page.goto('/home');

  await expect(summary(page)).toHaveText('Failed to load dashboard: CUDly sent an unexpected response. Try again in a moment.');
});
