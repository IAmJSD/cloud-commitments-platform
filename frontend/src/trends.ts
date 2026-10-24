/**
 * Inventory & Coverage -> Trends sub-tab (issue #30).
 *
 * Renders the savings-snapshot history from GET /api/analytics/trends: the
 * monthly savings run-rate and coverage over the chosen window, plus the
 * by-provider and by-service breakdowns. Scoped by the top-bar chips: the
 * account chip is forwarded as account_id (the backend enforces the
 * caller's allowed_accounts), the provider chip narrows every block.
 *
 * Everything is built with DOM methods and textContent, never innerHTML, so
 * no API field can inject markup. Listeners are bound once per element
 * (dataset marker), never re-added on a repeated render.
 */

import { Chart, registerables } from 'chart.js';
import * as api from './api';
import type { AnalyticsTrendsResponse, TrendsProviderRow, TrendsServiceRow } from './api';
import type { ApiError } from './api/types';
import * as state from './state';
import { formatCurrency } from './utils';
import { showSkeletonRows, teardownSkeleton } from './lib/skeleton';
import {
  DEFAULT_TRENDS_PERIOD_MONTHS,
  isTrendsPeriodMonths,
  trendsWindow,
  monthKeysBetween,
  filterByProvider,
  buildMonthlySeries,
  latestWithSavings,
  meanOrNull,
  formatCoverage,
  type TrendsMonthPoint,
} from './lib/trends-data';

Chart.register(...registerables);

const CONTENT_ID = 'trends-content';
const PERIOD_ID = 'trends-period';
const REFRESH_ID = 'trends-refresh-btn';
const CHART_ID = 'trends-savings-chart';

const PROVIDER_LABELS: Record<string, string> = { aws: 'AWS', azure: 'Azure', gcp: 'GCP' };

// Typed as the bar chart it is: assigning Chart<'bar'> to the default
// Chart<keyof ChartTypeRegistry> trips a Chart.js variance check whose
// result depends on ts-loader's check order (it broke the webpack build
// once chart-utils gained upsertChart, #747).
let trendsChart: Chart<'bar', (number | null)[], string> | null = null;
// Each load takes a ticket; a response that lands after a newer request was
// issued is dropped so a slow reply can't overwrite the current scope.
let loadSeq = 0;

function selectedPeriodMonths(): number {
  const el = document.getElementById(PERIOD_ID) as HTMLSelectElement | null;
  const n = Number(el?.value);
  return isTrendsPeriodMonths(n) ? n : DEFAULT_TRENDS_PERIOD_MONTHS;
}

function wireControls(): void {
  const period = document.getElementById(PERIOD_ID);
  if (period && period.dataset['wired'] !== '1') {
    period.addEventListener('change', () => { void loadTrends(); });
    period.dataset['wired'] = '1';
  }
  const refresh = document.getElementById(REFRESH_ID);
  if (refresh && refresh.dataset['wired'] !== '1') {
    refresh.addEventListener('click', () => { void loadTrends(); });
    refresh.dataset['wired'] = '1';
  }
}

/**
 * Fetch and render the Trends sub-tab. Idempotent: safe on every sub-tab
 * switch, period change, refresh click and chip change.
 */
export async function loadTrends(): Promise<void> {
  const container = document.getElementById(CONTENT_ID);
  if (!container) return;
  wireControls();

  const provider = state.getCurrentProvider();
  const accountIDs = state.getCurrentAccountIDs();
  // The account chip is single-select; forward only when exactly one is set.
  const accountID = accountIDs.length === 1 ? accountIDs[0] : undefined;
  const { start, end } = trendsWindow(selectedPeriodMonths());

  const seq = ++loadSeq;
  destroyChart();
  showSkeletonRows(container, 4, 1);

  try {
    const data = await api.getAnalyticsTrends({
      accountID,
      provider: provider || undefined,
      start: start.toISOString(),
      end: end.toISOString(),
    });
    if (seq !== loadSeq) return;
    teardownSkeleton(container);
    renderTrends(container, data, provider, monthKeysBetween(start, end), Boolean(provider || accountID));
  } catch (error) {
    if (seq !== loadSeq) return;
    teardownSkeleton(container);
    renderMessage(container, 'error', trendsErrorMessage(error as ApiError));
  }
}

/** Map the endpoint's known refusals to copy the user can act on. */
export function trendsErrorMessage(err: ApiError): string {
  if (err.status === 503) {
    return 'Trends are not available on this deployment: savings snapshots are not being collected.';
  }
  if (err.status === 400 && /account_id is required/i.test(err.message)) {
    return 'Select an account in the top bar to see its trends.';
  }
  return `Failed to load trends: ${err.message}`;
}

function destroyChart(): void {
  if (trendsChart) {
    trendsChart.destroy();
    trendsChart = null;
  }
}

function renderMessage(container: HTMLElement, cls: 'error' | 'empty', text: string): void {
  const p = document.createElement('p');
  p.className = cls;
  p.textContent = text;
  container.replaceChildren(p);
}

