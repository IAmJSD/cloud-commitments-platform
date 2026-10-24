/**
 * Shared chart utility helpers used by multiple chart modules.
 */

import { formatCurrency } from '../utils';

/**
 * Format a millisecond timestamp for a savings-trend x-axis tick label.
 * The intervalHint drives granularity: hourly shows date+time, everything else shows date only.
 * Exported for unit testing.
 */
export function formatTrendAxisTick(tsMs: number, intervalHint: 'hourly' | 'daily' | 'weekly'): string {
    const d = new Date(tsMs);
    if (intervalHint === 'hourly') {
        return d.toLocaleString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
    }
    return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
}

/**
 * Fewest data points the Home "Savings over time" chart needs before it draws
 * a line (issue #188). With a single point the chart is one dot on an
 * otherwise blank canvas, so the caller shows a sparse-data message instead.
 */
export const MIN_TREND_POINTS = 2;

/**
 * Sparse-data copy for the savings-trend chart (issue #188): says why there is
 * no chart yet and still surfaces the one value we do have. The date uses the
 * same en-US local formatting as the chart's axis, plus the year.
 */
export function formatSparseTrendMessage(point: { timestamp: string; cumulative_savings: number }): string {
    const when = new Date(point.timestamp).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
    const amount = formatCurrency(point.cumulative_savings || 0, '$', 2);
    return `Not enough history yet to show a trend. So far: ${amount} in cumulative savings as of ${when}.`;
}

/**
 * Format a millisecond timestamp for a savings-trend tooltip title (issue #53).
 * Unlike the compact axis tick, the tooltip has room for the year, which keeps
 * points a year apart (e.g. two "Jun 15"s on the 1-year range, or a 90-day
 * range that crosses New Year) distinguishable.
 */
export function formatTrendTooltipTitle(tsMs: number, intervalHint: 'hourly' | 'daily' | 'weekly'): string {
    const d = new Date(tsMs);
    if (intervalHint === 'hourly') {
        return d.toLocaleString('en-US', { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
    }
    return d.toLocaleDateString('en-US', { year: 'numeric', month: 'short', day: 'numeric' });
}
