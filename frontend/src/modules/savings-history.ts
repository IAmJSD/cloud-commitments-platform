/**
 * Savings History module - displays historical savings chart
 */

import { Chart, registerables } from 'chart.js';
import { getSavingsAnalytics, type SavingsAnalyticsResponse, type SavingsDataPoint } from '../api';
import * as state from '../state';
import { formatTrendAxisTick, upsertChart } from './chart-utils';
import { formatCompactCurrency, formatCurrency } from '../utils';
import { setChartSummary, renderChartTable, removeChartTable } from '../lib/chart-a11y';
import { applyChartDefaults, chartColor, withAlpha } from '../lib/chart-theme';

// Register Chart.js components
Chart.register(...registerables);

// Chart instance
let savingsChart: Chart | null = null;
// Id of the chart's "View as table" text alternative (#744).
const SAVINGS_HISTORY_TABLE_ID = 'savings-history-table';

// Canonical unit the API returns for per-bucket and summary values.
// estimated_savings in purchase_history is the monthly savings figure
// (it sits alongside monthly_cost in the schema), so all totals summed
// from that column are also monthly.
const API_UNIT = 'monthly' as const;

export type SavingsUnit = 'hourly' | 'monthly' | 'yearly';

// Conversion factors relative to monthly as the canonical unit.
const HOURS_PER_MONTH = 730;   // 365.25 * 24 / 12
const MONTHS_PER_YEAR = 12;

/**
 * Convert a monthly savings value to the chosen display unit.
 * The API always returns monthly values; this function converts for display only.
 */
export function convertFromMonthly(monthlyValue: number, unit: SavingsUnit): number {
    switch (unit) {
        case 'hourly':  return monthlyValue / HOURS_PER_MONTH;
        case 'monthly': return monthlyValue;
        case 'yearly':  return monthlyValue * MONTHS_PER_YEAR;
    }
}

/**
 * Return the short suffix string for the given unit (e.g. "/hr").
 */
export function unitSuffix(unit: SavingsUnit): string {
    switch (unit) {
        case 'hourly':  return '/hr';
        case 'monthly': return '/mo';
        case 'yearly':  return '/yr';
    }
}

/**
 * Return the adjective for use in stat-card headings (e.g. "Hourly").
 */
export function unitLabel(unit: SavingsUnit): string {
    switch (unit) {
        case 'hourly':  return 'Hourly';
        case 'monthly': return 'Monthly';
        case 'yearly':  return 'Yearly';
    }
}

/**
 * Read the current value of the #savings-unit dropdown.
 * Falls back to the API's canonical unit when the element is absent (e.g. in tests
 * that don't include the dropdown in their DOM fixture).
 */
function getSelectedUnit(): SavingsUnit {
    const el = document.getElementById('savings-unit') as HTMLSelectElement | null;
    const val = el?.value ?? API_UNIT;
    if (val === 'hourly' || val === 'monthly' || val === 'yearly') return val;
    return API_UNIT;
}

/**
 * Which series the chart plots (issue #749). Period savings and the running
 * total have different scales and units, so they are shown one at a time on
 * a single axis instead of on two axes whose line crossings mean nothing.
 */
export type SavingsSeries = 'period' | 'cumulative';

function getSelectedSeries(): SavingsSeries {
    const el = document.getElementById('savings-series') as HTMLSelectElement | null;
    return el?.value === 'cumulative' ? 'cumulative' : 'period';
}

/**
 * Load savings history data based on selected period
 */
