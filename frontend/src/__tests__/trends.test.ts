/**
 * Inventory & Coverage -> Trends (issue #30): data shaping in
 * lib/trends-data.ts and the fetch/render flow in trends.ts.
 */

const mockChartInstance = { destroy: jest.fn() };
jest.mock('chart.js', () => {
  const MockChart = jest.fn().mockImplementation(() => mockChartInstance) as jest.Mock & { register: jest.Mock };
  MockChart.register = jest.fn();
  return { Chart: MockChart, registerables: [] };
});

jest.mock('../api', () => ({
  getAnalyticsTrends: jest.fn(),
}));

jest.mock('../state', () => ({
  getCurrentProvider: jest.fn(() => ''),
  getCurrentAccountIDs: jest.fn(() => []),
}));

import { Chart } from 'chart.js';
import * as api from '../api';
import * as state from '../state';
import { loadTrends, trendsErrorMessage } from '../trends';
import {
  trendsWindow,
  monthKeysBetween,
  monthKeyOf,
  monthLabel,
  buildMonthlySeries,
  filterByProvider,
  latestWithSavings,
  formatCoverage,
  meanOrNull,
} from '../lib/trends-data';
import type { AnalyticsTrendsResponse, TrendsMonthlyRow } from '../api';
import type { ApiError } from '../api/types';

function row(month: string, provider: string, savings: number, coverage?: number | null): TrendsMonthlyRow {
  return {
    month: `${month}-01T00:00:00Z`,
    account_id: 'acct-001-external',
    provider,
    service: 'ec2',
    total_savings: savings,
    avg_coverage: coverage,
    snapshot_count: 30,
  };
}

describe('trends-data', () => {
  test('trendsWindow starts on the 1st of the oldest month and stays inside the 366-day cap', () => {
    const now = new Date('2026-10-23T15:00:00Z');
    const { start, end } = trendsWindow(12, now);
    expect(start.toISOString()).toBe('2025-11-01T00:00:00.000Z');
    expect(end).toBe(now);
    expect((end.getTime() - start.getTime()) / 86_400_000).toBeLessThan(366);
    expect(trendsWindow(3, now).start.toISOString()).toBe('2026-08-01T00:00:00.000Z');
  });

  test('monthKeysBetween lists every month across a year boundary', () => {
    expect(monthKeysBetween(new Date('2025-11-01T00:00:00Z'), new Date('2026-02-10T00:00:00Z')))
      .toEqual(['2025-11', '2025-12', '2026-01', '2026-02']);
  });

  test('monthKeyOf keeps the server month even with a UTC offset', () => {
    expect(monthKeyOf('2026-09-01T00:00:00+02:00')).toBe('2026-09');
    expect(monthKeyOf('garbage')).toBeNull();
    expect(monthLabel('2026-09')).toBe('Sep 2026');
  });

  test('buildMonthlySeries sums savings, averages present coverage and leaves gaps null', () => {
    const keys = ['2026-07', '2026-08', '2026-09'];
    const series = buildMonthlySeries([
      row('2026-07', 'aws', 100, 40),
      row('2026-07', 'azure', 50, null),
      row('2026-07', 'gcp', 25, 60),
      row('2026-09', 'aws', 80),
      row('2025-01', 'aws', 999, 99), // outside the window
    ], keys);
    expect(series).toEqual([
      { key: '2026-07', label: 'Jul 2026', savings: 175, coverage: 50 },
      { key: '2026-08', label: 'Aug 2026', savings: null, coverage: null },
      { key: '2026-09', label: 'Sep 2026', savings: 80, coverage: null },
    ]);
    expect(latestWithSavings(series)?.key).toBe('2026-09');
    expect(latestWithSavings(buildMonthlySeries([], keys))).toBeNull();
  });

  test('a genuine $0 month stays 0, not a gap', () => {
    expect(buildMonthlySeries([row('2026-08', 'aws', 0)], ['2026-08'])[0]?.savings).toBe(0);
  });

  test('filterByProvider narrows only when a provider is set', () => {
    const rows = [{ provider: 'aws' }, { provider: 'gcp' }];
    expect(filterByProvider(rows, '')).toEqual(rows);
    expect(filterByProvider(rows, 'gcp')).toEqual([{ provider: 'gcp' }]);
  });

  test('formatCoverage and meanOrNull never turn absent data into zero', () => {
    expect(formatCoverage(undefined)).toBe('N/A');
    expect(formatCoverage(null)).toBe('N/A');
    expect(formatCoverage(0)).toBe('0.0%');
    expect(formatCoverage(54.25)).toBe('54.3%');
    expect(meanOrNull([null, undefined])).toBeNull();
    expect(meanOrNull([10, null, 20])).toBe(15);
  });
});

