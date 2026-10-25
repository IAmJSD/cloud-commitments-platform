/**
 * Typography foundation (issue #741). jsdom does not cascade stylesheets, so
 * the jest suite can only read the CSS text; these checks read what Chromium
 * actually computes for a real form (Create API Key modal).
 */
import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

test.beforeEach(async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/users', (r) => r.fulfill({ json: { users: [] } }));
  await page.route('**/api/groups', (r) => r.fulfill({ json: { groups: [] } }));
  await page.route('**/api/api-keys', (r) => r.fulfill({ json: { keys: [] } }));
  await page.goto('/admin/users');
  await page.locator('#create-apikey-btn').click();
  await expect(page.locator('#create-apikey-modal')).toBeVisible();
});

test('form inputs use the page font at the same size as the adjacent buttons', async ({ page }) => {
  const font = (selector: string) =>
    page.locator(selector).evaluate((el) => {
      const cs = getComputedStyle(el);
      return { family: cs.fontFamily, size: cs.fontSize };
    });
  const body = await page.evaluate(() => getComputedStyle(document.body).fontFamily);
  const button = await font('#close-create-apikey-modal-btn');

  // Text, password and date inputs used to get three different sizes
  // (14.4px from a modal rule, 13.33px browser default for the others).
  for (const id of ['#apikey-name', '#apikey-password', '#apikey-expires-at']) {
    const input = await font(id);
    expect(input.family, id).toBe(body);
    expect(input.size, id).toBe(button.size);
  }
});

test('body text uses the normal line-height token', async ({ page }) => {
  const { lineHeight, fontSize } = await page.evaluate(() => {
    const cs = getComputedStyle(document.body);
    return { lineHeight: parseFloat(cs.lineHeight), fontSize: parseFloat(cs.fontSize) };
  });
  expect(lineHeight / fontSize).toBeCloseTo(1.5, 2);
});