export async function loadSavingsHistory(): Promise<void> {
    const periodSelect = document.getElementById('savings-period') as HTMLSelectElement;
    const chartContainer = document.getElementById('savings-history-chart')?.parentElement;
    const emptyEl = document.getElementById('savings-history-empty');
    const statsEl = document.getElementById('savings-stats');

    if (!periodSelect) return;

    const period = periodSelect.value;
    const { start, end, interval } = getPeriodDates(period);

    // Honour the global topbar filter chips (issue #503). The account chip
    // is single-select, and the backend's /history/analytics takes a single
    // account_id (see handler_analytics.go), so we forward the only selected
    // ID, mirroring dashboard.ts loadSavingsTrendChart. The provider chip is
    // forwarded too and the backend now applies it as a provider WHERE filter
    // on the savings-history query (issue #498, QA 2.3).
    const currentProvider = state.getCurrentProvider();
    const currentAccountIDs = state.getCurrentAccountIDs();

    // Build a human-readable description of the active filter so the
    // empty-state message distinguishes "no data yet" from "nothing in
    // the selected scope". Used when the API returns 0 data points.
    const filterDesc = buildFilterDesc(currentProvider, currentAccountIDs);

    try {
        const data = await getSavingsAnalytics({
            start: start.toISOString(),
            end: end.toISOString(),
            interval,
            ...(currentProvider ? { provider: currentProvider } : {}),
            ...(currentAccountIDs.length === 1 ? { account_ids: currentAccountIDs } : {}),
        });

        if (!data.data_points || data.data_points.length === 0) {
            showEmptyState(chartContainer, emptyEl, statsEl, filterDesc);
            return;
        }

        // Show chart, hide empty state
        if (chartContainer) chartContainer.classList.remove('hidden');
        if (emptyEl) emptyEl.classList.add('hidden');
        if (statsEl) statsEl.classList.remove('hidden');

        renderSavingsStats(data, start, end, interval);
        renderSavingsChart(data.data_points, interval, getSelectedUnit(), start, end, getSelectedSeries());
    } catch (error) {
        const msg = error instanceof Error ? error.message : 'Unknown error';
        console.error('Failed to load savings history:', msg);
        showErrorState(chartContainer, emptyEl, statsEl, msg);
    }
}

/**
 * Show empty state when no data is available.
 *
 * filterDesc is a short human-readable description of the active
 * topbar filter (e.g. "AWS" or "account abc-123"). When non-empty
 * the empty-state copy says "no data for the selected filter" so
 * the user understands the chart is hidden because of scoping, not
 * because no purchases exist at all (issue #701).
 */
function showEmptyState(
    chartContainer: HTMLElement | null | undefined,
    emptyEl: HTMLElement | null,
    statsEl: HTMLElement | null,
    filterDesc: string = '',
): void {
    if (chartContainer) chartContainer.classList.add('hidden');
    removeChartTable(SAVINGS_HISTORY_TABLE_ID);
    if (emptyEl) {
        emptyEl.classList.remove('hidden');
        // Update the copy inside the empty-state element so the message
        // reflects whether a filter is active. The element is always
        // present in the DOM (hidden by CSS class, not removed), so we
        // can safely write innerHTML here — this is the only place that
        // mutates it and the content is built from trusted constants.
        const heading = emptyEl.querySelector('p:first-child');
        const help = emptyEl.querySelector('p.help-text');
        if (heading) {
            heading.textContent = filterDesc
                ? `No savings data for the selected filter (${filterDesc}).`
                : 'No savings history data available yet.';
        }
        if (help) {
            help.textContent = filterDesc
                ? 'Try broadening the filter or selecting a different period.'
                : 'Data will be collected hourly once you have active purchases.';
        }
    }
    if (statsEl) statsEl.classList.add('hidden');

    // Clear any existing chart
    if (savingsChart) {
        savingsChart.destroy();
        savingsChart = null;
    }
}

/**
 * Show an explicit error state when the API call fails. Reuses the empty-state
 * DOM element but sets copy that makes clear this is a fetch failure, not
 * simply a lack of data (issue #701 CR finding).
 */
function showErrorState(
    chartContainer: HTMLElement | null | undefined,
    emptyEl: HTMLElement | null,
    statsEl: HTMLElement | null,
    message: string,
): void {
    if (chartContainer) chartContainer.classList.add('hidden');
    removeChartTable(SAVINGS_HISTORY_TABLE_ID);
    if (statsEl) statsEl.classList.add('hidden');
    if (emptyEl) {
        emptyEl.classList.remove('hidden');
        const heading = emptyEl.querySelector('p:first-child');
        const help = emptyEl.querySelector('p.help-text');
        if (heading) heading.textContent = 'Failed to load savings history.';
        if (help) help.textContent = `Please retry. (${message})`;
    }
    if (savingsChart) {
        savingsChart.destroy();
        savingsChart = null;
    }
}

