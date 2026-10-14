import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const cases = [
  { zone: 'UTC', now: '2026-10-15T23:59:00Z', suggested: '2026-10-20', selected: '2026-10-16' },
  { zone: 'America/Los_Angeles', now: '2026-03-08T07:59:00Z', suggested: '2026-03-13', selected: '2026-03-09' },
  { zone: 'Europe/Berlin', now: '2026-10-24T23:59:00Z', suggested: '2026-10-29', selected: '2026-10-26' },
];

for (const scenario of cases) {
  test.describe(scenario.zone, () => {
    test.use({ timezoneId: scenario.zone });
    test('suggests notice lead time and preserves an earlier user date (mock API)', async ({ page }, testInfo) => {
      const errors: string[] = [];
      page.on('pageerror', error => errors.push(error.message));
      page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
      page.on('requestfailed', request => errors.push(request.url()));
      page.on('response', response => { if (response.status() >= 400) errors.push(`${response.status()} ${response.url()}`); });
      await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1'
        ? route.fallback() : route.abort());
      await page.clock.setFixedTime(new Date(scenario.now));
      await seedAuth(page);
      await mockApi(page);
      const submitted: unknown[] = [];
      await page.route('**/api/plans**', async route => {
        const path = new URL(route.request().url()).pathname;
        if (path.endsWith('/purchases') && route.request().method() === 'POST') {
          submitted.push(route.request().postDataJSON());
          await route.fulfill({ json: { created: 1 } });
        } else if (path.endsWith('/accounts')) {
          await route.fulfill({ json: [] });
        } else {
          await route.fulfill({ json: { plans: [{ id: 'plan-notice', name: 'Notice plan', enabled: true,
            auto_purchase: false, notification_days_before: 3,
            services: { ec2: { provider: 'aws', service: 'ec2', enabled: true, term: 1, payment: 'no-upfront', coverage: 80 } },
            ramp_schedule: { current_step: 0, total_steps: 4, interval_days: 7, percent_per_step: 25 },
          }] } });
        }
      });
      await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [] } }));
      await page.goto('/plans');
      const scripts = await page.locator('script[src]').evaluateAll(elements => elements.map(element => (element as HTMLScriptElement).src));
      expect(scripts.some(source => /app\.[a-f0-9]+\.js/.test(source))).toBe(true);
      await page.locator('[data-action="add-purchases"][data-id="plan-notice"]').click();
      const modal = page.locator('#add-purchases-modal');
      const date = page.locator('#add-purchases-start-date');
      await expect(date).toHaveValue(scenario.suggested);
      await expect(page.locator('#add-purchases-date-help')).toContainText('3-day notification window');
      await expect(page.locator('#add-purchases-date-help')).toContainText('midnight UTC');
      const minimum = await date.getAttribute('min');
      const suggested = await date.inputValue();
      await date.fill(scenario.selected);
      await expect(date).toHaveValue(scenario.selected);
      await expect(date).toHaveAttribute('min', minimum!);
      await expect(page.locator('#add-purchases-date-warning')).toContainText('may leave too little notice');
      await page.screenshot({ path: testInfo.outputPath('notice-modal.png'), fullPage: true });
      await modal.getByRole('button', { name: 'Add Purchases', exact: true }).click();
      await expect(modal).toHaveCount(0);
      expect(submitted).toEqual([{ count: 1, start_date: scenario.selected }]);
      expect(errors).toEqual([]);
      await testInfo.attach('notice-date-receipt', { body: JSON.stringify({ ...scenario, suggested, minimum, notice: 3, submitted, scripts }), contentType: 'application/json' });
    });
  });
}
