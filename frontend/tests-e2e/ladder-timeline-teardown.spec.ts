import { test, expect, type Page } from '@playwright/test';
import { mockApi, seedAuth } from './fixtures/recs';
import type { LadderEvent, LadderRun } from '../src/api/ladder';

// Issue #667: the planned-purchase Chart.js instance (and the ResizeObserver
// it attaches) is released when the user leaves the Purchasing settings
// section, and rebuilt when they come back.
async function liveLadderObservers(page: Page): Promise<number> {
  return page.evaluate(() => (window as unknown as { __ladderObservers: () => number }).__ladderObservers());
}

test('leaving Purchasing settings disposes the ladder chart and returning rebuilds it', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => {
    // Track which ResizeObserver targets inside the ladder timeline are live.
    const live = new Map<ResizeObserver, Set<Element>>();
    const proto = ResizeObserver.prototype;
    const { observe, unobserve, disconnect } = proto;
    proto.observe = function (this: ResizeObserver, target: Element, options?: ResizeObserverOptions) {
      if (target.closest('#ladder-purchase-timeline')) {
        if (!live.has(this)) live.set(this, new Set());
        live.get(this)!.add(target);
      }
      return observe.call(this, target, options);
    };
    proto.unobserve = function (this: ResizeObserver, target: Element) { live.get(this)?.delete(target); return unobserve.call(this, target); };
    proto.disconnect = function (this: ResizeObserver) { live.delete(this); return disconnect.call(this); };
    (window as unknown as { __ladderObservers: () => number }).__ladderObservers = () => Array.from(live.values()).reduce((sum, set) => sum + set.size, 0);
  });
  await page.route('**/*', route => new URL(route.request().url()).hostname === '127.0.0.1' ? route.fallback() : route.abort());
  await seedAuth(page); await mockApi(page);
  await page.route('**/api/commitment-options', route => route.fulfill({ json: { status: 'ok', aws: {} } }));
  await page.route('**/api/ri-exchange/config', route => route.fulfill({ json: { mode: 'manual', auto_exchange_enabled: false, utilization_threshold: 80, max_payment_per_exchange_usd: 0, max_payment_daily_usd: 0, lookback_days: 30 } }));
  await page.route('**/api/ladder/configs', route => route.fulfill({ json: { configs: [] } }));
  const events: LadderEvent[] = [1, 2].map(day => ({ id: `event-${day}`, config_id: 'config', run_id: 'run', layer_type: 'compute-sp', term: '1yr', payment_option: 'no-upfront', status: 'scheduled', run_status: 'planned', revision: 0, amount_usd_hr: '1.000000', scheduled_date: `2030-01-0${day}T00:00:00Z`, created_at: '2026-01-01T00:00:00Z' }));
  const run: LadderRun = { id: 'run', config_id: 'config', status: 'planned', started_at: '2026-01-01T00:00:00Z', created_at: '2026-01-01T00:00:00Z', baseline_usd_hr: null, existing_usd_hr: null, target_usd_hr: null, gap_usd_hr: null, total_hourly_commit: '2.000000', actions: [] };
  let trancheLoads = 0;
  await page.route('**/api/ladder/runs?*', route => route.fulfill({ json: { total_count: 1, runs: [run] } }));
  await page.route('**/api/ladder/tranches?*', route => { trancheLoads++; return route.fulfill({ json: { total_count: events.length, total_usd_hr: '2.000000', events } }); });

  await page.goto('/admin/purchasing');
  const timeline = page.locator('#ladder-purchase-timeline');
  await expect(timeline.locator('[data-summary]')).toContainText('2 of 2');
  await expect.poll(() => liveLadderObservers(page)).toBeGreaterThan(0);
  const loadsAfterFirstVisit = trancheLoads;

  await page.locator('#admin-subtab-general').click();
  await expect.poll(() => liveLadderObservers(page)).toBe(0);
  await page.waitForLoadState('networkidle');
  // The General sub-tab reloads settings but must not rebuild the hidden chart.
  expect(await liveLadderObservers(page)).toBe(0);
  expect(trancheLoads).toBe(loadsAfterFirstVisit);

  await page.locator('#admin-subtab-purchasing').click();
  await expect(timeline.locator('canvas')).toBeVisible();
  await expect.poll(() => liveLadderObservers(page)).toBeGreaterThan(0);
  expect(trancheLoads).toBeGreaterThan(loadsAfterFirstVisit);

  await page.locator('#plans-tab-btn').click();
  await expect(page).toHaveURL(/\/plans$/);
  await expect.poll(() => liveLadderObservers(page)).toBe(0);

  await page.goBack();
  await expect(page).toHaveURL(/\/admin\/purchasing$/);
  await expect(timeline.locator('[data-summary]')).toContainText('2 of 2');
  await expect.poll(() => liveLadderObservers(page)).toBeGreaterThan(0);
  expect(errors).toEqual([]);
});