function renderTrends(
  container: HTMLElement,
  data: AnalyticsTrendsResponse,
  provider: string,
  keys: string[],
  filtered: boolean,
): void {
  const series = buildMonthlySeries(filterByProvider(data.monthly ?? [], provider), keys);
  const byProvider = filterByProvider(data.by_provider ?? [], provider);
  const byService = data.by_service ?? [];

  if (series.every((p) => p.savings === null) && byProvider.length === 0 && byService.length === 0) {
    renderMessage(container, 'empty', filtered
      ? 'No savings snapshots for the selected account/provider in this period.'
      : 'No savings snapshots yet. They are collected automatically once you have active commitments.');
    return;
  }

  const overview = card('Monthly savings and coverage');
  overview.appendChild(buildStats(series));
  const chartBox = document.createElement('div');
  chartBox.className = 'chart-container';
  const canvas = document.createElement('canvas');
  canvas.id = CHART_ID;
  canvas.setAttribute('role', 'img');
  canvas.setAttribute('aria-label', 'Bar chart of monthly savings run-rate; the table below lists the same figures');
  chartBox.appendChild(canvas);
  overview.appendChild(chartBox);
  overview.appendChild(buildTable(
    'trends-monthly-table',
    ['Month', 'Savings /mo', 'Coverage'],
    series.map((p) => [p.label, formatCurrency(p.savings), formatCoverage(p.coverage)]),
  ));

  const providerCard = card('By provider');
  providerCard.appendChild(byProvider.length > 0
    ? buildTable('trends-provider-table', ['Provider', 'Service', 'Avg savings /mo', 'Avg coverage'],
      byProvider.map((r: TrendsProviderRow) => [
        PROVIDER_LABELS[r.provider] ?? r.provider, r.service, formatCurrency(r.total_savings), formatCoverage(r.avg_coverage),
      ]))
    : emptyNote('No provider data in this period.'));

  const serviceCard = card('By service');
  serviceCard.appendChild(byService.length > 0
    ? buildTable('trends-service-table', ['Service', 'Region', 'Avg savings /mo', 'Avg coverage'],
      byService.map((r: TrendsServiceRow) => [
        r.service, r.region, formatCurrency(r.total_savings), formatCoverage(r.avg_coverage),
      ]))
    : emptyNote('No service data in this period.'));

  container.replaceChildren(overview, providerCard, serviceCard);
  renderChart(canvas, series);
}

function card(title: string): HTMLElement {
  const section = document.createElement('section');
  section.className = 'card';
  const h3 = document.createElement('h3');
  h3.textContent = title;
  section.appendChild(h3);
  return section;
}

function emptyNote(text: string): HTMLElement {
  const p = document.createElement('p');
  p.className = 'empty';
  p.textContent = text;
  return p;
}

function statCard(label: string, value: string, context: string): HTMLElement {
  const div = document.createElement('div');
  div.className = 'stat-card';
  const h4 = document.createElement('h4');
  h4.textContent = label;
  const v = document.createElement('p');
  v.className = 'stat-value';
  v.textContent = value;
  const c = document.createElement('p');
  c.className = 'stat-unit-context';
  c.textContent = context;
  div.append(h4, v, c);
  return div;
}

function buildStats(series: TrendsMonthPoint[]): HTMLElement {
  const grid = document.createElement('div');
  grid.className = 'stats-grid';
  const latest = latestWithSavings(series);
  const withData = series.filter((p) => p.savings !== null);
  const avg = meanOrNull(withData.map((p) => p.savings));
  const latestCoverage = [...series].reverse().find((p) => p.coverage !== null) ?? null;
  grid.append(
    statCard('Latest savings', latest ? `${formatCurrency(latest.savings)}/mo` : '--',
      latest ? `run-rate in ${latest.label}` : 'no snapshots in this period'),
    statCard('Average savings', avg !== null ? `${formatCurrency(avg)}/mo` : '--',
      `over ${withData.length} of ${series.length} months with data`),
    statCard('Coverage', formatCoverage(latestCoverage?.coverage),
      latestCoverage ? `in ${latestCoverage.label}` : 'not collected for this period'),
  );
  return grid;
}

function buildTable(id: string, headers: string[], rows: string[][]): HTMLElement {
  const wrap = document.createElement('div');
  wrap.className = 'trends-table-wrap';
  const table = document.createElement('table');
  table.id = id;
  const thead = document.createElement('thead');
  const hr = document.createElement('tr');
  for (const h of headers) {
    const th = document.createElement('th');
    th.scope = 'col';
    th.textContent = h;
    hr.appendChild(th);
  }
  thead.appendChild(hr);
  const tbody = document.createElement('tbody');
  for (const row of rows) {
    const tr = document.createElement('tr');
    for (const cell of row) {
      const td = document.createElement('td');
      td.textContent = cell;
      tr.appendChild(td);
    }
    tbody.appendChild(tr);
  }
  table.append(thead, tbody);
  wrap.appendChild(table);
  return wrap;
}

/**
 * One series on one axis: the monthly savings run-rate as bars. A month with
 * no snapshot is a gap (null), not a $0 bar. Coverage is a percentage on a
 * different scale, so it stays in the table and stat tile rather than on a
 * second y-axis.
 */
function renderChart(canvas: HTMLCanvasElement, series: TrendsMonthPoint[]): void {
  destroyChart();
  trendsChart = new Chart(canvas, {
    type: 'bar',
    data: {
      labels: series.map((p) => p.label),
      datasets: [{
        label: 'Savings /mo',
        data: series.map((p) => p.savings),
        backgroundColor: '#34a853',
        borderRadius: 4,
        maxBarThickness: 48,
      }],
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      plugins: {
        legend: { display: false },
        tooltip: {
          callbacks: {
            label: (ctx) => `Savings: ${formatCurrency(ctx.parsed.y, '$', 2)}/mo`,
          },
        },
      },
      scales: {
        x: { grid: { display: false } },
        y: {
          beginAtZero: true,
          grid: { color: 'rgba(0, 0, 0, 0.05)' },
          ticks: { maxTicksLimit: 6, callback: (v) => formatCurrency(Number(v)) },
        },
      },
    },
  });
}
