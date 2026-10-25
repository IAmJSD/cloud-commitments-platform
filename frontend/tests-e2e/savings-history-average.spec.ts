import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

type Mode = 'sparse' | 'zero-filled' | 'full' | 'zero' | 'empty' | 'error';

async function savingsFixture(page: Page) {
  const fixture: { mode: Mode; requests: URL[] } = { mode: 'sparse', requests: [] };
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1'
    ? route.continue() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/history**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname !== '/api/history/analytics') {
      await route.fulfill({ json: [] });
      return;
    }
    fixture.requests.push(url);
    if (fixture.mode === 'error') {
      await route.fulfill({ status: 500, json: { error: 'fixture unavailable' } });
      return;
    }
    const start = url.searchParams.get('start')!;
    const end = url.searchParams.get('end')!;
    const interval = url.searchParams.get('interval')!;
    const intervalMs = interval === 'hourly' ? 3600000 : 86400000;
    const count = (Date.parse(end) - Date.parse(start)) / intervalMs;
    const indices = fixture.mode === 'sparse' ? [0, Math.floor(count / 2), Math.floor(count)]
      : Array.from({ length: fixture.mode === 'full' ? count : Math.floor(count) + 1 }, (_, i) => i);
    const values = indices.map(i => fixture.mode === 'full' ? 10
      : fixture.mode === 'zero' ? 0 : [0, Math.floor(count / 2), Math.floor(count)].includes(i) ? 560 : 0);
    const total = values.reduce((sum, value) => sum + value, 0);
    await route.fulfill({ json: {
      start, end, interval,
      summary: { total_monthly_savings: total, total_annual_savings: total * 12, total_purchases: 3 },
      data_points: fixture.mode === 'empty' ? [] : indices.map((index, i) => ({
        timestamp: new Date(Math.floor(Date.parse(start) / intervalMs) * intervalMs + index * intervalMs).toISOString(),
        total_savings: values[i], cumulative_savings: values.slice(0, i + 1).reduce((sum, value) => sum + value, 0),
        total_upfront: 0, purchase_count: values[i] ? 1 : 0,
      })),
    } });
  });
  return fixture;
}

async function changePeriod(page: Page, period: string) {
  const response = page.waitForResponse(value => value.url().includes('/history/analytics?'));
  await page.locator('#savings-period').selectOption(period);
  await response;
}

test.use({ timezoneId: 'UTC' });

for (const [period, count, average, totalFull] of [
  ['24h', 24, '$70.00/mo', '$240.00'], ['7d', 168, '$10.00/mo', '$1,680.00'],
  ['30d', 30, '$56.00/mo', '$300.00'], ['90d', 90, '$18.67/mo', '$900.00'],
] as const) {
  test(period + ' averages the requested window regardless of missing zero buckets', async ({ page }) => {
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    const fixture = await savingsFixture(page);
    await page.clock.setFixedTime(new Date('2026-10-01T12:17:15.123Z'));
    await page.goto('/purchases/history');
    for (const mode of ['sparse', 'zero-filled', 'full'] as const) {
      fixture.mode = mode;
      await changePeriod(page, period);
      await expect(page.locator('#avg-savings-label')).toHaveText('Avg Monthly Savings');
      await expect(page.locator('#avg-hourly-savings')).toHaveText(mode === 'full' ? '$10.00/mo' : average);
      await expect(page.locator('#period-savings')).toHaveText(mode === 'full' ? totalFull : '$1,680.00');
      await expect(page.locator('#peak-savings')).toHaveText(mode === 'full' ? '$10.00/mo' : '$560.00/mo');
      const request = fixture.requests[fixture.requests.length - 1]!;
      const ms = period === '24h' || period === '7d' ? 3600000 : 86400000;
      expect(Date.parse(request.searchParams.get('end')!) - Date.parse(request.searchParams.get('start')!)).toBe(count * ms);
    }
    expect(errors).toEqual([]);
  });
}

test('unit switches, refresh and empty/error states work on a narrow screen', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const fixture = await savingsFixture(page);
  await page.clock.setFixedTime(new Date('2026-10-01T12:17:15Z'));
  await page.goto('/purchases/history');
  await changePeriod(page, '7d');
  for (const [unit, label, average, total, peak] of [
    ['hourly', 'Hourly', '$0.01/hr', '$2.30', '$0.77/hr'],
    ['yearly', 'Yearly', '$120.00/yr', '$20,160.00', '$6,720.00/yr'],
    ['monthly', 'Monthly', '$10.00/mo', '$1,680.00', '$560.00/mo'],
  ]) {
    await page.locator('#savings-unit').selectOption(unit!);
    await expect(page.locator('#avg-savings-label')).toHaveText('Avg ' + label + ' Savings');
    await expect(page.locator('#avg-hourly-savings')).toHaveText(average!);
    await expect(page.locator('#period-savings')).toHaveText(total!);
    await expect(page.locator('#peak-savings')).toHaveText(peak!);
  }
  // Issue #749: the running total is its own view, re-queried on change.
  const reload = page.waitForResponse(value => value.url().includes('/history/analytics?'));
  await page.locator('#savings-series').selectOption('cumulative');
  await reload;
  await expect(page.locator('#savings-history-chart')).toBeVisible();
  await page.locator('#savings-series').selectOption('period');
  fixture.mode = 'zero';
  await page.locator('#refresh-savings-btn').click();
  await expect(page.locator('#avg-hourly-savings')).toHaveText('$0.00/mo');
  await expect(page.locator('#savings-stats')).toBeVisible();
  fixture.mode = 'empty';
  await page.locator('#refresh-savings-btn').click();
  await expect(page.locator('#savings-stats')).toBeHidden();
  await expect(page.locator('#savings-history-empty')).toContainText('No savings history data');
  fixture.mode = 'error';
  await page.locator('#refresh-savings-btn').click();
  await expect(page.locator('#savings-history-empty')).toContainText('Failed to load savings history');
  await expect(page.locator('#savings-stats')).toBeHidden();
});

test.describe('local calendar selections across DST', () => {
  test.use({ timezoneId: 'Europe/Berlin' });
  for (const [now, period, hours] of [
    ['2026-03-29T12:17:15Z', '24h', 23], ['2026-10-25T12:17:15Z', '24h', 25],
    ['2026-03-31T12:17:15Z', '7d', 167], ['2026-10-27T12:17:15Z', '7d', 169],
    ['2026-03-31T12:17:15Z', '30d', 719], ['2026-10-27T12:17:15Z', '30d', 721],
  ] as const) {
    test(now + ' ' + period + ' uses the actual elapsed interval count', async ({ page }) => {
      const fixture = await savingsFixture(page);
      await page.clock.setFixedTime(new Date(now));
      await page.goto('/purchases/history');
      await changePeriod(page, period);
      const count = period === '30d' ? hours / 24 : hours;
      await expect(page.locator('#avg-hourly-savings')).toHaveText('$' + (1680 / count).toFixed(2) + '/mo');
      const request = fixture.requests[fixture.requests.length - 1]!;
      expect(Date.parse(request.searchParams.get('end')!) - Date.parse(request.searchParams.get('start')!)).toBe(hours * 3600000);
    });
  }
});