/**
 * Build a short human-readable description of the active topbar filter
 * for use in the Savings History empty-state message. Returns '' when
 * no filter is active (no chip selected) so callers can distinguish
 * "unfiltered empty" from "filtered empty".
 */
function buildFilterDesc(provider: string, accountIDs: readonly string[]): string {
    const parts: string[] = [];
    if (provider && provider.toLowerCase() !== 'all') parts.push(provider.toUpperCase());
    if (accountIDs.length > 0) parts.push(accountIDs[0] ?? '');
    return parts.join(', ');
}

/**
 * Get start/end dates and interval based on period selection
 */
function getPeriodDates(period: string): { start: Date; end: Date; interval: 'hourly' | 'daily' } {
    const end = new Date();
    const start = new Date(end);
    let interval: 'hourly' | 'daily' = 'hourly';

    switch (period) {
        case '24h':
            start.setHours(start.getHours() - 24);
            interval = 'hourly';
            break;
        case '7d':
            start.setDate(start.getDate() - 7);
            interval = 'hourly';
            break;
        case '30d':
            start.setDate(start.getDate() - 30);
            interval = 'daily';
            break;
        case '90d':
            start.setDate(start.getDate() - 90);
            interval = 'daily';
            break;
        default:
            start.setDate(start.getDate() - 7);
            interval = 'hourly';
    }

    return { start, end, interval };
}

/**
 * Render savings statistics
 */
function renderSavingsStats(data: SavingsAnalyticsResponse, start: Date, end: Date, interval: 'hourly' | 'daily'): void {
    const periodSavingsEl = document.getElementById('period-savings');
    // The unit indicator belongs below the value, not inside the label.
    // Period Savings is a cumulative dollar total over the selected date range,
    // not a per-unit rate -- the label stays plain ("Period Savings") and the
    // sub-line (#period-savings-unit) shows which view mode is active without
    // implying a rate.
    const periodSavingsLabelEl = document.getElementById('period-savings-label');
    const periodSavingsUnitEl = document.getElementById('period-savings-unit');
    const avgSavingsEl = document.getElementById('avg-hourly-savings');
    const peakSavingsEl = document.getElementById('peak-savings');
    const avgLabelEl = document.getElementById('avg-savings-label');

    const unit = getSelectedUnit();
    const suffix = unitSuffix(unit);
    const adjective = unitLabel(unit);

    const summary = data.summary;
    const dataPoints = data.data_points || [];

    // Calculate totals from data points when summary is absent.
    // Each data point's total_savings is the sum of estimated_savings
    // (a monthly figure) bucketed by the chosen interval.
    let totalSavings = 0;
    let peakSavings = 0;

    for (const dp of dataPoints) {
        // M-6: the type contract says total_savings is non-nullable, but a
        // future API change could introduce null; guard with ?? 0 only for the
        // aggregation where 0 is the correct absent interpretation (a missing
        // bucket contributes nothing to the total/peak -- not the same as a
        // fabricated $0 on a money display path).
        const savings = dp.total_savings ?? 0;
        totalSavings += savings;
        if (savings > peakSavings) {
            peakSavings = savings;
        }
    }

    // The server's window total (HistorySummary.total_monthly_savings) is
    // authoritative; the client-side bucket sum is only a fallback for a
    // missing summary. Average and peak have no wire counterpart (#276): the
    // average spreads the total over the whole requested window (empty
    // intervals included) and the peak comes from data_points. All monthly.
    const monthlyTotal = summary?.total_monthly_savings ?? totalSavings;
    const intervalMs = interval === 'hourly' ? 3600000 : 86400000;
    const periodCount = (end.getTime() - start.getTime()) / intervalMs;
    const monthlyAvg = monthlyTotal / periodCount;
    const monthlyPeak = peakSavings;

    // Convert to the user-chosen display unit.
    const displayTotal = convertFromMonthly(monthlyTotal, unit);
    const displayAvg = convertFromMonthly(monthlyAvg, unit);
    const displayPeak = convertFromMonthly(monthlyPeak, unit);

    if (periodSavingsEl) {
        // Period Savings is the cumulative total over the selected date range
        // (no per-unit rate suffix -- it is already a dollar total).
        periodSavingsEl.textContent = formatCurrency(displayTotal, '$', 2);
    }
    if (periodSavingsLabelEl) {
        periodSavingsLabelEl.textContent = 'Period Savings';
    }
    if (periodSavingsUnitEl) {
        periodSavingsUnitEl.textContent = `shown in ${adjective.toLowerCase()} equivalents`;
    }
    if (avgLabelEl) {
        avgLabelEl.textContent = `Avg ${adjective} Savings`;
    }
    if (avgSavingsEl) {
        avgSavingsEl.textContent = `${formatCurrency(displayAvg, '$', 2)}${suffix}`;
    }
    if (peakSavingsEl) {
        peakSavingsEl.textContent = `${formatCurrency(displayPeak, '$', 2)}${suffix}`;
    }
}

