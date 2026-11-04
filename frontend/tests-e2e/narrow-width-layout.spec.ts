/**
 * Mid-width layout (issue #1682): between the phone drawer (<= 768px) and a
 * full desktop the sidebar goes icon-only, the topbar chips stay on one line,
 * and a table pinned under a sticky Actions column shows a visible edge hint.
 */

import { expect, test, type Page } from '@playwright/test';

import { ACCOUNTS, mockApi, seedAuth } from './fixtures/recs';

const PLAN = {
  id: 'p1', name: 'RDS Full Coverage Purchase', enabled: true, auto_purchase: false, notification_days_before: 3, services: {},
  ramp_schedule: { type: 'weekly', current_step: 1, total_steps: 2, step_interval_days: 7, percent_per_step: 50 },
};
const PLANNED = [1, 2].map((n) => ({
  id: `pp${n}`, plan_id: 'p1', plan_name: PLAN.name, scheduled_date: `2026-1${n}-20T00:00:00Z`,
  provider: 'aws', service: 'rds', resource_type: 'db.r6g.2xlarge', region: 'eu-west-1', count: 2, term: 3,
  payment: 'partial-upfront', estimated_savings: 420, upfront_cost: 3600, has_recommendations: true,
  status: 'pending', step_number: n, total_steps: 2,
}));

async function openPlans(page: Page, width: number, query = '', accounts: unknown[] = ACCOUNTS): Promise<void> {
  await page.setViewportSize({ width, height: 900 });
  await seedAuth(page);
  await mockApi(page);
  // After mockApi: Playwright tries the last-registered route first.
  await page.route('**/api/accounts**', (route) => route.fulfill({ json: accounts }));
  await page.route('**/api/plans**', (route) => route.fulfill({ json: { plans: [PLAN] } }));
  await page.route('**/api/purchases/planned', (route) => route.fulfill({ json: { purchases: PLANNED } }));
  await page.goto(`/plans${query}`);
  await expect(page.locator('.planned-purchases-table tbody tr')).toHaveCount(PLANNED.length);
}

const sidebarWidth = (page: Page) =>
  page.locator('.app-sidebar').evaluate((el) => Math.round(el.getBoundingClientRect().width));

for (const width of [915, 1100]) {
  test(`sidebar is icon-only at ${width}px`, async ({ page }) => {
    await openPlans(page, width);
    expect(await sidebarWidth(page)).toBe(64);
    await expect(page.locator('.app-sidebar .sidebar-label').first()).toBeHidden();
  });
}

test('sidebar keeps its labels at 1280px', async ({ page }) => {
  await openPlans(page, 1280);
  expect(await sidebarWidth(page)).toBe(232);
  await expect(page.locator('.app-sidebar .sidebar-label').first()).toBeVisible();
});

for (const width of [780, 915, 1100]) {
  test(`topbar chips stay on one line at ${width}px`, async ({ page }) => {
    await openPlans(page, width);
    const chips = page.locator('.app-topbar .chip-select');
    await expect(chips.first()).toBeVisible();
    for (const chip of await chips.all()) {
      const lines = await chip.locator('.chip-select-value').evaluate((el) => el.getClientRects().length);
      expect(lines).toBe(1);
      await expect(chip.locator('.chip-select-label')).toBeHidden();
      const { top, bottom } = await chip.evaluate((el) => {
        const r = el.getBoundingClientRect();
        return { top: r.top, bottom: r.bottom };
      });
      const value = (await chip.locator('.chip-select-value').boundingBox())!;
      expect(value.y).toBeGreaterThanOrEqual(top);
      expect(value.y + value.height).toBeLessThanOrEqual(bottom);
      expect(value.height).toBeLessThan(24);
    }
    const topbar = await page.locator('.app-topbar').evaluate((el) => el.scrollWidth - el.clientWidth);
    expect(topbar).toBeLessThanOrEqual(0);
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(0);
  });
}

test('sticky Actions column shows a visible hint while columns hide behind it', async ({ page }) => {
  await openPlans(page, 915);
  const table = page.locator('.planned-purchases-table');
  await expect(table).toHaveClass(/scroll-hint-right/);
  const cell = page.locator('.planned-purchases-table tbody tr:first-child > td:last-child');
  // The hint fades in with a short opacity transition.
  await expect.poll(() => cell.evaluate((td) => getComputedStyle(td, '::before').opacity)).toBe('1');
  expect(await cell.evaluate((td) => parseFloat(getComputedStyle(td, '::before').width))).toBeGreaterThanOrEqual(16);
});

const toggleState = async (page: Page) => ({
  width: await sidebarWidth(page),
  expanded: await page.locator('.app-sidebar-toggle').getAttribute('aria-expanded'),
});

for (const [width, start] of [[1024, 64], [1280, 232]] as const) {
  test(`sidebar toggle expands and collapses at ${width}px and aria-expanded follows`, async ({ page }) => {
    await openPlans(page, width);
    const other = start === 64 ? 232 : 64;
    const states = [await toggleState(page)];
    for (let i = 0; i < 2; i++) {
      await page.locator('.app-sidebar-toggle').click();
      // The width animates; wait for it to settle on the target.
      await expect.poll(() => sidebarWidth(page)).toBe(i === 0 ? other : start);
      states.push(await toggleState(page));
    }
    const expected = (w: number) => ({ width: w, expanded: String(w === 232) });
    expect(states).toEqual([expected(start), expected(other), expected(start)]);
  });
}

test('the phone drawer shows full labels even with a stored collapsed sidebar', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('cudly_sidebar_collapsed', '1'));
  await openPlans(page, 600);
  await page.locator('.app-topbar .hamburger').click();
  await expect(page.locator('body')).toHaveClass(/sidebar-open/);
  await expect.poll(() => sidebarWidth(page)).toBe(232);
  await expect(page.locator('.app-sidebar .sidebar-label').first()).toBeVisible();
});

const LONG_ACCOUNT = {
  id: 'acct-long', provider: 'aws', external_id: '333333333333',
  name: 'Shared production platform account for the payments and ledger teams',
};

for (const width of [900, 1280]) {
  test(`a long selected account truncates inside its chip at ${width}px`, async ({ page }) => {
    await openPlans(page, width, `?account=${LONG_ACCOUNT.id}`, [...ACCOUNTS, LONG_ACCOUNT]);
    const value = page.locator('.app-topbar .chip-select-value').filter({ hasText: 'Shared production' });
    await expect(value).toBeVisible();
    const box = await value.evaluate((el) => ({
      clipped: el.scrollWidth > el.clientWidth,
      overflow: getComputedStyle(el).textOverflow,
      chipRight: el.closest('.chip-select')!.getBoundingClientRect().right,
      rootRight: el.closest('.chip-select-root')!.getBoundingClientRect().right,
    }));
    expect(box).toMatchObject({ clipped: true, overflow: 'ellipsis' });
    expect(box.chipRight).toBeLessThanOrEqual(box.rootRight + 1);
    expect(await page.locator('.app-topbar').evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(0);
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(0);
  });
}
