import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #539: Save carries only what the user changed, so a tab opened before
// another admin's save cannot write its stale defaults or service rows back.
test('Settings Save sends only the edited field and no service rows', async ({ page }) => {
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1'
    ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  const puts: Array<{ url: string; body: unknown }> = [];
  await page.route('**/api/config', route => {
    if (route.request().method() === 'PUT') {
      puts.push({ url: route.request().url(), body: route.request().postDataJSON() });
      return route.fulfill({ json: { status: 'updated' } });
    }
    return route.fulfill({ json: { global: {
      enabled_providers: ['aws'], default_term: 3, default_payment: 'all-upfront', default_coverage: 70, notification_days_before: 3,
    }, services: [
      { provider: 'aws', service: 'ec2', enabled: true, term: 3, payment: 'all-upfront', coverage: 70 },
    ] } });
  });
  await page.route('**/api/config/service/**', route => {
    puts.push({ url: route.request().url(), body: route.request().postDataJSON() });
    return route.fulfill({ json: { status: 'updated' } });
  });
  await page.route('**/api/commitment-options', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
  await page.route('**/api/ri-exchange/config', route => route.fulfill({ json: {
    mode: 'manual', auto_exchange_enabled: false, utilization_threshold: 80,
    max_payment_per_exchange_usd: 0, max_payment_daily_usd: 0, lookback_days: 30,
  } }));
  await page.route('**/api/ladder/**', route => route.fulfill({ json: { configs: [], runs: [], events: [], total_count: 0, total_usd_hr: '0.000000' } }));
  await page.goto('/admin/purchasing');

  await expect(page.locator('#setting-default-coverage')).toHaveValue('70');
  await page.locator('#setting-default-coverage').fill('60');
  await page.locator('#save-purchasing-btn').click();

  await expect(page.locator('.toast, [role="status"]').filter({ hasText: 'Settings saved' }).first()).toBeVisible();
  expect(puts).toHaveLength(1);
  expect(new URL(puts[0]!.url).pathname).toBe('/api/config');
  expect(puts[0]!.body).toEqual({ default_coverage: 60 });
});
