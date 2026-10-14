import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';
import type { LadderConfig } from '../src/api/ladder';

for (const viewport of [{ width: 1280, height: 900 }, { width: 390, height: 844 }]) {
  test(`ladder editor preserves selections and resets new configs at ${viewport.width}px`, async ({ page }) => {
    await page.setViewportSize(viewport);
    const errors: string[] = [];
    const failedRequests: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('response', response => { if (response.status() >= 400) failedRequests.push(response.url()); });
    page.on('requestfailed', request => failedRequests.push(request.url()));
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

    const configs: LadderConfig[] = ['aws', 'azure', 'gcp'].map((provider, index) => ({
      id: `config-${index}`, cloud_account_id: `account-${index}`, provider, enabled: true,
      mode: provider === 'aws' ? 'email_approval' : 'auto_approve',
      cadence: provider === 'aws' ? 'daily' : 'weekly',
      target_coverage: 80, buffer_fraction: 0.2, baseline_percentile: 10,
      lookback_days: 14, buffer_utilization_threshold: 85,
      max_hourly_commit_per_run: 5, max_actions_per_run: 3,
      ramp_schedule: { steps: [{ after_days: 0, fraction: 0.5 }, { after_days: 7, fraction: 0.5 }] },
    }));
    const saves: LadderConfig[] = [];
    await page.route('**/api/ladder/configs', async route => {
      if (route.request().method() === 'GET') {
        await route.fulfill({ json: { configs } });
        return;
      }
      expect(route.request().method()).toBe('PUT');
      const saved = route.request().postDataJSON() as LadderConfig;
      saves.push(saved);
      const index = configs.findIndex(config => config.cloud_account_id === saved.cloud_account_id && config.provider === saved.provider);
      if (index < 0) configs.push(saved);
      else configs[index] = saved;
      await route.fulfill({ json: saved });
    });

    await page.route('**/api/ladder/runs?*', route => route.fulfill({ json: { total_count: 0, runs: [] } }));
    await page.route('**/api/ladder/tranches?*', route => route.fulfill({ json: { total_count: 0, total_usd_hr: '0.000000', events: [] } }));
    await page.goto('/admin/purchasing');
    const modal = page.locator('#ladder-config-modal');
    const field = (name: string) => page.locator(`#ladder-cfg-${name}`);
    const edit = (provider: string) => page.locator(`.ladder-edit-btn[data-provider="${provider}"]`).first();

    for (const provider of ['azure', 'gcp', 'aws']) {
      const config = configs.find(row => row.provider === provider)!;
      await edit(provider).click();
      await expect(modal).toBeVisible();
      await expect(field('provider')).toHaveValue(config.provider);
      await expect(field('provider')).toBeDisabled();
      await expect(field('account')).toHaveValue(config.cloud_account_id);
      await expect(field('account')).toBeDisabled();
      await expect(field('id')).toHaveValue(config.id!);
      await expect(field('mode')).toHaveValue(config.mode);
      await expect(field('cadence')).toHaveValue(config.cadence);
      await expect(field('enabled')).toBeChecked();
      for (const [name, value] of Object.entries({
        'target-coverage': 80, 'buffer-fraction': 0.2, 'baseline-percentile': 10,
        'lookback-days': 14, 'buf-util-threshold': 85, 'max-hourly': 5, 'max-actions': 3,
      })) await expect(field(name)).toHaveValue(String(value));
      expect(JSON.parse(await field('ramp-schedule').inputValue())).toEqual(config.ramp_schedule);
      await page.locator(provider === 'gcp' ? '#ladder-modal-close-btn' : '#ladder-modal-cancel-btn').click();
      await expect(modal).toBeHidden();
      expect(saves).toEqual([]);
    }

    for (const [index, target] of [75, 70].entries()) {
      await edit('azure').click();
      const expected = { ...configs.find(row => row.provider === 'azure')!, target_coverage: target };
      await field('target-coverage').fill(String(target));
      await page.locator('#ladder-config-save-btn').click();
      await expect(modal).toBeHidden();
      expect(saves).toHaveLength(index + 1);
      expect(saves[index]).toEqual(expected);
      await expect(page.locator('#ladder-configs-table-container tbody tr').filter({ has: edit('azure') })).toContainText(`${target.toFixed(1)}%`);
      await page.getByRole('button', { name: 'Dismiss notification' }).click();
      await expect(page.getByRole('button', { name: 'Dismiss notification' })).toHaveCount(0);
    }

    await page.locator('#ladder-add-config-btn').click();
    await expect(field('id')).toHaveValue('');
    await expect(field('account')).toHaveValue('');
    await expect(field('account')).toBeEnabled();
    await expect(field('provider')).toBeEnabled();
    await expect(field('provider')).toHaveValue('aws');
    await expect(field('mode')).toHaveValue('email_approval');
    await expect(field('cadence')).toHaveValue('daily');
    await expect(field('enabled')).not.toBeChecked();
    for (const [name, value] of Object.entries({
      'target-coverage': '100', 'buffer-fraction': '0.1', 'baseline-percentile': '5',
      'lookback-days': '30', 'buf-util-threshold': '90', 'max-hourly': '', 'max-actions': '10',
    })) await expect(field(name)).toHaveValue(value);
    expect(JSON.parse(await field('ramp-schedule').inputValue())).toEqual({ steps: [{ after_days: 0, fraction: 1 }] });
    await field('provider').selectOption('gcp');
    await field('account').selectOption('acct-200');
    await field('mode').selectOption('auto_approve');
    await field('cadence').selectOption('weekly');
    await page.locator('#ladder-config-save-btn').click();
    await expect(modal).toBeHidden();
    expect(saves).toHaveLength(3);
    expect(saves[2]).toEqual({
      cloud_account_id: 'acct-200', provider: 'gcp', mode: 'auto_approve', cadence: 'weekly',
      enabled: false, target_coverage: 100, buffer_fraction: 0.1, baseline_percentile: 5,
      lookback_days: 30, buffer_utilization_threshold: 90, max_hourly_commit_per_run: null,
      max_actions_per_run: 10, ramp_schedule: { steps: [{ after_days: 0, fraction: 1 }] },
    });
    await page.getByRole('button', { name: 'Dismiss notification' }).click();
    await page.goto('/admin/general');
    await expect(page).toHaveURL(/\/admin\/general$/);
    await page.goBack();
    await expect(page).toHaveURL(/\/admin\/purchasing$/);
    await expect(edit('azure')).toBeVisible();
    await page.goForward();
    await expect(page).toHaveURL(/\/admin\/general$/);
    expect(saves).toHaveLength(3);
    expect(errors).toEqual([]);
    expect(failedRequests).toEqual([]);
  });
}
