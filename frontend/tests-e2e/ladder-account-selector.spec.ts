import { test, expect, type Page, type Route } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';
import type { LadderConfig } from '../src/api/ladder';

const accounts = [
  { id: 'registered-aws', name: '<img src=x onerror=alert(1)>', external_id: '<external>', provider: 'aws' },
  { id: 'registered-azure', name: 'Azure fixture', external_id: 'subscription-fixture', provider: 'azure' },
  { id: 'registered-gcp', name: 'GCP fixture', external_id: 'project-fixture', provider: 'gcp' },
];
const existing: LadderConfig = {
  id: 'saved-config', cloud_account_id: 'unavailable-saved-account', provider: 'azure', enabled: false,
  mode: 'email_approval', cadence: 'daily', target_coverage: 80, buffer_fraction: 0.1,
  baseline_percentile: 5, lookback_days: 30, buffer_utilization_threshold: 90,
  max_hourly_commit_per_run: null, max_actions_per_run: 10,
  ramp_schedule: { steps: [{ after_days: 0, fraction: 1 }] },
};

async function setup(page: Page): Promise<{ saves: LadderConfig[]; writes: string[]; errors: string[]; failures: string[] }> {
  const saves: LadderConfig[] = [];
  const writes: string[] = [];
  const errors: string[] = [];
  const failures: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => {
    if (message.type() === 'error' && !(message.text().includes('403') && message.location().url.endsWith('/api/accounts/list'))) errors.push(message.text());
  });
  page.on('response', response => { if (response.status() >= 400) failures.push(`${response.status()} ${new URL(response.url()).pathname}`); });
  page.on('requestfailed', request => failures.push(`${request.failure()?.errorText} ${new URL(request.url()).pathname}`));
  page.on('request', request => { if (request.method() !== 'GET') writes.push(new URL(request.url()).pathname); });
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1' ? route.fallback() : route.abort());
  await seedAuth(page);
  await mockApi(page);
  await page.route('**/api/config', route => route.fulfill({ json: { global: {}, services: [] } }));
  await page.route('**/api/commitment-options', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
  await page.route('**/api/ri-exchange/config', route => route.fulfill({ json: {
    mode: 'manual', auto_exchange_enabled: false, utilization_threshold: 80,
    max_payment_per_exchange_usd: 0, max_payment_daily_usd: 0, lookback_days: 30,
  } }));
  await page.route('**/api/accounts/list', route => route.fulfill({ json: accounts }));
  await page.route('**/api/ladder/configs', async route => {
    if (route.request().method() === 'GET') return route.fulfill({ json: { configs: [existing] } });
    expect(route.request().method()).toBe('PUT');
    const saved = route.request().postDataJSON() as LadderConfig;
    saves.push(saved);
    await route.fulfill({ json: saved });
  });
  await page.route('**/api/ladder/runs?*', route => route.fulfill({ json: { total_count: 0, runs: [] } }));
  await page.route('**/api/ladder/tranches?*', route => route.fulfill({ json: { total_count: 0, total_usd_hr: '0.000000', events: [] } }));
  await page.goto('/admin/purchasing');
  await expect(page.locator('#ladder-add-config-btn')).toBeVisible();
  await expect(page.locator('#global-settings-form')).not.toHaveClass(/hidden/);
  return { saves, writes, errors, failures };
}

