import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';
import type { LadderEvent, LadderRun } from '../src/api/ladder';
import { ladderTotal } from '../src/ladder-timeline-filters';

for (const width of [1280, 390]) {
  test(`planned graph edits persist through reload at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1' ? route.fallback() : route.abort());
    await seedAuth(page); await mockApi(page);
    await page.route('**/api/commitment-options', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
    await page.route('**/api/ri-exchange/config', route => route.fulfill({ json: { mode: 'manual', auto_exchange_enabled: false, utilization_threshold: 80, max_payment_per_exchange_usd: 0, max_payment_daily_usd: 0, lookback_days: 30 } }));
    await page.route('**/api/ladder/configs', route => route.fulfill({ json: { configs: [{ id: 'config', cloud_account_id: 'acct-001', provider: 'aws', enabled: true, mode: 'email_approval', cadence: 'daily', target_coverage: 80, buffer_fraction: 0.1, baseline_percentile: 5, lookback_days: 30, buffer_utilization_threshold: 50, max_hourly_commit_per_run: 3, max_actions_per_run: 5, ramp_schedule: { steps: [{ after_days: 0, fraction: 1 }] } }] } }));
    const events: LadderEvent[] = [1, 2, 3].map(day => ({ id: `event-${day}`, config_id: 'config', run_id: 'run', layer_type: day === 3 ? 'convertible-ri' : 'compute-sp', term: day === 3 ? '3yr' : '1yr', payment_option: day === 3 ? 'all-upfront' : 'no-upfront', status: 'scheduled', run_status: 'planned', revision: 0, amount_usd_hr: day === 2 ? '0.500000' : '1.000000', scheduled_date: `2030-01-0${day}T00:00:00Z`, created_at: '2026-01-01T00:00:00Z' }));
    const run: LadderRun = { id: 'run', config_id: 'config', status: 'planned', started_at: '2026-01-01T00:00:00Z', created_at: '2026-01-01T00:00:00Z', baseline_usd_hr: null, existing_usd_hr: null, target_usd_hr: null, gap_usd_hr: null, total_hourly_commit: '2.500000', actions: [] };
    const saves: unknown[] = [];
    let releaseSave!: () => void;
    let rejectNextSave = false;
    await page.route('**/api/ladder/runs?*', route => route.fulfill({ json: { total_count: 1, runs: [run] } }));
    await page.route('**/api/ladder/tranches?*', route => route.fulfill({ json: { total_count: events.length, total_usd_hr: ladderTotal(events), events } }));
    await page.route('**/api/ladder/tranches/*', async route => {
      const body = route.request().postDataJSON();
      expect(route.request().method()).toBe('PATCH');
      const event = events.find(item => route.request().url().endsWith(`/${item.id}`))!;
      saves.push(body);
      if (rejectNextSave) { await route.fulfill({ status: 409, json: { error: 'Plan changed' } }); return; }
      if (event.revision !== body.expected_revision) { await route.fulfill({ status: 409, json: { error: 'Plan changed' } }); return; }
      await new Promise<void>(resolve => { releaseSave = resolve; });
      event.amount_usd_hr = body.amount_usd_hr; event.scheduled_date = body.scheduled_date; event.revision++;
      await route.fulfill({ json: { ...event, run_total_usd_hr: '2.250000' } });
    });
    await page.goto('/admin/purchasing');
    const timeline = page.locator('#ladder-purchase-timeline');
    await expect(timeline.locator('[data-summary]')).toContainText('3 of 3');
    await timeline.getByRole('combobox', { name: 'Commitment term', exact: true }).selectOption('3yr');
    await expect(timeline.locator('[data-summary]')).toContainText('1 of 3');
    await expect(timeline.locator('canvas')).toHaveAttribute('aria-label', /planned additions only.*\$1\.00\/hr on Jan 3, 2030 rising to \$1\.00\/hr on Jan 3, 2030/);
    await expect(timeline.locator('[data-table] th', { hasText: 'Cumulative (on-demand-equivalent $/hr)' })).toBeVisible();
    await timeline.getByRole('combobox', { name: 'Payment option', exact: true }).selectOption('all-upfront');
    await expect(timeline.locator('[data-summary]')).toContainText('$1.00/hr in this selection');
    await timeline.getByRole('button', { name: 'Clear filters' }).click();
    const canvas = timeline.locator('canvas');
    await canvas.scrollIntoViewIfNeeded();
    const box = (await canvas.boundingBox())!;
    const modal = page.locator('.ladder-event-editor');
    // The editor opens asynchronously; a click that lands on it after it opened is intercepted, which also means the hit was found.
    search: for (let dx = 0; dx <= 48; dx += 4) {
      for (let dy = -28; dy <= 12; dy += 4) {
        await canvas.click({ position: { x: box.width / 2 + dx, y: box.height / 2 + dy }, timeout: 1000 }).catch(() => undefined);
        if (await modal.isVisible()) break search;
      }
    }
    await expect(modal).toBeVisible();
    await expect(modal.getByLabel('Planning budget (on-demand-equivalent $/hr)')).toHaveValue('0.500000');
    await modal.getByLabel('Planning budget (on-demand-equivalent $/hr)').fill('0.250000');
    await expect(modal.locator('#ladder-event-preview')).toContainText('$2.25/hr');
    await modal.getByRole('button', { name: 'Undo draft changes' }).click();
    await expect(modal.getByLabel('Planning budget (on-demand-equivalent $/hr)')).toHaveValue('0.500000');
    await modal.getByLabel('Scheduled date and time (UTC)').fill('2020-01-01T00:00');
    await modal.getByRole('button', { name: 'Save planned purchase' }).click();
    await expect(modal.locator('[role="alert"]')).toContainText('future');
    expect(saves).toEqual([]);
    await modal.getByLabel('Scheduled date and time (UTC)').fill('2030-01-04T00:00');
    await modal.getByLabel('Planning budget (on-demand-equivalent $/hr)').fill('0.250000');
    await modal.getByRole('button', { name: 'Save planned purchase' }).click();
    await expect(modal.getByRole('button', { name: 'Save in progress' })).toBeFocused();
    await page.keyboard.press('Escape'); await page.keyboard.press('Tab'); await page.keyboard.press('Enter');
    await expect(modal).toBeVisible();
    await expect(modal.getByRole('button', { name: 'Save in progress' })).toBeFocused();
    await expect.poll(() => saves.length).toBe(1);
    releaseSave();
    await expect(modal).toHaveCount(0);
    expect(saves).toHaveLength(1);
    await page.reload();
    await expect(timeline.locator('[data-table]')).toContainText('$0.2500/hr');
    await expect(timeline.locator('[data-table]')).toContainText('Jan 4, 2030, 00:00 UTC');
    await timeline.locator('[data-event="event-2"]').focus(); await page.keyboard.press('Enter');
    await expect(modal.getByLabel('Scheduled date and time (UTC)')).toBeFocused();
    rejectNextSave = true;
    await modal.getByLabel('Planning budget (on-demand-equivalent $/hr)').fill('0.200000');
    await modal.getByRole('button', { name: 'Save planned purchase' }).click();
    await expect(modal.locator('[role="alert"]')).toContainText('draft is retained');
    await expect(modal.getByLabel('Planning budget (on-demand-equivalent $/hr)')).toHaveValue('0.200000');
    await page.keyboard.press('Escape');
    await expect(modal).toHaveCount(0);
    await expect(timeline.locator('[data-event="event-2"]')).toBeFocused();
    for (const provider of ['azure', 'gcp']) {
      await timeline.getByRole('combobox', { name: 'Cloud provider', exact: true }).selectOption(provider);
      await expect(timeline.locator('[data-state]')).toContainText('only for AWS');
    }
    expect(saves).toHaveLength(2);
    expect(errors).toEqual([]);
  });
}