/**
 * Render savings chart using Chart.js.
 *
 * Issue #749: one series on one y-axis. Period savings are bars (each bar is
 * one interval's projected monthly additions, converted to the chosen unit);
 * the running total is a separate view picked with #savings-series. The old
 * chart drew both on two y-axes, so the points where the lines crossed had
 * no meaning. Ticks and tooltips use the shared formatters ("$1.5K" on the
 * axis, "$1,500.00" in the tooltip) instead of `$${x.toFixed(2)}`.
 *
 * BUG FIX (QA 2.2): the x-axis is `type:'linear'` anchored to the selected
 * period [start, end] so the leftmost tick is the period start, not the first
 * data point. Mirrors the approach used by the Home dashboard chart (PR #746).
 *
 * BUG FIX (QA 2.4/2.5): the y-axis has maxTicksLimit:6 to prevent tick
 * instability; the compact formatter keeps up to two fraction digits so
 * close float ticks stay distinct.
 */
function renderSavingsChart(
    dataPoints: SavingsDataPoint[],
    interval: string,
    unit: SavingsUnit = 'monthly',
    periodStart?: Date,
    periodEnd?: Date,
    series: SavingsSeries = 'period',
): void {
    const ctx = document.getElementById('savings-history-chart') as HTMLCanvasElement;

    if (!ctx) {
        console.error('Canvas element not found: savings-history-chart');
        return;
    }

    const cumulative = series === 'cumulative';
    // The running total is a dollar amount, not a rate, so it takes no
    // unit suffix (its unit conversion is tracked separately in #134).
    const suffix = cumulative ? '' : unitSuffix(unit);

    // Determine the interval hint for the x-axis tick formatter.
    // The interval parameter can be 'hourly', 'daily', 'weekly', or 'monthly';
    // formatTrendAxisTick accepts 'hourly' | 'daily' | 'weekly' -- treat
    // 'monthly' the same as 'daily' (date-only label).
    const tickIntervalHint: 'hourly' | 'daily' | 'weekly' =
        interval === 'hourly' ? 'hourly' : interval === 'weekly' ? 'weekly' : 'daily';

    // Map each data point to {x: timestamp_ms, y: value} so Chart.js positions
    // each point at its real date on the linear axis instead of by label index.
    // M-6: use ?? 0 (not || 0) to treat a genuine 0-saving bucket correctly.
    const points = dataPoints.map(dp => ({
        x: new Date(dp.timestamp).getTime(),
        y: cumulative ? dp.cumulative_savings ?? 0 : convertFromMonthly(dp.total_savings ?? 0, unit),
    }));

    // Axis bounds: anchor to the selected period so the leftmost tick is the
    // period start, not the first data point (QA 2.2).
    const nowMs = Date.now();
    const axisMinMs = periodStart ? periodStart.getTime() : nowMs - 7 * 86400_000;
    const axisMaxMs = periodEnd ? periodEnd.getTime() : nowMs;

    const label = cumulative ? 'Cumulative Savings' : 'Period Savings';
    const periodColor = chartColor('--cudly-chart-2');
    const cumulativeColor = chartColor('--cudly-chart-cumulative');
    const dataset = cumulative
        ? {
            type: 'line' as const,
            label,
            data: points,
            borderColor: cumulativeColor,
            backgroundColor: withAlpha(cumulativeColor, 0.1),
            fill: true,
            tension: 0.3,
            pointRadius: dataPoints.length > 50 ? 0 : 3,
            pointHoverRadius: 5,
        }
        : {
            type: 'bar' as const,
            label,
            data: points,
            backgroundColor: withAlpha(periodColor, 0.7),
            borderColor: periodColor,
            borderWidth: 1,
            maxBarThickness: 24,
        };

    // #744: a value summary for the role="img" canvas plus every point as a
    // table. The table goes after the sizing container, not inside it.
    // Merged with #749: the summary names the series currently drawn, and the
    // table keeps both columns (period in the chosen unit, running total)
    // whichever series the chart shows, so no value is hidden by the toggle.
    const latest = dataPoints[dataPoints.length - 1];
    setChartSummary(ctx,
        `${cumulative ? 'Line chart of cumulative savings' : 'Bar chart of savings per period'}: `
        + `${dataPoints.length} data point${dataPoints.length === 1 ? '' : 's'}, `
        + `cumulative savings ${formatCurrency(latest?.cumulative_savings ?? 0, '$', 2)} at the latest point. `
        + 'Values are in the table below the chart.');
    renderChartTable(ctx.parentElement ?? ctx, SAVINGS_HISTORY_TABLE_ID, {
        caption: 'Savings history',
        columns: ['Date', `Period savings (${unitSuffix(unit)})`, 'Cumulative savings'],
        rows: dataPoints.map((dp) => [
            formatTrendAxisTick(new Date(dp.timestamp).getTime(), tickIntervalHint),
            formatCurrency(convertFromMonthly(dp.total_savings ?? 0, unit), '$', 2),
            formatCurrency(dp.cumulative_savings ?? 0, '$', 2),
        ]),
    });

    applyChartDefaults(Chart.defaults);
    // #747: update in place so a period or unit change doesn't replay the
    // entry animation. Switching the series (#749) changes the chart type,
    // which upsertChart handles by recreating the chart.
    savingsChart = upsertChart(savingsChart, ctx, {
        type: cumulative ? 'line' : 'bar',
        data: {
            datasets: [dataset],
        },
        options: {
            responsive: true,
            maintainAspectRatio: false,
            interaction: {
                intersect: false,
                mode: 'index',
            },
            scales: {
                x: {
                    type: 'linear',
                    display: true,
                    min: axisMinMs,
                    max: axisMaxMs,
                    // A bar chart pads the x-axis by default, which would
                    // push the period start off the axis edge (QA 2.2).
                    offset: false,
                    grid: {
                        display: false,
                    },
                    ticks: {
                        maxTicksLimit: 8,
                        maxRotation: 0,
                        callback: (value) => formatTrendAxisTick(value as number, tickIntervalHint),
                    },
                },
                y: {
                    type: 'linear',
                    display: true,
                    position: 'left',
                    beginAtZero: true,
                    grid: {
                        color: chartColor('--cudly-chart-grid-faint'),
                    },
                    ticks: {
                        maxTicksLimit: 6,
                        callback: function(value: number | string) {
                            const numValue = typeof value === 'string' ? parseFloat(value) : value;
                            return formatCompactCurrency(numValue);
                        },
                    },
                    title: {
                        display: true,
                        text: cumulative ? 'Cumulative savings' : `Savings per period (${unitLabel(unit)})`,
                    },
                },
            },
            plugins: {
                legend: {
                    display: false,
                },
                tooltip: {
                    callbacks: {
                        label: function(context) {
                            const raw = context.raw as ({ x: number; y: number } | number) | null | undefined;
                            const value = (raw != null && typeof raw === 'object' ? raw.y : raw as number) || 0;
                            return `${context.dataset.label}: ${formatCurrency(value, '$', 2)}${suffix}`;
                        },
                    },
                },
            },
        },
    });
}

