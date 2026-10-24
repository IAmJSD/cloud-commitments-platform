/**
 * Issue #731: dialogs that used to be shown by toggling a class (login gate,
 * upcoming purchase details, ladder config) now go through openModal: they are
 * named dialogs, focus moves inside, Tab stays inside, and Escape closes the
 * dismissible ones and hands focus back to the trigger.
 */
import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';

/** Press Tab `count` times and report whether focus ever left `selector`. */
async function tabStaysInside(page: Page, selector: string, count: number): Promise<boolean> {
  for (let i = 0; i < count; i++) {
    await page.keyboard.press(i % 3 === 2 ? 'Shift+Tab' : 'Tab');
    const inside = await page.evaluate(sel => !!document.querySelector(sel)?.contains(document.activeElement), selector);
    if (!inside) return false;
  }
  return true;
}

test('login gate is a named dialog that holds focus and announces errors', async ({ page }) => {
  await mockApi(page);
  await page.route('**/api/info', route => route.fulfill({ json: { admin_exists: true } }));
  await page.goto('/');
  const dialog = page.getByRole('dialog', { name: 'CUDly Login' });
  await expect(dialog).toBeVisible();
  await expect(page.locator('#login-email')).toBeFocused();
  expect(await tabStaysInside(page, '#login-modal', 8)).toBe(true);

  await page.keyboard.press('Escape');
  await expect(dialog).toBeVisible();

  await page.locator('#login-modal button[type="submit"]').click();
  await expect(page.getByRole('alert')).toHaveText('Enter email and password');
});

test('upcoming purchase details dialog takes focus, closes on Escape and restores focus', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/dashboard/upcoming', route => route.fulfill({ json: { purchases: [{
    execution_id: 'exec-1', plan_id: 'plan-1', plan_name: 'Weekly EC2 ramp', scheduled_date: '2026-11-01T00:00:00Z',
    provider: 'aws', service: 'ec2', step_number: 1, total_steps: 4, estimated_savings: 120, created_by_user_id: 'user-smoke',
  }] } }));
  await page.route('**/api/dashboard/summary**', route => route.fulfill({ json: {
    potential_monthly_savings: 0, total_recommendations: 0, active_commitments: 0, committed_monthly: 0,
    current_coverage: 0, target_coverage: 0, ytd_savings: 0, by_service: {},
  } }));
  await page.goto('/home');
  const view = page.locator('[data-action="view-purchase"]');
  await view.click();

  const dialog = page.getByRole('dialog', { name: 'Upcoming Purchase Details' });
  await expect(dialog).toBeVisible();
  expect(await page.evaluate(() => !!document.getElementById('purchase-details-modal')?.contains(document.activeElement))).toBe(true);
  expect(await tabStaysInside(page, '#purchase-details-modal', 5)).toBe(true);

  await page.keyboard.press('Escape');
  await expect(page.locator('#purchase-details-modal')).toHaveCount(0);
  await expect(view).toBeFocused();
});

test('ladder config modal takes focus and closes on Escape', async ({ page }) => {
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/config', route => route.fulfill({ json: { global: {}, services: [] } }));
  await page.route('**/api/ladder/configs', route => route.fulfill({ json: { configs: [{
    id: 'config-0', cloud_account_id: 'account-0', provider: 'aws', enabled: true, mode: 'email_approval',
    cadence: 'daily', target_coverage: 80, buffer_fraction: 0.2, baseline_percentile: 10, lookback_days: 14,
    buffer_utilization_threshold: 85, max_hourly_commit_per_run: 5, max_actions_per_run: 3,
    ramp_schedule: { steps: [{ after_days: 0, fraction: 1 }] },
  }] } }));
  await page.route('**/api/ladder/runs?*', route => route.fulfill({ json: { total_count: 0, runs: [] } }));
  await page.route('**/api/ladder/tranches?*', route => route.fulfill({ json: { total_count: 0, total_usd_hr: '0.000000', events: [] } }));
  await page.goto('/admin/purchasing');

  const edit = page.locator('.ladder-edit-btn').first();
  await edit.click();
  const modal = page.locator('#ladder-config-modal');
  await expect(modal).toBeVisible();
  expect(await page.evaluate(() => !!document.getElementById('ladder-config-modal')?.contains(document.activeElement))).toBe(true);

  await page.keyboard.press('Escape');
  await expect(modal).toBeHidden();
  await expect(edit).toBeFocused();
});
