/**
 * The Opportunities list is read in cursor pages (issue #723): the page must
 * request a bounded page, follow next_cursor, and show every row of all pages.
 */

import { expect, test } from '@playwright/test';

import { mockApi, RECS, seedAuth } from './fixtures/recs';

test('Opportunities follows next_cursor and shows the rows of every page', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  const requests: URL[] = [];
  const split = 7;
  await page.route('**/api/recommendations*', (route) => {
    const url = new URL(route.request().url());
    requests.push(url);
    const second = url.searchParams.get('cursor') === 'page-2';
    route.fulfill({
      json: {
        recommendations: second ? RECS.slice(split) : RECS.slice(0, split),
        summary: {},
        regions: [],
        ...(second ? {} : { next_cursor: 'page-2' }),
      },
    });
  });
  await page.goto('/opportunities');
  await expect(page.locator('.recommendations-filter-live')).toContainText(
    `Showing ${RECS.length} of ${RECS.length} recommendations`,
  );
  const listReads = requests.filter((u) => u.pathname === '/api/recommendations');
  expect(listReads.length).toBeGreaterThanOrEqual(2);
  expect(listReads.every((u) => u.searchParams.get('limit') === '1000')).toBe(true);
  expect(listReads.some((u) => u.searchParams.get('cursor') === 'page-2')).toBe(true);
});
