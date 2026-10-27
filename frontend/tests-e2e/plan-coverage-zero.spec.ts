import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #523: the plan executor rejects a 0% target, so the form refuses 0 (without clamping it to 1) and an
// emptied field (Number('') is 0) and posts the lowest valid target, 1, as 1.
test('Create plan refuses an empty or 0% coverage and posts 1 as 1', async ({ page }) => {
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1'
    ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  const posts: unknown[] = [];
  await page.route('**/api/plans**', route => {
    if (route.request().method() === 'POST') {
      posts.push(route.request().postDataJSON());
      return route.fulfill({ json: { id: 'new-plan', name: 'Zero target', enabled: true, services: {} } });
    }
    return route.fulfill({ json: new URL(route.request().url()).pathname.endsWith('/accounts') ? [] : { plans: [] } });
  });
  await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [] } }));
  await page.route('**/api/commitment-options**', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
  await page.goto('/plans');

  await page.locator('#new-plan-btn').click();
  const modal = page.locator('#plan-modal');
  await expect(modal).toBeVisible();
  await modal.locator('#plan-name').fill('Zero target');
  await modal.locator('#plan-account-search').fill('AWS');
  await modal.locator('.account-suggestion-item').first().click();
  const save = page.getByRole('button', { name: 'Save Plan' });
  await expect(save).toBeEnabled();

  const coverage = modal.locator('#plan-coverage');
  await coverage.fill('');
  await save.click();
  await expect(page.locator('.toast, [role="status"]').filter({ hasText: 'Target Coverage must be a whole number between 1 and 100' }).first()).toBeVisible();
  expect(posts).toHaveLength(0);

  // Clicking Save blurs the field; it must keep 0 and be refused, not clamped to 1.
  await coverage.fill('0');
  await save.click();
  await expect(page.locator('.toast, [role="status"]').filter({ hasText: 'Target Coverage must be a whole number between 1 and 100' }).first()).toBeVisible();
  await expect(coverage).toHaveValue('0');
  expect(posts).toHaveLength(0);

  await modal.locator('#plan-coverage').fill('1');
  await save.click();
  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0]).toMatchObject({ target_coverage: 1 });
});
