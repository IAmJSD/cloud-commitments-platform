/**
 * Keyboard focus return and ARIA wiring for issue #729.
 *
 * - UX-27: Escape in an RI Exchange column-filter popover returns focus to
 *   the column's filter button instead of dropping it to <body>.
 * - UX-31: the chip-select search input carries aria-activedescendant while
 *   it holds focus, and picking an option returns focus to the chip.
 * - UX-36: #topbar-filters is a labelled group.
 *
 * Driven with real key presses in Chromium, since "where does focus land"
 * is the whole point and jsdom does not move focus on its own.
 */

import { test, expect, type Page } from '@playwright/test';
import { ACCOUNTS, mockApi, seedAuth } from './fixtures/recs';

/* Enough accounts to push the Account chip past its 8-option search threshold. */
const MANY_ACCOUNTS = [
  ...ACCOUNTS,
  ...Array.from({ length: 6 }, (_, i) => ({
    id: `acct-30${i}`, name: `AWS Extra ${i} (acct-30${i})`, provider: 'aws', external_id: `30000000000${i}`,
  })),
];

const activeId = (page: Page): Promise<string> =>
  page.evaluate(() => {
    const el = document.activeElement as HTMLElement | null;
    return el ? `${el.tagName}#${el.id}.${el.className}` : '';
  });

test('Escape in the Active RI filter popover returns focus to its button', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/ri-exchange/**', async (route) => {
    const end = new URL(route.request().url()).pathname.split('/').pop()!;
    const responses: Record<string, unknown> = {
      instances: { instances: [{
        reserved_instance_id: 'ri-1', instance_type: 'm5.large', instance_count: 2, region: 'us-east-1',
        state: 'active', offering_class: 'convertible', offering_type: 'No Upfront',
        product_description: 'Linux/UNIX', start: '2026-01-01T00:00:00Z', end: '2027-01-01T00:00:00Z',
      }] },
      utilization: { utilization: [] }, 'reshape-recommendations': { recommendations: [] }, history: { records: [] },
    };
    await route.fulfill({ json: responses[end] ?? {} });
  });
  await page.goto('/inventory/ri-exchange');

  const btn = page.locator('#ri-exchange-instances-list .column-filter-btn[data-column="instance_type"]');
  await expect(btn).toBeVisible();
  await btn.focus();
  await page.keyboard.press('Enter');
  const popover = page.locator('.column-filter-popover');
  await expect(popover).toBeVisible();
  expect(await popover.evaluate((el) => el.contains(document.activeElement))).toBe(true);

  await page.keyboard.press('Escape');
  await expect(popover).toHaveCount(0);
  await expect(btn).toBeFocused();
  await expect(btn).toHaveAttribute('aria-expanded', 'false');
});

test('Account chip search: activedescendant on the input, focus back on the chip after a pick', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/accounts**', (route) => route.fulfill({ json: MANY_ACCOUNTS }));
  await page.goto('/home');

  const chip = page.locator('#topbar-filters button[aria-label="Account"]');
  await expect(chip).toBeVisible();
  await chip.focus();
  await page.keyboard.press('Enter');
  const search = page.locator('#topbar-filters .chip-select-search');
  await expect(search).toBeFocused();

  await page.keyboard.type('Extra 3');
  await page.keyboard.press('ArrowDown');
  const descId = await search.getAttribute('aria-activedescendant');
  expect(descId).toBeTruthy();
  await expect(page.locator(`[id="${descId}"]`)).toContainText('AWS Extra 3 (acct-303)');
  await expect(chip).not.toHaveAttribute('aria-activedescendant', /.+/);

  await page.keyboard.press('Enter');
  await expect(search).toBeHidden();
  await expect(chip).toBeFocused();
  await expect(chip).toContainText('AWS Extra 3');
  expect(await activeId(page)).toContain('chip-select');
});

test('the global filters container is a labelled group', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.goto('/home');
  await expect(page.getByRole('group', { name: 'Global filters' })).toHaveAttribute('id', 'topbar-filters');
});
