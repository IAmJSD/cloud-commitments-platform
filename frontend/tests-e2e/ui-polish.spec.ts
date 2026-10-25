/**
 * 2026-08 UX review polish (issue #191), checked in a real browser:
 *
 *  - The four summary cards on Home and Opportunities lay out 4 across, 2x2
 *    or 1 column. The old auto-fit grid wrapped them 3 + 1 on mid-width
 *    screens, leaving an orphan card beside dead space.
 *  - Toggling a row checkbox with the keyboard keeps focus on that checkbox.
 *    The table rebuild used to drop focus to <body>, so the next Tab
 *    restarted at the top of the table.
 *  - The Opportunities lookback control shows its AWS-only scope inline.
 *
 * jsdom has no layout and no real focus navigation, so these run in Chromium.
 */

import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

const DASHBOARD = {
  potential_monthly_savings: 3320,
  total_recommendations: 19,
  active_commitments: 12,
  committed_monthly: 8300,
  current_coverage: 54.2,
  target_coverage: 80,
  ytd_savings: 21400,
  by_service: { ec2: { potential_savings: 1760, current_savings: 900 } },
};

async function open(page: Page, path: string): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/dashboard/summary**', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(DASHBOARD) }),
  );
  await page.route('**/api/dashboard/upcoming', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }),
  );
  await page.goto(path);
}

/** Cards per row, top to bottom, from one layout pass. */
async function rowsOf(page: Page, selector: string): Promise<number[]> {
  return page.evaluate((sel) => {
    const tops = Array.from(document.querySelectorAll(`${sel} > .card`))
      .map((el) => Math.round(el.getBoundingClientRect().top));
    const rows = new Map<number, number>();
    for (const t of tops) rows.set(t, (rows.get(t) ?? 0) + 1);
    return [...rows.entries()].sort((a, b) => a[0] - b[0]).map(([, n]) => n);
  }, selector);
}

const LAYOUTS: Array<[number, number[]]> = [
  [1440, [4]],
  [1100, [2, 2]],
  [1024, [2, 2]],
  [800, [2, 2]],
  [390, [1, 1, 1, 1]],
];

for (const [section, path] of [['#summary', '/home'], ['#recommendations-summary', '/opportunities']] as const) {
  test(`${path} summary cards never leave an orphan`, async ({ page }) => {
    await open(page, path);
    await expect(page.locator(`${section} > .card:not(.skeleton-tile)`)).toHaveCount(4);
    for (const [width, expected] of LAYOUTS) {
      await page.setViewportSize({ width, height: 900 });
      await expect.poll(() => rowsOf(page, section), { message: `${path} @ ${width}px` }).toEqual(expected);
    }
  });
}

test('Space on a row checkbox keeps keyboard focus on that checkbox', async ({ page }) => {
  await open(page, '/opportunities');
  const third = page.locator('tbody input[type="checkbox"][data-rec-id]').nth(2);
  await expect(third).toBeVisible();
  const recId = await third.getAttribute('data-rec-id');

  await third.focus();
  await page.keyboard.press('Space');
  const same = page.locator(`tbody input[type="checkbox"][data-rec-id="${recId}"]`);
  await expect(same).toBeChecked();
  await expect(same).toBeFocused();

  // The next Tab continues from the row (its Plan button), not from the top.
  await page.keyboard.press('Tab');
  await expect(page.locator(`button.rec-plan-btn[data-rec-id="${recId}"]`)).toBeFocused();

  // And Space again unchecks it without losing focus.
  await page.keyboard.press('Shift+Tab');
  await page.keyboard.press('Space');
  await expect(same).not.toBeChecked();
  await expect(same).toBeFocused();
});

test('lookback control states its AWS-only scope inline', async ({ page }) => {
  await open(page, '/opportunities');
  const hint = page.locator('#recs-lookback-scope');
  await expect(hint).toBeVisible();
  await expect(hint).toHaveText(/AWS rows only/);
  await expect(page.locator('#recs-lookback-days')).toHaveAttribute('aria-describedby', 'recs-lookback-scope');
});
