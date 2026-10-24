import { test, expect, type Locator, type Page, type Route } from '@playwright/test';
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

// Fails when the toast's box overlaps Save's; Playwright's click() would
// otherwise only time out on the intercepted pointer event.
async function expectSaveClear(page: Page, toast: Locator): Promise<void> {
  const t = (await toast.boundingBox())!;
  const s = (await page.locator('#ladder-config-save-btn').boundingBox())!;
  const overlaps = t.x < s.x + s.width && s.x < t.x + t.width && t.y < s.y + s.height && s.y < t.y + t.height;
  expect(overlaps, `toast ${JSON.stringify(t)} covers Save ${JSON.stringify(s)}`).toBe(false);
}

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

test('table names the account, Auto Approve warns and confirms, and save errors keep the server text', async ({ page }) => {
  const { saves, errors } = await setup(page);
  await page.route('**/api/accounts/list', route => route.fulfill({ json: [...accounts, { id: 'unavailable-saved-account', name: 'Azure saved', external_id: 'sub-saved', provider: 'azure' }] }));
  await page.reload();
  const firstCell = page.locator('#ladder-configs-table-container tbody td').first();
  await expect(firstCell).toHaveText('Azure saved (sub-saved)');
  await expect(firstCell).toHaveAttribute('title', 'unavailable-saved-account');

  await page.locator('#ladder-add-config-btn').click();
  const warning = page.locator('#ladder-cfg-mode-warning');
  await expect(warning).toBeHidden();
  await page.locator('#ladder-cfg-account').selectOption('registered-aws');
  await page.locator('#ladder-cfg-mode').selectOption('auto_approve');
  await expect(warning).toBeVisible();
  await expect(warning).toContainText('skips human approval');
  await page.locator('#ladder-cfg-enabled').check();
  await page.locator('#ladder-config-save-btn').click();
  const confirm = page.getByRole('dialog', { name: 'Enable Auto Approve?' });
  await expect(confirm).toBeVisible();
  await confirm.getByRole('button', { name: 'Cancel' }).click();
  await expect(confirm).toBeHidden();
  await expect(page.locator('#ladder-config-modal')).toBeVisible();
  expect(saves).toEqual([]);

  await page.route('**/api/ladder/configs', route => route.fulfill({ status: 400, json: { error: 'max_actions_per_run must be between 1 and 50' } }));
  await page.locator('#ladder-config-save-btn').click();
  await confirm.getByRole('button', { name: 'Enable Auto Approve' }).click();
  await expect(page.getByText('Failed to save ladder config: max_actions_per_run must be between 1 and 50')).toBeVisible();
  await expect(page.locator('#ladder-config-modal')).toBeVisible();
  // The rejected PUT logs its 400 and the handler's console.error; nothing else may fail.
  expect(errors.filter(error => !error.includes('400') && !error.startsWith('saveLadderConfig:'))).toEqual([]);
});

test('config form uses compact aligned inputs and scrolls inside the modal on short viewports', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await setup(page);
  await page.locator('#ladder-add-config-btn').click();
  const box = async (id: string) => (await page.locator(`#ladder-cfg-${id}`).boundingBox())!;
  const coverage = await box('target-coverage');
  const fraction = await box('buffer-fraction');
  for (const id of ['target-coverage', 'buffer-fraction', 'lookback-days', 'max-hourly', 'max-actions', 'cadence']) {
    expect((await box(id)).height).toBeLessThan(44);
  }
  // Numeric fields pair up in two aligned columns.
  expect(Math.abs(fraction.y - coverage.y)).toBeLessThan(1);
  expect(fraction.x).toBeGreaterThan(coverage.x + coverage.width);
  for (const id of ['baseline-percentile', 'max-hourly']) expect((await box(id)).x).toBeCloseTo(coverage.x, 0);
  for (const id of ['buf-util-threshold', 'max-actions']) expect((await box(id)).x).toBeCloseTo(fraction.x, 0);
  // The ramp textarea spans both columns, with its help text underneath.
  const ramp = await box('ramp-schedule');
  expect(ramp.x).toBeCloseTo(coverage.x, 0);
  expect(ramp.x + ramp.width).toBeCloseTo(fraction.x + fraction.width, 0);
  const rampHint = (await page.locator('#ladder-cfg-ramp-schedule-hint').boundingBox())!;
  expect(rampHint.y).toBeGreaterThanOrEqual(ramp.y + ramp.height);
  await expect(page.locator('#ladder-cfg-lookback-days')).toHaveAccessibleDescription('Minimum 10');

  // Short viewport: header and Save stay on screen while the body scrolls.
  await page.setViewportSize({ width: 1280, height: 720 });
  const body = page.locator('#ladder-config-modal .modal-body');
  expect(await body.evaluate(el => el.scrollHeight > el.clientHeight)).toBe(true);
  await page.locator('#ladder-cfg-ramp-schedule').scrollIntoViewIfNeeded();
  for (const selector of ['#ladder-modal-title', '#ladder-config-save-btn']) {
    const rect = (await page.locator(selector).boundingBox())!;
    expect(rect.y).toBeGreaterThanOrEqual(0);
    expect(rect.y + rect.height).toBeLessThanOrEqual(720);
  }

  // Narrow viewport: one column.
  await page.setViewportSize({ width: 390, height: 844 });
  expect((await box('buffer-fraction')).x).toBeCloseTo((await box('target-coverage')).x, 0);
});

