/**
 * Admin -> Purchasing policies layout (issue #190): section links, collapsed
 * per-provider service grids, and a titled Exchange Automation placeholder
 * while its config loads. Runs in Chromium because scrolling, stickiness and
 * <details> visibility are layout behaviors jsdom does not model.
 */

import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const json = (body: unknown) => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });

async function openPurchasing(
  page: Page,
  opts: { hangExchange?: boolean; providers?: string[] } = {},
): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/config', (r) => r.fulfill(json({
    global: { enabled_providers: opts.providers ?? ['aws', 'azure', 'gcp'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80, notification_email: 'ops@example.com' },
    services: [],
  })));
  await page.route('**/api/config/service/**', (r) => r.fulfill(json({})));
  await page.route('**/api/ladder/configs**', (r) => r.fulfill(json({ configs: [] })));
  if (opts.hangExchange) await page.route('**/api/ri-exchange/config', () => { /* never answers */ });
  await page.goto('/admin/purchasing');
  await expect(page.locator('#purchasing-section-nav')).toBeVisible();
}

test('provider service grids start collapsed and a section link opens and scrolls to one', async ({ page }) => {
  await openPurchasing(page);
  const awsTerm = page.locator('#aws-ec2-term');
  await expect(awsTerm).toBeHidden();
  await expect(page.locator('#gcp-compute-term')).toBeHidden();

  await page.locator('#purchasing-section-nav a[href="#aws-settings"]').click();
  await expect(awsTerm).toBeVisible();
  await expect(awsTerm).toBeInViewport();
  await expect(page.locator('#aws-settings')).toBeFocused();
  expect(new URL(page.url()).pathname).toBe('/admin/purchasing');
  expect(new URL(page.url()).hash).toBe('');
  // The link row stays reachable after the jump (sticky under the topbar).
  await expect(page.locator('#purchasing-section-nav')).toBeInViewport();
});

test('links for disabled providers are hidden', async ({ page }) => {
  await openPurchasing(page, { providers: ['aws', 'gcp'] });
  await expect(page.locator('#purchasing-section-nav a[href="#aws-settings"]')).toBeVisible();
  await expect(page.locator('#purchasing-section-nav a[href="#gcp-settings"]')).toBeVisible();
  await expect(page.locator('#purchasing-section-nav a[href="#azure-settings"]')).toBeHidden();
});

test('collapsed service fields are still saved', async ({ page }) => {
  await openPurchasing(page);
  const servicePut = page.waitForRequest((r) => r.method() === 'PUT' && r.url().includes('/api/config/service/aws/ec2'));
  await page.locator('#setting-default-term').selectOption('1');
  await page.locator('.modal-confirm-actions button', { hasText: 'Apply to all' }).click();
  await expect(page.locator('#aws-ec2-term')).toBeHidden();
  await page.locator('#save-purchasing-btn').click();
  expect(JSON.parse((await servicePut).postData() ?? '{}').term).toBe(1);
});

test('Exchange Automation loads inside its own titled frame', async ({ page }) => {
  await openPurchasing(page, { hangExchange: true });
  const frame = page.locator('#ri-exchange-automation-settings > fieldset');
  await expect(frame.locator('legend')).toHaveText('Exchange Automation');
  await expect(frame).toHaveAttribute('aria-busy', 'true');
  // Issue #186: the shared skeleton (with its screen-reader status) fills the
  // frame instead of a visible loading paragraph.
  await expect(frame.locator('.skeleton-group')).toBeVisible();
  await expect(frame.locator('.skeleton-status')).toHaveText('Loading…');
  await expect(frame.locator('p.loading')).toHaveCount(0);
});
