/**
 * One card and one dialog surface (issue #743), checked on computed styles in
 * Chromium (jsdom has no cascade):
 *
 *  - Home's and Opportunities' summary tiles render the same .kpi-tile
 *    component, so their border, radius, shadow, padding and type match.
 *  - A .modal dialog (New Plan) and a confirmDialog (Delete plan) share the
 *    scrim, radius and shadow; they used to differ in all three.
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
  by_service: {},
};

const PLAN = {
  id: 'p1', name: 'Plan', enabled: true, auto_purchase: false, notification_days_before: 3, services: {},
  ramp_schedule: { type: 'weekly', current_step: 1, total_steps: 4, step_interval_days: 7, percent_per_step: 25 },
};

async function open(page: Page, path: string): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/dashboard/summary**', (route) => route.fulfill({ json: DASHBOARD }));
  await page.route('**/api/dashboard/upcoming', (route) => route.fulfill({ json: [] }));
  await page.route('**/api/plans**', (route) =>
    route.request().method() === 'GET' ? route.fulfill({ json: { plans: [PLAN] } }) : route.fallback(),
  );
  await page.route('**/api/purchases/planned', (route) => route.fulfill({ json: { purchases: [] } }));
  await page.goto(path);
}

/** The computed styles that make a tile look like a tile. */
async function tileLook(page: Page, section: string): Promise<Record<string, string>> {
  const tile = page.locator(`${section} > .card`).first();
  await expect(tile).toBeVisible();
  await expect(tile).not.toHaveClass(/skeleton-tile/);
  return tile.evaluate((el) => {
    const cs = getComputedStyle(el);
    const h3 = getComputedStyle(el.querySelector('h3')!);
    const value = getComputedStyle(el.querySelector('.value')!);
    return {
      classes: el.className,
      border: cs.border,
      borderRadius: cs.borderRadius,
      boxShadow: cs.boxShadow,
      padding: cs.padding,
      background: cs.backgroundColor,
      titleFont: `${h3.fontSize} ${h3.fontWeight} ${h3.color}`,
      valueFont: `${value.fontSize} ${value.fontWeight}`,
    };
  });
}

test('Home and Opportunities summary tiles look the same', async ({ page }) => {
  await open(page, '/home');
  const home = await tileLook(page, '#summary');
  await page.goto('/opportunities');
  const opps = await tileLook(page, '#recommendations-summary');
  expect(opps).toEqual(home);
  expect(home.classes).toContain('kpi-tile');
  expect(home.border).toMatch(/^1px solid/);
});

test('a .modal dialog and a confirmDialog share scrim, radius and shadow', async ({ page }) => {
  await open(page, '/plans');

  await page.locator('#new-plan-btn').click();
  const modal = page.locator('#plan-modal');
  await expect(modal).toBeVisible();
  const planLook = await modal.evaluate((el) => ({
    scrim: getComputedStyle(el).backgroundColor,
    radius: getComputedStyle(el.querySelector('.modal-content')!).borderRadius,
    shadow: getComputedStyle(el.querySelector('.modal-content')!).boxShadow,
  }));
  await page.locator('#close-plan-modal-btn').click();
  await expect(modal).toBeHidden();

  await page.locator('[data-action="delete-plan"]').first().click();
  const confirm = page.locator('.modal-confirm');
  await expect(confirm).toBeVisible();
  const confirmLook = await confirm.evaluate((el) => ({
    scrim: getComputedStyle(el.closest('.modal-confirm-backdrop')!).backgroundColor,
    radius: getComputedStyle(el).borderRadius,
    shadow: getComputedStyle(el).boxShadow,
  }));

  expect(confirmLook).toEqual(planLook);
  expect(planLook.radius).toBe('12px');
  expect(planLook.shadow).not.toBe('none');
});
