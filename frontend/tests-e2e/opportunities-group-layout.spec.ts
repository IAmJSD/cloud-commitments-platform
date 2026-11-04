/**
 * Opportunities multi-variant group layout (issue #1684).
 *
 * jsdom has no layout, so the colspan, horizontal-scroll, popover-height and
 * disabled-button checks run in Chromium against the built bundle.
 */

import { expect, test, type Page } from '@playwright/test';

import { mockApi, RECS, seedAuth } from './fixtures/recs';

const GROUP_BASE = {
  provider: 'aws', cloud_account_id: 'acct-001', service: 'rds', resource_type: 'db.r6g.2xlarge',
  engine: 'postgresql', region: 'eu-west-1', count: 2, monthly_cost: 300, on_demand_monthly: 500,
};
const GROUP = [
  { ...GROUP_BASE, id: 'g-1-nu', term: 1, payment: 'no-upfront', savings: 120, upfront_cost: 0, effective_savings_pct: 24 },
  { ...GROUP_BASE, id: 'g-1-au', term: 1, payment: 'all-upfront', savings: 160, upfront_cost: 1800, effective_savings_pct: 32 },
  { ...GROUP_BASE, id: 'g-3-pu', term: 3, payment: 'partial-upfront', savings: 210, upfront_cost: 3600, effective_savings_pct: 42 },
  { ...GROUP_BASE, id: 'g-3-au', term: 3, payment: 'all-upfront', savings: 230, upfront_cost: 7000, effective_savings_pct: 46 },
];
const ROWS = [...GROUP, ...RECS];

async function openOpportunities(page: Page): Promise<void> {
  await page.setViewportSize({ width: 1920, height: 1080 });
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/recommendations*', (route) =>
    route.fulfill({ json: { recommendations: ROWS, summary: {}, regions: [] } }));
  await page.goto('/opportunities');
  await expect(page.locator('.recommendations-filter-live')).toContainText(
    `Showing ${ROWS.length} of ${ROWS.length} recommendations`,
  );
}

const groupRow = (page: Page) => page.locator('tr.rec-cell-summary-row[data-cell-key*="db.r6g.2xlarge"]');

test('collapsed group summary spans every column after Service', async ({ page }) => {
  await openOpportunities(page);
  const geometry = await groupRow(page).evaluate((row) => {
    const td = row.querySelector<HTMLTableCellElement>('td.rec-cell-summary-content')!;
    const ths = row.closest('table')!.querySelectorAll('thead th');
    return {
      display: getComputedStyle(td).display,
      tdRight: td.getBoundingClientRect().right,
      tableRight: ths[ths.length - 1]!.getBoundingClientRect().right,
    };
  });
  expect(geometry.display).toBe('table-cell');
  expect(Math.abs(geometry.tdRight - geometry.tableRight)).toBeLessThanOrEqual(1);
});

test('expanding a group adds no horizontal scroll and keeps the header still', async ({ page }) => {
  await openOpportunities(page);
  const headerLefts = () => page.locator('#recommendations-list thead th').evaluateAll(
    (ths) => ths.map((th) => Math.round(th.getBoundingClientRect().left)),
  );
  const before = await headerLefts();
  await groupRow(page).locator('.rec-cell-chevron').click();
  await expect(page.locator('tr.rec-variant-row')).toHaveCount(GROUP.length);

  const after = await headerLefts();
  after.forEach((left, i) => expect(Math.abs(left - before[i]!)).toBeLessThanOrEqual(2));

  const overflow = await page.locator('#recommendations-list table').evaluate((table) => {
    const out: string[] = [];
    for (let el: HTMLElement | null = table.parentElement; el; el = el.parentElement) {
      const ox = getComputedStyle(el).overflowX;
      if ((ox === 'auto' || ox === 'scroll') && el.scrollWidth > el.clientWidth + 1) {
        out.push(`${el.tagName}#${el.id}.${el.className}: ${el.scrollWidth} > ${el.clientWidth}`);
      }
    }
    const doc = document.documentElement;
    if (doc.scrollWidth > doc.clientWidth + 1) out.push(`document: ${doc.scrollWidth} > ${doc.clientWidth}`);
    return out;
  });
  expect(overflow).toEqual([]);
});

test('Columns popover lists every option without an inner scrollbar', async ({ page }) => {
  await openOpportunities(page);
  await page.locator('.column-visibility-btn').click();
  const popover = page.getByRole('dialog', { name: 'Show/hide columns' });
  await expect(popover).toBeVisible();
  const { scrollHeight, clientHeight } = await popover.evaluate((el) => ({
    scrollHeight: el.scrollHeight, clientHeight: el.clientHeight,
  }));
  expect(scrollHeight).toBeLessThanOrEqual(clientHeight);
});

test('disabled Purchase button does not keep the primary fill', async ({ page }) => {
  await openOpportunities(page);
  const purchase = page.locator('#bulk-purchase-btn');
  await expect(purchase).toBeDisabled();
  const disabledBg = await purchase.evaluate((el) => getComputedStyle(el).backgroundColor);

  await page.locator('tr.recommendation-row[data-rec-id="r01"]')
    .getByRole('checkbox', { name: /^Select / }).check();
  await expect(purchase).toBeEnabled();
  const enabledBg = await purchase.evaluate((el) => getComputedStyle(el).backgroundColor);
  expect(disabledBg).not.toBe(enabledBg);
});