for (const width of [1280, 390]) {
  test(`registered account selection filters providers and saves the internal ID at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    const { saves, writes, errors, failures } = await setup(page);
    const modal = page.getByRole('dialog', { name: 'Commitment Laddering Config' });
    const account = modal.getByLabel('Cloud Account', { exact: true });
    const provider = modal.getByLabel('Provider', { exact: true });
    await page.locator('#ladder-add-config-btn').click();
    await expect(account).toBeEnabled();
    await expect(account).toHaveValue('');
    await expect(page.locator('#ladder-config-save-btn')).toBeDisabled();
    await expect(account.locator('option')).toHaveText(['Select a registered account', '<img src=x onerror=alert(1)> (<external>)']);
    await expect(page.locator('#ladder-config-modal img')).toHaveCount(0);
    await account.selectOption('registered-aws');
    await provider.selectOption('gcp');
    await expect(account).toHaveValue('');
    await expect(account.locator('option')).toHaveText(['Select a registered account', 'GCP fixture (project-fixture)']);
    await expect(page.locator('#ladder-config-save-btn')).toBeDisabled();
    await account.selectOption('registered-gcp');
    await page.locator('#ladder-config-save-btn').click();
    await expect(page.locator('#ladder-config-modal')).toBeHidden();
    expect(saves).toHaveLength(1);
    expect(saves[0]).toMatchObject({ cloud_account_id: 'registered-gcp', provider: 'gcp' });
    expect(writes).toEqual(['/api/ladder/configs']);
    expect(errors).toEqual([]);
    expect(failures).toEqual([]);
  });
}

test('a forbidden list blocks creation but preserves an unavailable edit identity', async ({ page }) => {
  const { saves, writes, errors, failures } = await setup(page);
  await page.route('**/api/accounts/list', route => route.fulfill({ status: 403, json: { error: 'Forbidden' } }));
  await page.locator('#ladder-add-config-btn').click();
  await expect(page.locator('#ladder-cfg-account-status')).toContainText('Could not load registered accounts');
  await expect(page.locator('#ladder-cfg-account')).toBeDisabled();
  await expect(page.locator('#ladder-config-save-btn')).toBeDisabled();
  await page.locator('#ladder-modal-cancel-btn').click();
  await page.locator('.ladder-edit-btn').click();
  await expect(page.locator('#ladder-cfg-account-status')).toContainText('Saved identity is unchanged');
  await expect(page.locator('#ladder-cfg-account')).toHaveValue(existing.cloud_account_id);
  await expect(page.locator('#ladder-cfg-account')).toBeDisabled();
  await expect(page.locator('#ladder-cfg-provider')).toBeDisabled();
  await page.locator('#ladder-cfg-target-coverage').fill('75');
  await page.locator('#ladder-config-save-btn').click();
  await expect(page.locator('#ladder-config-modal')).toBeHidden();
  expect(saves).toEqual([{ ...existing, target_coverage: 75 }]);
  expect(writes).toEqual(['/api/ladder/configs']);
  expect(errors).toEqual([]);
  expect(failures).toEqual(['403 /api/accounts/list', '403 /api/accounts/list']);
});

test('provider changes during loading and stale reopened responses keep the current account list', async ({ page }) => {
  const { saves, writes, errors, failures } = await setup(page);
  let pending!: Route;
  await page.route('**/api/accounts/list', route => { pending = route; });
  await page.locator('#ladder-add-config-btn').click();
  await expect(page.locator('#ladder-cfg-account-status')).toContainText('Loading registered accounts');
  await expect.poll(() => pending !== undefined).toBe(true);
  await page.locator('#ladder-cfg-provider').selectOption('gcp');
  await pending.fulfill({ json: accounts });
  await expect(page.locator('#ladder-cfg-account option')).toHaveText(['Select a registered account', 'GCP fixture (project-fixture)']);
  const completed = pending;
  await page.locator('#ladder-modal-cancel-btn').click();
  await page.locator('#ladder-add-config-btn').click();
  await expect(page.locator('#ladder-cfg-account-status')).toContainText('Loading registered accounts');
  await expect.poll(() => pending !== completed).toBe(true);
  const stale = pending;
  await page.locator('#ladder-modal-cancel-btn').click();
  await page.route('**/api/accounts/list', route => route.fulfill({ json: accounts }));
  await page.locator('#ladder-add-config-btn').click();
  await expect(page.locator('#ladder-cfg-account')).toBeEnabled();
  await page.locator('#ladder-cfg-account').selectOption('registered-aws');
  await stale.fulfill({ json: [] });
  await expect(page.locator('#ladder-cfg-account')).toHaveValue('registered-aws');
  expect(saves).toEqual([]);
  expect(writes).toEqual([]);
  expect(errors).toEqual([]);
  expect(failures).toEqual([]);
});