describe('loadTrends', () => {
  const keys = monthKeysBetween(trendsWindow(6).start, trendsWindow(6).end);
  const latest = keys[keys.length - 1]!;
  const previous = keys[keys.length - 2]!;

  function response(overrides: Partial<AnalyticsTrendsResponse> = {}): AnalyticsTrendsResponse {
    return {
      start: '', end: '', months: 6,
      monthly: [row(previous, 'aws', 300, 70), row(previous, 'gcp', 100), row(latest, 'aws', 400, 80)],
      by_provider: [
        { provider: 'aws', service: 'ec2', total_savings: 350, avg_coverage: 75 },
        { provider: 'gcp', service: 'compute', total_savings: 100 },
      ],
      by_service: [{ service: 'ec2', region: 'us-east-1', total_savings: 350, avg_coverage: 75 }],
      ...overrides,
    };
  }

  function buildDOM(): void {
    document.body.innerHTML = `
      <select id="trends-period"><option value="3">3</option><option value="6" selected>6</option><option value="12">12</option></select>
      <button id="trends-refresh-btn">Refresh</button>
      <div id="trends-content"></div>`;
  }

  const content = (): HTMLElement => document.getElementById('trends-content')!;
  const cells = (id: string): string[][] =>
    Array.from(document.querySelectorAll(`#${id} tbody tr`)).map((tr) =>
      Array.from(tr.querySelectorAll('td')).map((td) => td.textContent ?? ''));

  beforeEach(() => {
    buildDOM();
    (api.getAnalyticsTrends as jest.Mock).mockReset().mockResolvedValue(response());
    (state.getCurrentProvider as jest.Mock).mockReturnValue('');
    (state.getCurrentAccountIDs as jest.Mock).mockReturnValue([]);
    (Chart as unknown as jest.Mock).mockClear();
  });

  test('renders stats, a monthly table, both breakdowns and a single-series chart', async () => {
    await loadTrends();

    const monthly = cells('trends-monthly-table');
    expect(monthly).toHaveLength(6);
    expect(monthly[5]).toEqual([monthLabel(latest), '$400', '80.0%']);
    expect(monthly[4]).toEqual([monthLabel(previous), '$400', '70.0%']);
    expect(monthly[0]).toEqual([monthLabel(keys[0]!), '\u2014', 'N/A']);

    expect(cells('trends-provider-table')).toEqual([
      ['AWS', 'ec2', '$350', '75.0%'],
      ['GCP', 'compute', '$100', 'N/A'],
    ]);
    expect(cells('trends-service-table')).toEqual([['ec2', 'us-east-1', '$350', '75.0%']]);

    const stats = Array.from(content().querySelectorAll('.stat-value')).map((e) => e.textContent);
    expect(stats).toEqual(['$400/mo', '$400/mo', '80.0%']);

    expect(Chart).toHaveBeenCalledTimes(1);
    const config = (Chart as unknown as jest.Mock).mock.calls[0][1];
    expect(config.data.datasets).toHaveLength(1);
    expect(config.data.datasets[0].data).toEqual([null, null, null, null, 400, 400]);
  });

  test('forwards every selected account and the window, never the provider when unset', async () => {
    (state.getCurrentAccountIDs as jest.Mock).mockReturnValue(['acct-uuid-1', 'acct-uuid-2']);
    await loadTrends();
    const arg = (api.getAnalyticsTrends as jest.Mock).mock.calls[0][0];
    expect(arg.accountIDs).toEqual(['acct-uuid-1', 'acct-uuid-2']);
    expect(arg.provider).toBeUndefined();
    expect(arg.start).toBe(trendsWindow(6).start.toISOString());
  });

  test('provider chip narrows the monthly series and by-provider table too', async () => {
    (state.getCurrentProvider as jest.Mock).mockReturnValue('gcp');
    await loadTrends();
    expect((api.getAnalyticsTrends as jest.Mock).mock.calls[0][0].provider).toBe('gcp');
    expect(cells('trends-provider-table')).toEqual([['GCP', 'compute', '$100', 'N/A']]);
    expect(cells('trends-monthly-table')[4]).toEqual([monthLabel(previous), '$100', 'N/A']);
    expect(cells('trends-monthly-table')[5]).toEqual([monthLabel(latest), '\u2014', 'N/A']);
  });

  test('period select drives the window length', async () => {
    (document.getElementById('trends-period') as HTMLSelectElement).value = '12';
    await loadTrends();
    expect(cells('trends-monthly-table')).toHaveLength(12);
  });

  test('empty response shows an explanatory empty state, scoped when filtered', async () => {
    (api.getAnalyticsTrends as jest.Mock).mockResolvedValue(response({ monthly: [], by_provider: null, by_service: null }));
    await loadTrends();
    expect(content().querySelector('p.empty')?.textContent).toMatch(/No savings snapshots yet/);
    expect(Chart).not.toHaveBeenCalled();

    (state.getCurrentAccountIDs as jest.Mock).mockReturnValue(['acct-uuid-1']);
    await loadTrends();
    expect(content().querySelector('p.empty')?.textContent).toMatch(/selected account\/provider/);
  });

  test('API fields render as text, never markup', async () => {
    const evil = '<img src=x onerror="window.__pwned=1">';
    (api.getAnalyticsTrends as jest.Mock).mockResolvedValue(response({
      by_service: [{ service: evil, region: evil, total_savings: 1 }],
      by_provider: [{ provider: evil, service: evil, total_savings: 1 }],
    }));
    await loadTrends();
    expect(content().querySelector('img')).toBeNull();
    expect(cells('trends-service-table')[0]?.[0]).toBe(evil);
  });

  test('errors render inline with actionable copy', async () => {
    const err = (status: number, message: string): ApiError => Object.assign(new Error(message), { status });
    (api.getAnalyticsTrends as jest.Mock).mockRejectedValue(err(503, 'analytics snapshots not configured'));
    await loadTrends();
    expect(content().querySelector('p.error')?.textContent).toMatch(/not available on this deployment/);

    expect(trendsErrorMessage(err(400, 'account_id is required for scoped users')))
      .toBe('Select an account in the top bar to see its trends.');
    expect(trendsErrorMessage(err(500, 'boom'))).toBe('Failed to load trends: boom');
  });

  test('a stale response does not overwrite a newer one', async () => {
    let resolveFirst!: (v: AnalyticsTrendsResponse) => void;
    (api.getAnalyticsTrends as jest.Mock)
      .mockImplementationOnce(() => new Promise((r) => { resolveFirst = r; }))
      .mockResolvedValueOnce(response({ by_service: [{ service: 'newer', region: 'r', total_savings: 1 }] }));
    const first = loadTrends();
    await loadTrends();
    resolveFirst(response({ by_service: [{ service: 'stale', region: 'r', total_savings: 1 }] }));
    await first;
    expect(cells('trends-service-table')[0]?.[0]).toBe('newer');
  });

  test('controls are wired once however often the view loads', async () => {
    await loadTrends();
    await loadTrends();
    await loadTrends();
    (api.getAnalyticsTrends as jest.Mock).mockClear();
    document.getElementById('trends-refresh-btn')!.click();
    document.getElementById('trends-period')!.dispatchEvent(new Event('change'));
    await new Promise((r) => setTimeout(r, 0));
    expect(api.getAnalyticsTrends).toHaveBeenCalledTimes(2);
    expect(mockChartInstance.destroy).toHaveBeenCalled();
  });
});
