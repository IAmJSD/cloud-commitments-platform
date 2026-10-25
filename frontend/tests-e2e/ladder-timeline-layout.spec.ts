import { test, expect } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';
import type { LadderEvent, LadderRun } from '../src/api/ladder';

// Issue #753: the planned-purchases block is its own card, labels sit above
// their controls in small muted text, and the date filters are typed in UTC
// as YYYY-MM-DD HH:MM instead of a browser-locale datetime picker.
for (const width of [1280, 390]) {
  test(`planned purchases card lays out filters and UTC dates at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1' ? route.fallback() : route.abort());
    await seedAuth(page); await mockApi(page);
    await page.route('**/api/commitment-options', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
    await page.route('**/api/ri-exchange/config', route => route.fulfill({ json: { mode: 'manual', auto_exchange_enabled: false, utilization_threshold: 80, max_payment_per_exchange_usd: 0, max_payment_daily_usd: 0, lookback_days: 30 } }));
    await page.route('**/api/ladder/configs', route => route.fulfill({ json: { configs: [] } }));
    const events: LadderEvent[] = [1, 2].map(day => ({ id: `event-${day}`, config_id: 'config', run_id: 'run', layer_type: 'compute-sp', term: '1yr', payment_option: 'no-upfront', status: 'scheduled', run_status: 'planned', revision: 0, amount_usd_hr: '1.000000', scheduled_date: `2030-01-0${day}T12:00:00Z`, created_at: '2026-01-01T00:00:00Z' }));
    const run: LadderRun = { id: 'run', config_id: 'config', status: 'planned', started_at: '2026-01-01T00:00:00Z', created_at: '2026-01-01T00:00:00Z', baseline_usd_hr: null, existing_usd_hr: null, target_usd_hr: null, gap_usd_hr: null, total_hourly_commit: '2.000000', actions: [] };
    await page.route('**/api/ladder/runs?*', route => route.fulfill({ json: { total_count: 1, runs: [run] } }));
    await page.route('**/api/ladder/tranches?*', route => route.fulfill({ json: { total_count: events.length, total_usd_hr: '2.000000', events } }));
    await page.goto('/admin/purchasing');

    const card = page.locator('#ladder-purchase-timeline > fieldset.settings-category');
    await expect(card.locator('legend')).toHaveText('Planned purchases');
    await expect(card.locator('[data-summary]')).toContainText('2 of 2');
    // A separate card, not nested inside the laddering settings card.
    expect(await card.evaluate(el => el.parentElement!.closest('fieldset'))).toBeNull();

    const term = card.getByRole('combobox', { name: 'Commitment term', exact: true });
    const termLabel = card.locator('label[for="ladder-timeline-term"]');
    const [labelBox, termBox] = [(await termLabel.boundingBox())!, (await term.boundingBox())!];
    expect(labelBox.y + labelBox.height).toBeLessThanOrEqual(termBox.y + 1);
    expect(await termLabel.evaluate(el => getComputedStyle(el).fontSize)).toBe('12px');

    const from = card.getByRole('textbox', { name: 'From (UTC)' });
    const to = card.getByRole('textbox', { name: 'To (UTC)' });
    await expect(from).toHaveValue(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}(:\d{2})?$/);
    await to.fill('2030-01-01');
    await to.press('Enter');
    await to.blur();
    await expect(to).toHaveValue('2030-01-01 23:59:59');
    await expect(card.locator('[data-summary]')).toContainText('1 of 2');
    await to.fill('01.02.2030');
    await to.blur();
    await expect(to).toHaveAttribute('aria-invalid', 'true');
    await expect(card.getByRole('alert')).toContainText('not a valid UTC date');
    await expect(card.locator('[data-summary]')).toContainText('1 of 2');
    await card.getByRole('button', { name: 'Clear filters' }).click();
    await expect(card.getByRole('alert')).toBeHidden();
    await expect(card.locator('[data-summary]')).toContainText('2 of 2');

    const overflow = await card.evaluate(el => el.scrollWidth - el.clientWidth);
    expect(overflow).toBeLessThanOrEqual(1);
    expect(errors).toEqual([]);
  });
}