test('a cleared Buffer Fraction blocks the save instead of sending a zero buffer (#134)', async ({ page }) => {
  const { saves, writes, errors } = await setup(page);
  await page.locator('#ladder-add-config-btn').click();
  await expect(page.locator('#ladder-cfg-account')).toBeEnabled();
  await page.locator('#ladder-cfg-account').selectOption('registered-aws');
  await page.locator('#ladder-cfg-buffer-fraction').fill('');
  await page.locator('#ladder-config-save-btn').click();
  await expect(page.getByText('Buffer fraction is required. Enter a number before saving.')).toBeVisible();
  await expect(page.locator('#ladder-config-modal')).toBeVisible();
  await expect(page.locator('#ladder-cfg-buffer-fraction')).toBeFocused();
  expect(saves).toEqual([]);
  expect(writes).toEqual([]);
  // The error toast stays up for 30s; it must sit beside the modal, not on
  // Save, so the corrected form saves without dismissing it first (#742).
  const toast = page.getByRole('alert').filter({ hasText: 'Buffer fraction is required.' });
  await expect(toast).toBeVisible();
  await expectSaveClear(page, toast);

  await page.locator('#ladder-cfg-buffer-fraction').fill('0');
  await page.locator('#ladder-config-save-btn').click();
  await expect(page.locator('#ladder-config-modal')).toBeHidden();
  expect(saves).toHaveLength(1);
  expect(saves[0]!.buffer_fraction).toBe(0);
  expect(errors).toEqual([]);
});

for (const [width, height] of [[1280, 720], [390, 844]]) {
  test(`Save stays clickable while a save-error toast is showing at ${width}x${height} (#742)`, async ({ page }) => {
    await page.setViewportSize({ width, height });
    const { errors } = await setup(page);
    let puts = 0;
    await page.route('**/api/ladder/configs', route => {
      puts += 1;
      return route.fulfill({ status: 400, json: { error: 'max_actions_per_run must be between 1 and 50' } });
    });
    await page.locator('#ladder-add-config-btn').click();
    await expect(page.locator('#ladder-cfg-account')).toBeEnabled();
    await page.locator('#ladder-cfg-account').selectOption('registered-aws');
    await page.locator('#ladder-config-save-btn').click();
    const toast = page.getByRole('alert').filter({ hasText: 'Failed to save ladder config: max_actions_per_run must be between 1 and 50' });
    await expect(toast).toBeVisible();
    await expect.poll(() => puts).toBe(1);

    await expectSaveClear(page, toast);
    // Retry with the toast still up: the click reaches Save and sends a second PUT.
    await page.locator('#ladder-config-save-btn').click({ timeout: 5000 });
    await expect.poll(() => puts).toBe(2);
    await expect(toast.first()).toBeVisible();
    await expect(page.locator('#ladder-config-modal')).toBeVisible();
    expect(errors.filter(error => !error.includes('400') && !error.startsWith('saveLadderConfig:'))).toEqual([]);
  });
}