/**
 * True when the Purchases tab is the currently-visible tab. The reload-on-
 * filter-change subscriptions below skip the fetch when this is false so we
 * don't burn an API call (and a skeleton flash) for a section the user isn't
 * looking at: `switchTab('purchases')` runs loadSavingsHistory() on next
 * entry anyway.
 */
function isPurchasesTabActive(): boolean {
    return document.getElementById('purchases-tab')?.classList.contains('active') === true;
}

/**
 * Initialize savings history event listeners (issue #503).
 *
 * Wires the period dropdown + refresh button, and subscribes to the global
 * topbar filter chips so a provider/account change re-queries this chart.
 * Previously only the local controls were wired, so changing the Account
 * chip did nothing until the Purchases tab was left and re-entered.
 *
 * Mirrors the recommendations.ts pattern from PR #488:
 *   - Active-tab guard: only fire loadSavingsHistory() when the Purchases
 *     tab is active.
 *   - Coalesce duplicate reloads via queueMicrotask: the provider-change
 *     handler in topbar-filters.ts updates BOTH state slots (clear accounts
 *     then set provider, per the #185 ordering rule), firing the account-
 *     AND provider-subscribers from one user action. Without coalescing we'd
 *     kick off two loadSavingsHistory() calls back-to-back: extra API load
 *     plus a stale-overwrite risk if the first response lands after the
 *     second.
 *   - Re-check active-tab inside the microtask: a tab switch between the
 *     chip change and the microtask flush cancels the now-unneeded fetch.
 */
