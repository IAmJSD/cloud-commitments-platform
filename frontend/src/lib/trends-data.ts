/**
 * Pure data shaping for the Trends view (issue #30). Kept free of DOM and
 * Chart.js so the month bucketing, provider narrowing and null handling are
 * unit-testable on their own.
 */

import type { TrendsMonthlyRow } from '../api/analytics';

/** Window lengths the Trends period picker offers, in calendar months. */
export const TRENDS_PERIOD_MONTHS = [3, 6, 12] as const;
export type TrendsPeriodMonths = typeof TRENDS_PERIOD_MONTHS[number];
export const DEFAULT_TRENDS_PERIOD_MONTHS: TrendsPeriodMonths = 6;

export function isTrendsPeriodMonths(n: number): n is TrendsPeriodMonths {
  return (TRENDS_PERIOD_MONTHS as readonly number[]).includes(n);
}

/**
 * One calendar month of the series. savings and coverage are null when the
 * month has no snapshot (or no coverage figure): a gap, never a fabricated 0.
 */
export interface TrendsMonthPoint {
  /** "YYYY-MM" */
  key: string;
  /** "Sep 2026" */
  label: string;
  savings: number | null;
  coverage: number | null;
}

/**
 * The [start, end] window for the last `months` calendar months, inclusive
 * of the current one: start is the first instant of the oldest month (UTC),
 * end is `now`. The backend's monthly query counts months between the two,
 * so starting on the 1st keeps it from dropping or adding a bucket.
 */
export function trendsWindow(months: number, now: Date = new Date()): { start: Date; end: Date } {
  const start = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - (months - 1), 1));
  return { start, end: now };
}

/** Every "YYYY-MM" key from start's month to end's month, oldest first (UTC). */
export function monthKeysBetween(start: Date, end: Date): string[] {
  const keys: string[] = [];
  const cursor = new Date(Date.UTC(start.getUTCFullYear(), start.getUTCMonth(), 1));
  const last = Date.UTC(end.getUTCFullYear(), end.getUTCMonth(), 1);
  while (cursor.getTime() <= last) {
    keys.push(`${cursor.getUTCFullYear()}-${String(cursor.getUTCMonth() + 1).padStart(2, '0')}`);
    cursor.setUTCMonth(cursor.getUTCMonth() + 1);
  }
  return keys;
}

/**
 * The "YYYY-MM" bucket a monthly row belongs to. The backend sends the
 * DATE_TRUNC('month') timestamp; reading the date part as written keeps the
 * server's month even when the timestamp carries a UTC offset, which a
 * Date round-trip in the browser's zone could shift into the previous month.
 */
export function monthKeyOf(month: string): string | null {
  const m = /^(\d{4})-(\d{2})/.exec(month);
  return m ? `${m[1]}-${m[2]}` : null;
}

/** "2026-09" -> "Sep 2026" */
export function monthLabel(key: string): string {
  const [y, m] = key.split('-').map(Number);
  return new Date(Date.UTC(y!, m! - 1, 1)).toLocaleDateString('en-US', {
    month: 'short',
    year: 'numeric',
    timeZone: 'UTC',
  });
}

/**
 * Keep only the rows for `provider`. The endpoint applies the provider chip
 * to by_service only, so the monthly series and by_provider table are
 * narrowed here to keep the whole view on the same scope. An empty provider
 * keeps everything.
 */
export function filterByProvider<T extends { provider: string }>(rows: readonly T[], provider: string): T[] {
  if (!provider) return [...rows];
  return rows.filter((r) => r.provider === provider);
}

/** Mean of the finite values, or null when there are none. */
export function meanOrNull(values: ReadonlyArray<number | null | undefined>): number | null {
  const present = values.filter((v): v is number => typeof v === 'number' && Number.isFinite(v));
  if (present.length === 0) return null;
  return present.reduce((s, v) => s + v, 0) / present.length;
}

/**
 * Collapse the (month, account, provider, service) rows into one point per
 * calendar month of `keys`. Savings run-rates add up across buckets; coverage
 * percentages are averaged over the buckets that reported one. Rows outside
 * the window are dropped.
 */
export function buildMonthlySeries(rows: readonly TrendsMonthlyRow[], keys: readonly string[]): TrendsMonthPoint[] {
  const savings = new Map<string, number>();
  const coverage = new Map<string, number[]>();
  for (const row of rows) {
    const key = monthKeyOf(row.month);
    if (!key || !keys.includes(key)) continue;
    if (Number.isFinite(row.total_savings)) {
      savings.set(key, (savings.get(key) ?? 0) + row.total_savings);
    }
    if (typeof row.avg_coverage === 'number' && Number.isFinite(row.avg_coverage)) {
      const list = coverage.get(key) ?? [];
      list.push(row.avg_coverage);
      coverage.set(key, list);
    }
  }
  return keys.map((key) => ({
    key,
    label: monthLabel(key),
    savings: savings.has(key) ? savings.get(key)! : null,
    coverage: meanOrNull(coverage.get(key) ?? []),
  }));
}

/** The most recent point that has savings data, or null for an empty series. */
export function latestWithSavings(series: readonly TrendsMonthPoint[]): TrendsMonthPoint | null {
  for (let i = series.length - 1; i >= 0; i--) {
    if (series[i]!.savings !== null) return series[i]!;
  }
  return null;
}

/** "54.2%" or "N/A" for an absent figure (never "0%"). */
export function formatCoverage(value: number | null | undefined): string {
  return typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(1)}%` : 'N/A';
}
