import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

async function expectFeedbackContext(page: Page, date: string): Promise<void> {
  const href = await page.locator('#feedback-link').getAttribute('href');
  const params = new URLSearchParams(href?.split('?')[1]);
  expect(href).toMatch(/^mailto:/);
  expect(params.get('subject')).toBe('CUDly Feedback');
  expect(params.get('body')).toContain(`- URL: ${page.url()}`);
  expect(params.get('body')).toContain(`- Date: ${date}`);
  expect(params.get('body')).toContain('## Steps to Reproduce (for bugs)');
}

test('feedback uses the current SPA route and time on mouse, keyboard, focus and context menu', async ({ page }) => {
  const pageErrors: string[] = [];
  const consoleErrors: string[] = [];
  const failedRequests: string[] = [];
  page.on('pageerror', error => pageErrors.push(String(error)));
  page.on('console', message => {
    if (message.type() === 'error') consoleErrors.push(message.text());
  });
  page.on('response', response => {
    if (response.status() >= 400) failedRequests.push(`${response.status()} ${new URL(response.url()).pathname}`);
  });
  page.on('requestfailed', request => failedRequests.push(new URL(request.url()).pathname));
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/history?*', route => route.fulfill({ json: [] }));
  await page.route('**/api/history/analytics?*', route => route.fulfill({ json: {
    start: '2026-10-01T00:00:00Z', end: '2026-10-14T00:00:00Z', interval: 'day',
    summary: { total_period_savings: 0, total_upfront_spent: 0, purchase_count: 0, average_savings_per_period: 0, peak_savings: 0 },
    data_points: [],
  } }));
  await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [] } }));
  await page.route('**/api/dashboard/upcoming', route => route.fulfill({ json: [] }));
  await page.route('**/api/dashboard/summary?*', route => route.fulfill({ json: {
    potential_monthly_savings: 0, total_recommendations: 0, active_commitments: 0,
    committed_monthly: 0, current_coverage: 0, target_coverage: 0, ytd_savings: 0, by_service: {},
  } }));
  await page.clock.setFixedTime(new Date('2026-10-13T23:59:00Z'));
  await page.goto('/purchases');
  await expect(page.locator('#purchases-tab.active')).toBeVisible();
  const originalDocument = await page.evaluate(() => performance.timeOrigin);
  const feedback = page.locator('#feedback-link');
  await expectFeedbackContext(page, '2026-10-13T23:59:00.000Z');
  await feedback.evaluate(link => {
    link.addEventListener('click', event => event.preventDefault());
    link.addEventListener('contextmenu', event => event.preventDefault());
  });

  for (const tab of ['plans', 'home', 'opportunities']) {
    await page.locator(`#sidebar [data-tab="${tab}"]`).click();
    await expect(page.locator(`#${tab}-tab.active`)).toBeVisible();
    await feedback.focus();
    await page.clock.setFixedTime(new Date('2026-10-14T00:01:00Z'));
    await feedback.press('Enter');
    await expectFeedbackContext(page, '2026-10-14T00:01:00.000Z');
    await page.clock.setFixedTime(new Date('2026-10-14T00:02:00Z'));
    await feedback.click();
    await expectFeedbackContext(page, '2026-10-14T00:02:00.000Z');
    expect(await page.evaluate(() => performance.timeOrigin)).toBe(originalDocument);
  }

  await page.evaluate(() => window.history.pushState(null, '', '/opportunities?filter=a%26b%2Bc#details'));
  await page.clock.setFixedTime(new Date('2026-10-14T00:03:00Z'));
  await feedback.click({ button: 'right' });
  await expectFeedbackContext(page, '2026-10-14T00:03:00.000Z');
  await page.locator('#sidebar [data-tab="home"]').focus();
  await page.clock.setFixedTime(new Date('2026-10-14T00:04:00Z'));
  await feedback.focus();
  await expectFeedbackContext(page, '2026-10-14T00:04:00.000Z');
  expect(pageErrors).toEqual([]);
  expect(failedRequests).toEqual([]);
  expect(consoleErrors).toEqual([]);
});
