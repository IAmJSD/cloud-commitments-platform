/**
 * Navigation keyboard model and route-change focus (issue #730).
 *
 * The sidebar announced itself as an ARIA tablist but implemented none of
 * its keyboard model, and activating an item left focus on the sidebar link
 * with nothing announced. Now the sidebar is plain links with
 * aria-current="page", activation moves focus to the new page's heading,
 * and the in-page sub-tab lists implement arrows/Home/End with a roving
 * tabindex. These run in Chromium because focus after a real key press is
 * what the report is about.
 */

import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

async function openApp(page: Page, path = '/home'): Promise<void> {
  await seedAuth(page);
  await mockApi(page);
  await page.goto(path);
  await expect(page.locator('#admin-tab-btn')).toBeVisible();
}

const activeId = (page: Page): Promise<string> =>
  page.evaluate(() => document.activeElement?.id ?? '');

test('the sidebar is page links with aria-current, not ARIA tabs', async ({ page }) => {
  await openApp(page);
  const nav = page.getByRole('navigation', { name: 'Main navigation' });
  await expect(nav).toBeVisible();
  await expect(page.getByRole('tablist', { name: 'Main navigation' })).toHaveCount(0);
  await expect(nav.locator('[aria-current="page"]')).toHaveAttribute('data-tab', 'home');

  await nav.getByRole('link', { name: 'Plans', exact: true }).click();
  await expect(nav.locator('[aria-current="page"]')).toHaveCount(1);
  await expect(nav.locator('[aria-current="page"]')).toHaveAttribute('data-tab', 'plans');
});

test('Enter on a sidebar link moves focus to the new page heading', async ({ page }) => {
  await openApp(page);
  await page.locator('#opportunities-tab-btn').focus();
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/\/opportunities$/);
  await expect(page.locator('#opportunities-page-heading')).toBeFocused();
  await expect(page.locator('#opportunities-page-heading')).toHaveText('Opportunities');

  // Back restores the previous page and moves focus with it.
  await page.goBack();
  await expect(page).toHaveURL(/\/home$/);
  await expect(page.locator('#home-page-heading')).toBeFocused();
});

test('Admin sub-tabs: one Tab stop, arrows move focus, Enter selects', async ({ page }) => {
  await openApp(page, '/admin/general');
  const tabs = page.getByRole('tablist', { name: 'Admin navigation' }).getByRole('tab');
  await expect(tabs).toHaveCount(4);
  await expect(page.locator('#admin-subtab-general')).toHaveAttribute('tabindex', '0');
  await expect(page.locator('#admin-subtab-purchasing')).toHaveAttribute('tabindex', '-1');

  await page.locator('#admin-subtab-general').focus();
  await page.keyboard.press('ArrowRight');
  expect(await activeId(page)).toBe('admin-subtab-purchasing');
  // Manual activation: focus moved, selection and URL did not.
  await expect(page.locator('#admin-subtab-general')).toHaveAttribute('aria-selected', 'true');
  await expect(page).toHaveURL(/\/admin\/general$/);

  await page.keyboard.press('End');
  expect(await activeId(page)).toBe('admin-subtab-users');
  await page.keyboard.press('ArrowRight'); // wraps to the first tab
  expect(await activeId(page)).toBe('admin-subtab-general');
  await page.keyboard.press('ArrowLeft');
  expect(await activeId(page)).toBe('admin-subtab-users');

  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/\/admin\/users$/);
  await expect(page.locator('#admin-subtab-users')).toHaveAttribute('aria-selected', 'true');
  await expect(page.locator('#admin-subtab-users')).toHaveAttribute('tabindex', '0');
  await expect(page.locator('#admin-subtab-general')).toHaveAttribute('tabindex', '-1');
});

test('Inventory sub-tabs follow the same keyboard model', async ({ page }) => {
  await openApp(page, '/inventory/active-commitments');
  await page.locator('#inv-subtab-active-commitments').focus();
  await page.keyboard.press('ArrowRight');
  expect(await activeId(page)).toBe('inv-subtab-coverage');
  await page.keyboard.press('Home');
  expect(await activeId(page)).toBe('inv-subtab-active-commitments');
  await page.keyboard.press('ArrowLeft');
  expect(await activeId(page)).toBe('inv-subtab-ri-exchange');
  await page.keyboard.press(' ');
  await expect(page).toHaveURL(/\/inventory\/ri-exchange$/);
  await expect(page.locator('#inv-subtab-ri-exchange')).toHaveAttribute('tabindex', '0');
});
