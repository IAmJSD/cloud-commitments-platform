/**
 * Shared chart utility helpers used by multiple chart modules.
 */

import { Chart, type ChartConfiguration } from 'chart.js';

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

/**
 * Create a chart, or update an existing one in place (issue #747).
 *
 * Destroying and recreating a chart on every filter or range change replays
 * Chart.js's grow-from-zero entry animation each time. When `existing` is
 * still drawing on `canvas`, swap in the new data and options and redraw
 * without animation; otherwise (first render, or the canvas was replaced)
 * destroy any stale instance and create a fresh one.
 */
export function upsertChart(existing: Chart | null, canvas: HTMLCanvasElement, config: ChartConfiguration): Chart {
    if (existing && existing.canvas === canvas) {
        existing.data = config.data;
        existing.options = config.options ?? {};
        existing.update('none');
        return existing;
    }
    existing?.destroy();
    return new Chart(canvas, config);
}
