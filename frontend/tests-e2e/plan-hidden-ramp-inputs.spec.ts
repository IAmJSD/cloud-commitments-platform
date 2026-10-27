import { test, expect, Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

// Issue #1536: a preset ramp wrote an out-of-range value into the hidden custom-ramp inputs, so native validation
// blocked Save on an unfocusable control with no POST and no message.
async function openCreateForm(page: Page, posts: unknown[]) {
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1'
    ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/plans**', route => {
    if (route.request().method() === 'POST') {
      posts.push(route.request().postDataJSON());
      return route.fulfill({ json: { id: 'new-plan', name: 'Hidden ramp', enabled: true, services: {} } });
    }
    return route.fulfill({ json: new URL(route.request().url()).pathname.endsWith('/accounts') ? [] : { plans: [] } });
  });
  await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [] } }));
  await page.route('**/api/commitment-options**', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
  await page.goto('/plans');
  await page.locator('#new-plan-btn').click();
  const modal = page.locator('#plan-modal');
  await expect(modal).toBeVisible();
  await modal.locator('#plan-name').fill('Hidden ramp');
  await modal.locator('#plan-account-search').fill('AWS');
  await modal.locator('.account-suggestion-item').first().click();
  return modal;
}

const labels: Record<string, string> = { custom: 'Custom', 'weekly-25pct': 'Weekly 25%', immediate: 'Immediate' };
for (const path of [['custom', 'immediate'], ['weekly-25pct', 'immediate']]) {
  test(`Create plan saves the Immediate ramp after ${path[0]}`, async ({ page }) => {
    const posts: unknown[] = [];
    const modal = await openCreateForm(page, posts);
    await modal.locator('.ramp-option').filter({ hasText: labels[path[0]!]! }).click();
    await expect(modal.locator(`input[name="ramp-schedule"][value="${path[0]}"]`)).toBeChecked();
    await modal.locator('.ramp-option').filter({ hasText: labels.immediate! }).click();
    await expect(modal.locator('input[name="ramp-schedule"][value="immediate"]')).toBeChecked();
    await expect(modal.locator('#custom-ramp-config')).toBeHidden();

    const save = page.getByRole('button', { name: 'Save Plan' });
    await expect(save).toBeEnabled();
    await save.click();
    await expect.poll(() => posts.length).toBe(1);
    expect(posts[0]).toMatchObject({ ramp_schedule: 'immediate' });
  });
}

test('Edit as the first modal, then Custom, lets the user type custom ramp values', async ({ page }) => {
  const puts: unknown[] = [];
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1'
    ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  const plan = { id: 'draft-plan', name: 'Existing plan', enabled: true, auto_purchase: false, notification_days_before: 3,
    services: { ec2: { provider: 'aws', service: 'ec2', enabled: true, term: 1, payment: 'no-upfront', coverage: 80 } },
    ramp_schedule: { type: 'weekly', current_step: 1, total_steps: 4, step_interval_days: 7, percent_per_step: 25 } };
  await page.route('**/api/plans**', route => {
    const url = new URL(route.request().url());
    if (route.request().method() === 'PUT') {
      puts.push(route.request().postDataJSON());
      return route.fulfill({ json: plan });
    }
    return route.fulfill({ json: url.pathname.endsWith('/accounts') ? []
      : url.pathname.endsWith('/draft-plan') ? plan : { plans: [plan] } });
  });
  await page.route('**/api/purchases/planned', route => route.fulfill({ json: { purchases: [] } }));
  await page.route('**/api/commitment-options**', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
  await page.goto('/plans');
  await page.locator('[data-action="edit-plan"][data-id="draft-plan"]').click();
  const modal = page.locator('#plan-modal');
  await expect(modal).toBeVisible();

  await expect(modal.locator('#plan-term')).toHaveValue('1');
  await expect(modal.locator('#plan-payment')).toHaveValue('no-upfront');
  await modal.locator('.ramp-option').filter({ hasText: 'Custom' }).click();
  await expect(modal.locator('#custom-ramp-config')).toBeVisible();
  await expect(modal.locator('#ramp-step-percent')).toBeEnabled();
  await expect(modal.locator('#ramp-interval-days')).toBeEnabled();
  await modal.locator('#ramp-step-percent').fill('30');
  await modal.locator('#ramp-interval-days').fill('14');

  await modal.locator('#plan-account-search').fill('AWS');
  await modal.locator('.account-suggestion-item').first().click();
  await page.getByRole('button', { name: 'Save Plan' }).click();
  await expect.poll(() => puts.length).toBe(1);
  expect(puts[0]).toMatchObject({ ramp_schedule: 'custom', custom_step_percent: 30, custom_interval_days: 14 });
});
