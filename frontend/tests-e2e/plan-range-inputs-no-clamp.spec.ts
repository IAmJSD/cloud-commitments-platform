import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #1534: Save blurs the field first, so a blur clamp saved a value the user never typed.
// Out-of-range ramp and notification values must stay as typed and be refused on Save
// (the browser's own range validation blocks the submit; savePlan's toast is the backstop).
test('Create plan keeps out-of-range ramp and notify values and refuses them on Save', async ({ page }) => {
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1'
    ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  const posts: unknown[] = [];
  await page.route('**/api/plans**', route => {
    if (route.request().method() === 'POST') {
      posts.push(route.request().postDataJSON());
      return route.fulfill({ json: { id: 'new-plan', name: 'No clamp', enabled: true, services: {} } });
    }
    return route.fulfill({ json: new URL(route.request().url()).pathname.endsWith('/accounts') ? [] : { plans: [] } });
  });
  await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [] } }));
  await page.route('**/api/commitment-options**', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
  await page.goto('/plans');

  await page.locator('#new-plan-btn').click();
  const modal = page.locator('#plan-modal');
  await expect(modal).toBeVisible();
  await modal.locator('#plan-name').fill('No clamp');
  await modal.locator('#plan-account-search').fill('AWS');
  await modal.locator('.account-suggestion-item').first().click();
  await page.locator('#plan-modal label.ramp-option').filter({ hasText: 'Define your own schedule' }).click();
  const save = page.getByRole('button', { name: 'Save Plan' });
  await expect(save).toBeEnabled();

  const step = modal.locator('#ramp-step-percent');
  await step.fill('150');
  await save.click();
  await expect(step).toHaveAttribute('aria-invalid', 'true');
  await expect(step).toHaveValue('150');
  expect(posts).toHaveLength(0);
  await step.fill('20');

  await modal.locator('#plan-advanced-settings summary').click();
  const notify = modal.locator('#plan-notify-days');
  await notify.fill('0');
  await save.click();
  await expect(notify).toHaveAttribute('aria-invalid', 'true');
  await expect(notify).toHaveValue('0');
  expect(posts).toHaveLength(0);

  await notify.fill('3');
  await save.click();
  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0]).toMatchObject({ notification_days_before: 3, custom_step_percent: 20 });
});