export function initSavingsHistory(): void {
    const periodSelect = document.getElementById('savings-period');
    const refreshBtn = document.getElementById('refresh-savings-btn');
    const unitSelect = document.getElementById('savings-unit');

    if (periodSelect) {
        periodSelect.addEventListener('change', loadSavingsHistory);
    }

    if (refreshBtn) {
        refreshBtn.addEventListener('click', loadSavingsHistory);
    }

    // Series toggle (issue #749): same dedup pattern as the unit dropdown.
    const seriesSelect = document.getElementById('savings-series') as (HTMLSelectElement & { _seriesChangeHandler?: () => void }) | null;
    if (seriesSelect) {
        if (seriesSelect._seriesChangeHandler) {
            seriesSelect.removeEventListener('change', seriesSelect._seriesChangeHandler);
        }
        seriesSelect._seriesChangeHandler = (): void => { void loadSavingsHistory(); };
        seriesSelect.addEventListener('change', seriesSelect._seriesChangeHandler);
    }

    // Wire unit dropdown. Using a named handler stored as a property so that
    // repeated calls to initSavingsHistory() don't stack duplicate listeners
    // (feedback_event_listener_dedup pattern).
    if (unitSelect) {
        const prevHandler = (unitSelect as HTMLSelectElement & { _unitChangeHandler?: () => void })._unitChangeHandler;
        if (prevHandler) {
            unitSelect.removeEventListener('change', prevHandler);
        }
        const unitChangeHandler = (): void => { void loadSavingsHistory(); };
        (unitSelect as HTMLSelectElement & { _unitChangeHandler?: () => void })._unitChangeHandler = unitChangeHandler;
        unitSelect.addEventListener('change', unitChangeHandler);
    }

    let reloadQueued = false;
    const scheduleReload = (): void => {
        if (!isPurchasesTabActive() || reloadQueued) return;
        reloadQueued = true;
        queueMicrotask(() => {
            reloadQueued = false;
            if (isPurchasesTabActive()) void loadSavingsHistory();
        });
    };
    state.subscribeProvider(scheduleReload);
    state.subscribeAccount(scheduleReload);
}

// Export for use in other modules
export { savingsChart };
