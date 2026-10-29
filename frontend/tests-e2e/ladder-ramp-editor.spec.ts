import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';
import type { LadderConfig } from '../src/api/ladder';

// The ramp schedule step editor in the ladder config modal: rows edit the
// JSON the save sends, the JSON view edits the rows, and a schedule the
// backend would refuse never reaches the API.

const stored: LadderConfig = {
  id: 'config-0', cloud_account_id: 'account-0', provider: 'aws', enabled: true,
  mode: 'email_approval', cadence: 'daily', target_coverage: 80, buffer_fraction: 0.2,
  baseline_percentile: 10, lookback_days: 14, buffer_utilization_threshold: 85,
  max_hourly_commit_per_run: 5, max_actions_per_run: 3,
  ramp_schedule: { steps: [{ after_days: 0, fraction: 0.5 }, { after_days: 7, fraction: 0.5 }] },
};

async function setup(page: Page): Promise<{ saves: LadderConfig[]; errors: string[] }> {
  const saves: LadderConfig[] = [];
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1'
    ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/config', route => route.fulfill({ json: { global: {}, services: [] } }));
  await page.route('**/api/commitment-options', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
  await page.route('**/api/ri-exchange/config', route => route.fulfill({ json: {
    mode: 'manual', auto_exchange_enabled: false, utilization_threshold: 80,
    max_payment_per_exchange_usd: 0, max_payment_daily_usd: 0, lookback_days: 30,
  } }));
  await page.route('**/api/ladder/configs', async route => {
    if (route.request().method() === 'GET') return route.fulfill({ json: { configs: [stored] } });
    const saved = route.request().postDataJSON() as LadderConfig;
    saves.push(saved);
    await route.fulfill({ json: saved });
  });
  await page.route('**/api/ladder/runs?*', route => route.fulfill({ json: { total_count: 0, runs: [] } }));
  await page.route('**/api/ladder/tranches?*', route => route.fulfill({ json: { total_count: 0, total_usd_hr: '0.000000', events: [] } }));
  await page.goto('/admin/purchasing');
  await page.locator('.ladder-edit-btn').first().click();
  await expect(page.locator('#ladder-config-modal')).toBeVisible();
  return { saves, errors };
}

test('editing ramp steps saves the matching schedule and blocks schedules the backend refuses', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  const { saves, errors } = await setup(page);
  const modal = page.getByRole('dialog', { name: 'Commitment Laddering Config' });
  const summary = page.locator('#ladder-ramp-summary');
  const share = (n: number) => modal.getByLabel(`Step ${n} share (%)`);
  const after = (n: number) => modal.getByLabel(`Step ${n} after (days)`);

  await expect(after(2)).toHaveValue('7');
  await expect(share(2)).toHaveValue('50');
  await expect(summary).toHaveText('Total 100% across 2 steps · last purchase on day 7');
  await expect(page.locator('#ladder-cfg-ramp-schedule')).toBeHidden();

  // A total under 100% shows in the summary and is refused on save.
  await share(1).fill('40');
  await expect(summary).toHaveText('Total 90% across 2 steps · shares must add up to 100%');
  await page.locator('#ladder-config-save-btn').click();
  await expect(page.getByText('Check the ramp schedule. Shares add up to 90%; they must total 100%.')).toBeVisible();
  await expect(share(1)).toBeFocused();
  expect(saves).toEqual([]);

  // Add step fills the remaining 10% a week after the last step, focused.
  await modal.getByRole('button', { name: 'Add step' }).click();
  await expect(after(3)).toBeFocused();
  await expect(after(3)).toHaveValue('14');
  await expect(share(3)).toHaveValue('10');
  await expect(summary).toHaveText('Total 100% across 3 steps · last purchase on day 14');

  // A step that is not later than the previous one is flagged on its row.
  await after(3).fill('7');
  await expect(after(3)).toHaveAttribute('aria-invalid', 'true');
  await expect(after(3)).toHaveAccessibleDescription('Must be later than step 2 (day 7).');
  await after(3).fill('45');
  await expect(after(3)).not.toHaveAttribute('aria-invalid');

  // Remove the middle step from the keyboard; focus stays on a Remove button.
  await modal.getByRole('button', { name: 'Remove step 2' }).focus();
  await page.keyboard.press('Enter');
  await expect(modal.getByRole('button', { name: 'Remove step 2' })).toBeFocused();
  await expect(after(2)).toHaveValue('45');
  await share(1).fill('90');

  // The JSON view shows the same schedule, and editing it redraws the rows.
  await modal.getByRole('button', { name: 'Edit as JSON' }).click();
  const json = page.locator('#ladder-cfg-ramp-schedule');
  await expect(json).toBeVisible();
  expect(JSON.parse(await json.inputValue())).toEqual({ steps: [{ after_days: 0, fraction: 0.9 }, { after_days: 45, fraction: 0.1 }] });
  await json.fill('{"steps": [');
  await expect(page.locator('#ladder-cfg-ramp-schedule-error')).toContainText('Not valid JSON');
  await expect(after(2)).toHaveValue('45');
  await json.fill('{"steps":[{"after_days":0,"fraction":0.6},{"after_days":30,"fraction":0.4}]}');
  await expect(page.locator('#ladder-cfg-ramp-schedule-error')).toBeHidden();
  await expect(share(1)).toHaveValue('60');
  await expect(after(2)).toHaveValue('30');
  await share(2).fill('25');
  await share(1).fill('75');

  await page.locator('#ladder-config-save-btn').click();
  await expect(page.locator('#ladder-config-modal')).toBeHidden();
  expect(saves).toHaveLength(1);
  expect(saves[0]).toEqual({
    ...stored,
    ramp_schedule: { steps: [{ after_days: 0, fraction: 0.75 }, { after_days: 30, fraction: 0.25 }] },
  });
  expect(errors).toEqual([]);
});

test('a preset fills the steps and saves as shown', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const { saves, errors } = await setup(page);
  const modal = page.getByRole('dialog', { name: 'Commitment Laddering Config' });
  await modal.getByRole('button', { name: '4 weekly steps' }).click();
  await expect(page.locator('#ladder-ramp-summary')).toHaveText('Total 100% across 4 steps · last purchase on day 21');
  await expect(page.locator('#ladder-ramp-bar .ramp-bar-seg')).toHaveCount(4);
  await page.locator('#ladder-config-save-btn').click();
  await expect(page.locator('#ladder-config-modal')).toBeHidden();
  expect(saves[0]!.ramp_schedule).toEqual({ steps: [0, 7, 14, 21].map(after_days => ({ after_days, fraction: 0.25 })) });
  expect(errors).toEqual([]);
});
