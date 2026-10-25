import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #642: Reset settings writes values from code, which fires no
// 'input'/'blur'; the inline range error from an earlier edit must still clear.
test('Reset settings clears the inline error left by an emptied coverage field', async ({ page }) => {
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1'
    ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/config', route => route.fulfill({ json: { global: {
    enabled_providers: ['aws'], default_term: 3, default_payment: 'all-upfront', default_coverage: 70, notification_days_before: 3,
  }, services: [] } }));
  await page.route('**/api/commitment-options', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
  await page.route('**/api/ri-exchange/config', route => route.fulfill({ json: {
    mode: 'manual', auto_exchange_enabled: false, utilization_threshold: 80,
    max_payment_per_exchange_usd: 0, max_payment_daily_usd: 0, lookback_days: 30,
  } }));
  await page.route('**/api/ladder/**', route => route.fulfill({ json: { configs: [], runs: [], events: [], total_count: 0, total_usd_hr: '0.000000' } }));
  await page.goto('/admin/purchasing');

  const coverage = page.locator('#setting-default-coverage');
  const error = page.locator('#setting-default-coverage-range-error');
  await expect(coverage).toHaveValue('70');
  await coverage.fill('');
  await coverage.blur();
  await expect(error).toBeVisible();
  await expect(coverage).toHaveAttribute('aria-invalid', 'true');

  await page.locator('#reset-purchasing-btn').click();
  await page.getByRole('button', { name: 'Reset settings' }).click();

  await expect(coverage).toHaveValue('80');
  await expect(error).toBeHidden();
  await expect(coverage).not.toHaveAttribute('aria-invalid', /.*/);
});
