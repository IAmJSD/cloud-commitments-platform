import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #51: the Purchase History From/To inputs show LOCAL calendar days,
// while /api/history filters on whole UTC days (handler_history.go). The
// mock below applies the same UTC-day filter so a wrong bound drops rows
// exactly as the real backend would.

const ROW = {
  purchase_id: '51515151-5151-5151-5151-515151515151', provider: 'aws', service: 'ec2',
  resource_type: 'm5.large', region: 'us-east-1', count: 1, term: 1, upfront_cost: 1200,
  monthly_cost: 50, estimated_savings: 10, status: 'completed', account_id: 'acct-001',
};

async function historyFixture(page: Page, now: string, purchasedAt: string) {
  const requests: URL[] = [];
  await page.clock.setFixedTime(new Date(now));
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/history**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    const start = url.searchParams.get('start');
    const end = url.searchParams.get('end');
    const at = Date.parse(purchasedAt);
    const inRange = (!start || at >= Date.parse(`${start}T00:00:00Z`))
      && (!end || at <= Date.parse(`${end}T23:59:59Z`));
    return route.fulfill({ json: { summary: {}, purchases: inRange ? [{ ...ROW, timestamp: purchasedAt }] : [] } });
  });
  return requests;
}

test.describe('east of UTC (Pacific/Auckland)', () => {
  test.use({ timezoneId: 'Pacific/Auckland' });

  test('seeds today and keeps a purchase from earlier today when filtering on today only', async ({ page }) => {
    // 10:00 NZST on Jul 28 is 22:00 UTC on Jul 27; the purchase ran at 09:00 NZST.
    const requests = await historyFixture(page, '2026-07-27T22:00:00Z', '2026-07-27T21:00:00Z');
    await page.goto('/purchases/history');
    await expect(page.locator('#history-end')).toHaveValue('2026-07-28');
    await expect(page.locator('#history-start')).toHaveValue('2026-07-21');

    await page.locator('#history-start').fill('2026-07-28');
    const response = page.waitForResponse(r => r.url().includes('/api/history?'));
    await page.locator('#load-history-btn').click();
    await response;
    const last = requests[requests.length - 1]!;
    expect(last.searchParams.get('start')).toBe('2026-07-27');
    expect(last.searchParams.get('end')).toBe('2026-07-28');
    await expect(page.locator('#history-list tbody tr')).not.toHaveCount(0);
  });
});

test.describe('west of UTC (America/Los_Angeles)', () => {
  test.use({ timezoneId: 'America/Los_Angeles' });

  test('seeds today, not tomorrow, and still requests this evening\'s purchases', async ({ page }) => {
    // 20:00 PDT on Jul 27 is 03:00 UTC on Jul 28; the purchase ran at 17:30 PDT.
    const requests = await historyFixture(page, '2026-07-28T03:00:00Z', '2026-07-28T00:30:00Z');
    await page.goto('/purchases/history');
    await expect(page.locator('#history-end')).toHaveValue('2026-07-27');
    await expect(page.locator('#history-start')).toHaveValue('2026-07-20');
    await expect.poll(() => requests.length).toBeGreaterThan(0);
    const last = requests[requests.length - 1]!;
    expect(last.searchParams.get('start')).toBe('2026-07-20');
    expect(last.searchParams.get('end')).toBe('2026-07-28');
    await expect(page.locator('#history-list tbody tr')).not.toHaveCount(0);
  });
});
