import { expect, test, type Page } from '@playwright/test';

import { mockApi, RECS, seedAuth } from './fixtures/recs';

// Issue #755: Enter on a column-filter button re-sorted the table instead of
// opening the filter, because the focusable role=button <th> handled Enter
// for the whole header. The header now holds sibling sort and filter buttons.

async function openOpportunities(page: Page): Promise<void> {
  await mockApi(page);
  await seedAuth(page);
  await page.goto('/');
  await page.getByRole('tab', { name: 'Opportunities' }).click();
  await expect(page.locator('.recommendations-filter-live')).toContainText(
    `Showing ${RECS.length} of ${RECS.length} recommendations`,
  );
}

const firstRowId = (page: Page) =>
  page.locator('tr.recommendation-row').first().getAttribute('data-rec-id');

test('Enter on a column-filter button opens the filter and does not sort', async ({ page }) => {
  await openOpportunities(page);
  const before = await firstRowId(page);
  const filterBtn = page.getByRole('button', { name: 'Filter Service' });
  await filterBtn.focus();
  await page.keyboard.press('Enter');

  const dialog = page.getByRole('dialog', { name: 'Filter Service' });
  await expect(dialog).toBeVisible();
  expect(await firstRowId(page)).toBe(before);
  await expect(page.locator('th[data-sort="savings"]')).toHaveAttribute('aria-sort', 'descending');
  await expect(page.locator('th[data-sort="service"]')).not.toHaveAttribute('aria-sort', /.*/);
});

test('the sort button sorts from the keyboard and keeps focus', async ({ page }) => {
  await openOpportunities(page);
  const sortBtn = page.locator('th[data-sort="service"] .sort-btn');
  await sortBtn.focus();
  await page.keyboard.press('Enter');

  await expect(page.locator('th[data-sort="service"]')).toHaveAttribute('aria-sort', 'ascending');
  await expect(page.locator('th[data-sort="savings"]')).not.toHaveAttribute('aria-sort', /.*/);
  await expect(page.locator('th[data-sort="service"] .sort-btn')).toBeFocused();
  await expect(page.locator('.column-filter-popover')).toHaveCount(0);
});

test('column headers are not nested interactive controls and filter targets are at least 24px', async ({ page }) => {
  await openOpportunities(page);
  await expect(page.locator('th.sortable[role="button"], th.sortable[tabindex]')).toHaveCount(0);
  const boxes = await page.locator('th .column-filter-btn').evaluateAll((els) =>
    els.map((el) => el.getBoundingClientRect()).map((r) => ({ w: r.width, h: r.height })));
  expect(boxes.length).toBeGreaterThan(0);
  for (const box of boxes) {
    expect(box.w).toBeGreaterThanOrEqual(24);
    expect(box.h).toBeGreaterThanOrEqual(24);
  }
});
