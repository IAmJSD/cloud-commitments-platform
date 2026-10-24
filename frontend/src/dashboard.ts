/**
 * Dashboard module for CUDly
 */

import { Chart, registerables } from 'chart.js';
import * as api from './api';
import * as state from './state';
import { formatCurrency, getDateParts, NO_VALUE, providerBadgeClass } from './utils';
import type { DashboardSummary, UpcomingPurchase, ServiceSavings, LocalRecommendation } from './types';
import type { SavingsDataPoint } from './api';
import { showToast } from './toast';
import { confirmDialog } from './confirmDialog';
import { openModal, closeModal } from './modal';
import { canAccess } from './permissions';
import { groupRecsByCell, pageLevelRange, formatSavingsRange, triggerAutoRefreshIfStale, startRecommendationsRefresh } from './recommendations';
import { getRecommendationsFreshness } from './api/recommendations';
import { buildFirstRunEmptyState, isScopeFiltered } from './first-run-empty-state';
import { showSkeletonTiles, showSkeletonBlock, teardownSkeleton } from './lib/skeleton';
import { filterByEnabledProviders } from './lib/enabled-providers';
import { scopeUpcomingToProvider, upcomingEmptyMessage, UPCOMING_ACCOUNT_SCOPE_NOTE } from './lib/upcoming-scope';
import { formatTrendAxisTick, formatTrendTooltipTitle, formatSparseTrendMessage, MIN_TREND_POINTS } from './modules/chart-utils';
import { setChartSummary, renderChartTable, removeChartTable } from './lib/chart-a11y';
// Re-export for backward compatibility (tests and other callers import from dashboard.ts).
export { formatTrendAxisTick } from './modules/chart-utils';

// Register Chart.js components
Chart.register(...registerables);

// Separate Chart instance for the trend widget so it doesn't conflict with
// the per-service savings chart instance below.
let savingsTrendChart: Chart | null = null;
let savingsTrendRange: '7' | '30' | '90' | 'all' = '90';

// Chart instance for the per-service savings-range bar chart (issue #765).
let savingsByServiceChart: Chart | null = null;

// Maximum number of services to show in the bar chart before truncating.
const SAVINGS_BY_SERVICE_MAX = 10;

// #744: a 2px card-coloured gap on top of every stacked segment, so adjacent
// layers are separated by an edge as well as by their layer colour.
const SEGMENT_GAP = { borderColor: '#ffffff', borderWidth: { top: 2, right: 0, bottom: 0, left: 0 } } as const;

// Ids of the "View as table" text alternatives for the Home charts (#744).
const SAVINGS_BY_SERVICE_TABLE_ID = 'savings-by-service-table';
const SAVINGS_TREND_TABLE_ID = 'savings-trend-table';

// One colour per stacked layer, the same on every bar, so the legend swatches
// are exactly the colours drawn (#189). The x axis already names each service,
// so hue encodes the layer, not the service: no colour can change with rank
// (#700) and the layers differ by hue rather than by shades of one (#744).
// Slots 1-3 of the validated categorical palette, CVD-distinct in every pair.
// Upside (aqua) sits below 3:1 on white; the tooltip carries its value.
export const SAVINGS_LAYER_COLORS = {
  current: '#2a78d6',      // blue
  lowestOption: '#eb6834', // orange
  upside: '#1baf7a',       // aqua
} as const;

// In-memory index of the currently-rendered upcoming purchases, keyed by
// execution_id. The "View Details" affordance renders from this — the
// /api/dashboard/upcoming response already carries every field the
// dialog displays, so no extra roundtrip is needed. The Cancel button
// uses the same execution_id to call DELETE /api/purchases/planned/{id}
// so cancelling removes JUST this scheduled step (the plan template
// stays in place). Earlier iterations: PR #207 keyed by plan_id which
// caused the cancel-then-delete-the-whole-plan bug; PR #213 (this one)
// enumerates real pending executions to surface execution_id properly.
let upcomingPurchasesIndex: Map<string, UpcomingPurchase> = new Map();

/**
 * True when the Home tab is the currently-visible tab. Used by the
 * reload-on-filter-change subscriptions below to skip the fetch (and
 * the resulting skeleton flash) when the user is on another tab —
 * switchTab('home') calls loadDashboard() on entry anyway.
 * Mirrors the isPurchasesTabActive() guard in modules/savings-history.ts.
 */
function isHomeTabActive(): boolean {
  return document.getElementById('home-tab')?.classList.contains('active') === true;
}

/**
 * Build a short human-readable description of the active topbar filter
 * for use in empty-state messages on the Home charts. Returns '' when no
 * filter is active so callers can distinguish "unfiltered empty" from
 * "filtered empty". Mirrors buildTrendFilterDesc used in loadSavingsTrendChart.
 */
export function buildFilterDesc(provider: string, accountIDs: readonly string[]): string {
  const parts: string[] = [];
  if (provider && provider.toLowerCase() !== 'all') parts.push(provider.toUpperCase());
  if (accountIDs.length > 0) parts.push(accountIDs[0] ?? '');
  return parts.join(', ');
}

/**
 * Setup dashboard event handlers
 */
export function setupDashboardHandlers(): void {
  // Filter source-of-truth lives in state.ts (mutated by the global
  // topbar chips). Subscribe to filter changes and reload the dashboard;
  // the issue #185 ordering rule (clear accounts before refetching for a
  // new provider) is enforced by topbar-filters.ts at the source so the
  // dashboard's loadDashboard() always sees consistent state.
  //
  // Coalescing: the provider-change handler in topbar-filters.ts fires
  // BOTH the account subscriber (setCurrentAccountIDs([])) AND the
  // provider subscriber (setCurrentProvider(newProv)) synchronously.
  // Without coalescing, two loadDashboard() calls race back-to-back.
  // queueMicrotask defers the actual fetch to after the current call
  // stack clears, so the two fires collapse into one reload.
  // Active-tab guard: skip the fetch when the Home tab is not visible;
  // switchTab('home') triggers loadDashboard() on entry.
  // Mirrors the scheduleReload pattern in modules/savings-history.ts.
  let dashboardReloadQueued = false;
  const scheduleDashboardReload = (): void => {
    if (!isHomeTabActive() || dashboardReloadQueued) return;
    dashboardReloadQueued = true;
    queueMicrotask(() => {
      dashboardReloadQueued = false;
      if (isHomeTabActive()) void loadDashboard();
    });
  };
  state.subscribeProvider(scheduleDashboardReload);
  state.subscribeAccount(scheduleDashboardReload);

  setupSavingsTrendHandlers();
}

/**
 * Load dashboard data
 */
export async function loadDashboard(): Promise<void> {
  // Issue #344 T3: render skeletons synchronously before kicking off the
  // fetch so the panels show "loading" intent instead of staying blank.
  // The success render replaces children for a clean handoff; the catch
  // block calls teardownSkeleton before rendering the error.
  const summaryEl = document.getElementById('summary');
  if (summaryEl) showSkeletonTiles(summaryEl, 4);
  const upcomingEl = document.getElementById('upcoming-list');
  if (upcomingEl) showSkeletonBlock(upcomingEl, '100%', '6rem');

  try {
    const currentProvider = state.getCurrentProvider();
    const currentAccountIDs = state.getCurrentAccountIDs();

    // Fetch summary, upcoming, and recommendations concurrently.
    // Recommendations are fetched here (frontend-only approach) because
    // /api/dashboard/summary still returns a flat potential_monthly_savings
    // which overcounts by summing every variant of every cell (~6x inflation).
    // The recs endpoint is Postgres-cached and cheap; a future backend PR can
    // move the range computation server-side if needed.
    // Promise.allSettled ensures the dashboard still renders if any individual
    // fetch fails -- each card falls back gracefully.
    const [summaryResult, upcomingResult, recsResult, enabledProviders, accountsResult] = await Promise.allSettled([
      api.getDashboardSummary(currentProvider, currentAccountIDs),
      api.getUpcomingPurchases(),
      api.getRecommendations({ provider: currentProvider, account_ids: currentAccountIDs }),
      loadEnabledProviders(),
      // Only feeds the first-run onboarding card (issue #756); wrapped so a
      // missing or failing endpoint degrades to "unknown" instead of
      // breaking the dashboard.
      Promise.resolve().then(() => api.listAccountsMinimal()),
    ]);

    const summaryData = summaryResult.status === 'fulfilled' ? (summaryResult.value as DashboardSummary) : null;
    const upcomingData = upcomingResult.status === 'fulfilled' ? upcomingResult.value : null;
    // api.Recommendation and LocalRecommendation are structurally identical
    // except for provider: string vs provider: Provider. The provider values
    // from the API are always the union members at runtime, so this cast is safe.
    // Defensive extraction: the backend always returns the envelope shape
    //   { recommendations: [...], summary: {...}, regions: [...] }
    // so the real runtime value is never a flat array. We unwrap it here to
    // match what the Opportunities page does (cast to RecommendationsResponse
    // and read .recommendations). A flat-array result is also accepted so
    // test fixtures that resolve with a plain array continue to work.
    // #304: apiRequest resolves `null` for a 204 or content-length 0 response
    // (a malformed 2xx body rejects, #273); guard against null / unexpected
    // shapes to avoid "X is not iterable".
    const rawRecs = recsResult.status === 'fulfilled' ? recsResult.value : null;
    const recsArray = Array.isArray(rawRecs)
      ? rawRecs
      : (rawRecs != null && typeof rawRecs === 'object' && Array.isArray((rawRecs as { recommendations?: unknown }).recommendations))
        ? (rawRecs as { recommendations: unknown[] }).recommendations
        : [];
    // #180: apply the same Settings -> Enabled Providers filter as the
    // Opportunities page, so the two Potential Monthly Savings cards are
    // computed from the same rows. A failed recs fetch stays distinguishable
    // (null) so the tile can fall back to the backend count.
    const recs: readonly LocalRecommendation[] = filterByEnabledProviders(
      recsArray as unknown as LocalRecommendation[],
      enabledProviders.status === 'fulfilled' ? enabledProviders.value : null,
    );
    const recsCount = recsResult.status === 'fulfilled' ? recs.length : null;

    if (summaryResult.status === 'rejected') {
      throw summaryResult.reason as Error;
    }
    if (summaryData == null) {
      throw new Error('Dashboard summary response was empty');
    }

    // Build a human-readable filter description for filter-aware empty states
    // on the Home chart. Mirrors the pattern from loadSavingsTrendChart (#747).
    const filterDesc = buildFilterDesc(currentProvider, currentAccountIDs);

    renderDashboardSummary(summaryData, recs, recsCount);
    // `recs` is already narrowed to Enabled Providers (#180), matching the
    // Opportunities first-run check, which also runs on the preference-
    // filtered list.
    renderHomeOnboarding(
      recsResult.status === 'fulfilled' && recs.length === 0 && !summaryData.active_commitments,
      accountsResult.status === 'fulfilled' && Array.isArray(accountsResult.value)
        ? accountsResult.value.length > 0
        : null,
      isScopeFiltered(currentProvider, currentAccountIDs),
    );
    // Single merged per-service chart (#908): potential range from recs +
    // current-savings underlay from the summary's by_service map.
    renderSavingsByService(recs, summaryData.by_service || {}, filterDesc);
    if (upcomingResult.status === 'rejected') {
      console.error('Failed to load upcoming purchases:', upcomingResult.reason);
      renderUpcomingError();
    } else {
      renderUpcomingPurchases(upcomingData?.purchases || []);
    }
    // Load the savings-over-time widget independently -- failure shouldn't
    // block the rest of the dashboard (e.g. analytics not configured).
    void loadSavingsTrendChart();
    void loadYtdSparkline();

    // Auto-refresh the recommendations cache if the last collection
    // is older than 24h (or never ran). The dashboard's KPI tiles +
    // savings card derive from recs; without this, a user who only
    // opens Home would never trigger the refresh cycle that the
    // Opportunities page used to own exclusively. The helper surfaces
    // collection errors as toasts and dedups concurrent refreshes; on
    // success it calls loadDashboard() to repaint this page with the
    // fresh data instead of forcing a recs render.
    void triggerAutoRefreshIfStale(loadDashboard);
  } catch (error) {
    console.error('Failed to load dashboard:', error);
    const summary = document.getElementById('summary');
    if (summary) {
      teardownSkeleton(summary);
      const err = error as Error;
      while (summary.firstChild) summary.removeChild(summary.firstChild);
      const p = document.createElement('p');
      p.classList.add('error');
      p.textContent = `Failed to load dashboard: ${err.message}`;
      summary.appendChild(p);
    }
    // Clear the upcoming-list skeleton too — shimmer next to a dashboard
    // error reads as a fresh fetch in-flight, which is misleading.
    const upcoming = document.getElementById('upcoming-list');
    if (upcoming) teardownSkeleton(upcoming);
  }
}

/**
 * Settings -> Enabled Providers, or null when the config can't be read (a
 * missing permission or network error must not block Home; null means "no
 * filter", the same permissive fallback the Opportunities page uses).
 */
async function loadEnabledProviders(): Promise<readonly string[] | null> {
  try {
    const cfg = await api.getConfig();
    return cfg?.global?.enabled_providers ?? null;
  } catch {
    return null;
  }
}

/**
 * Issue #756: first-run onboarding card at the top of Home. Shown when no
 * recommendations loaded and nothing is committed yet, so a new user gets a
 * pointer to connecting an account (or refreshing) instead of a page of
 * "--" tiles and empty charts.
 */
function renderHomeOnboarding(show: boolean, hasAccounts: boolean | null, scopeFiltered: boolean): void {
  const host = document.getElementById('home-onboarding');
  if (!host) return;
  host.classList.toggle('hidden', !show);
  if (!show) {
    host.replaceChildren();
    return;
  }
  host.replaceChildren(buildFirstRunEmptyState({
    hasAccounts,
    scopeFiltered,
    onRefresh: () => startRecommendationsRefresh(loadDashboard),
    loadFreshness: getRecommendationsFreshness,
  }));
}

function renderDashboardSummary(
  data: DashboardSummary,
  recs: readonly LocalRecommendation[],
  recsCount: number | null,
): void {
  const summary = document.getElementById('summary');
  if (!summary) return;

  // Compute per-cell savings range from recs. pageLevelRange sums per-cell
  // min/max savings (best and worst variant per physical resource cell),
  // avoiding the ~6x inflation of summing every variant of every cell that
  // the flat summary.potential_monthly_savings carries.
  // Falls back to NO_VALUE (an em dash) when recs is empty or fetch failed; never fabricates $0
  // for an absent/failed computation (finding H-3: no silent $0 fallback).
  // Soft-fail (#304): wrap in try/catch so an unexpected non-iterable shape
  // that slips past the Array.isArray guard in loadDashboard (e.g. if
  // groupRecsByCell is called from a path that bypasses the guard) cannot
  // blank the entire dashboard -- the savings card shows NO_VALUE instead.
  let savingsDisplay: string;
  try {
    const groups = groupRecsByCell(recs);
    const range = pageLevelRange(groups);
    savingsDisplay = range.cellCount > 0
      ? formatSavingsRange(range.savingsMin, range.savingsMax)
      : NO_VALUE;
  } catch (recErr) {
    console.warn('Dashboard: failed to compute savings range from recommendations:', recErr);
    savingsDisplay = NO_VALUE;
  }

  // When no recommendations and no commitments exist, "100% coverage" is
  // misleading -- nothing is being tracked. Show a dash instead.
  const nothingTracked = !data.total_recommendations && !data.active_commitments;
  // #978: round to 1 decimal place to match sibling coverage formatting (inventory.ts)
  // and prevent overflow at narrower screen widths.
  // H-3: use null-safe checks so absent coverage/target are shown as NO_VALUE
  // rather than fabricating '0%'/'80%'. The hardcoded 80 default is removed
  // because target_coverage is a user-configured value from the API -- the
  // frontend must not substitute a business default when the field is absent.
  const currentCoverage = data.current_coverage ?? null;
  const targetCoverage = data.target_coverage ?? null;
  const coverageValue = nothingTracked
    ? '—'
    : currentCoverage !== null ? `${currentCoverage.toFixed(1)}%` : NO_VALUE;
  const coverageDetail = nothingTracked
    ? 'No services tracked'
    : targetCoverage !== null ? `Target: ${targetCoverage.toFixed(1)}%` : `Target: ${NO_VALUE}`;

  // Render KPI tiles via DOM construction (textContent / appendChild)
  // rather than an innerHTML template literal, per the issue #340 plan's
  // XSS constraint. The values are all backend-sourced numbers/strings,
  // but the safe-by-default pattern is cheap and removes the question.
  while (summary.firstChild) summary.removeChild(summary.firstChild);
  const tiles: ReadonlyArray<{
    kpi: string;
    title: string;
    value: string;
    valueSavings?: boolean;
    detail: string;
  }> = [
    { kpi: 'savings',     title: 'Potential Monthly Savings', value: savingsDisplay, valueSavings: true,
      // #180: count the same rows the range sums (and the Opportunities page
      // counts); the backend total ignores the Enabled Providers setting.
      detail: `${recsCount ?? data.total_recommendations ?? NO_VALUE} recommendations` },
    { kpi: 'commitments', title: 'Active Commitments', value: data.active_commitments != null ? String(data.active_commitments) : NO_VALUE,
      detail: `${formatCurrency(data.committed_monthly)}/mo committed` },
    { kpi: 'coverage',    title: 'Current Coverage', value: coverageValue, detail: coverageDetail },
    { kpi: 'ytd',         title: 'YTD Savings', value: formatCurrency(data.ytd_savings), valueSavings: true,
      detail: 'From commitment purchases' },
  ];
  for (const t of tiles) {
    const card = document.createElement('div');
    card.classList.add('card', 'kpi-tile');
    card.dataset['kpi'] = t.kpi;
    const h3 = document.createElement('h3');
    h3.textContent = t.title;
    const valueP = document.createElement('p');
    valueP.classList.add('value', 'kpi-tile-value');
    // An absent figure is not a saving: keep the placeholder neutral rather
    // than green (issue #739).
    if (t.valueSavings && t.value !== NO_VALUE) valueP.classList.add('savings');
    valueP.textContent = t.value;
    const detailP = document.createElement('p');
    detailP.classList.add('detail', 'kpi-tile-detail');
    detailP.textContent = t.detail;
    const spark = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    spark.classList.add('kpi-tile-spark', 'hidden');
    spark.dataset['sparkKey'] = t.kpi;
    spark.setAttribute('aria-hidden', 'true');
    card.appendChild(h3);
    card.appendChild(valueP);
    card.appendChild(detailP);
    card.appendChild(spark);
    summary.appendChild(card);
  }
}

/**
 * Build a small SVG polyline path string from a series of numeric values,
 * normalized into a width × height viewport. Returns the points string for
 * a <polyline points="..."> element. Pure helper — no DOM access, no I/O.
 */
function sparklinePoints(values: readonly number[], width: number, height: number): string {
  if (values.length < 2) return '';
  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = max - min || 1;
  const stepX = width / (values.length - 1);
  return values.map((v, i) => {
    const x = i * stepX;
    const y = height - ((v - min) / range) * height;
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(' ');
}

/**
 * Attach an SVG sparkline to a single KPI tile, keyed by its data-spark-key.
 * Silently no-ops when the placeholder isn't in the DOM (e.g. a different
 * card layout is rendered) or when there aren't enough data points to draw
 * a meaningful line. Uses DOM methods only — no innerHTML.
 */
function attachSparkline(key: string, values: readonly number[]): void {
  const svg = document.querySelector<SVGSVGElement>(`.kpi-tile-spark[data-spark-key="${key}"]`);
  if (!svg) return;
  while (svg.firstChild) svg.removeChild(svg.firstChild);
  if (values.length < 2) {
    svg.classList.add('hidden');
    return;
  }
  svg.classList.remove('hidden');

  const width = 80;
  const height = 24;
  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);
  svg.setAttribute('preserveAspectRatio', 'none');

  const points = sparklinePoints(values, width, height);
  if (!points) {
    svg.classList.add('hidden');
    return;
  }

  const polyline = document.createElementNS('http://www.w3.org/2000/svg', 'polyline');
  polyline.setAttribute('points', points);
  polyline.setAttribute('fill', 'none');
  polyline.setAttribute('stroke', 'currentColor');
  polyline.setAttribute('stroke-width', '1.5');
  polyline.setAttribute('stroke-linecap', 'round');
  polyline.setAttribute('stroke-linejoin', 'round');
  svg.appendChild(polyline);
}

export const __test__ = { sparklinePoints, attachSparkline, computeServiceStats };

// canCancelUpcomingPurchase returns true when the current session is
// permitted to cancel the given upcoming purchase via the Dashboard
// widget (issues #950, #1400). UX gate only -- the backend
// authorizePlannedPurchaseCancel in internal/api/handler_purchases.go
// remains the security boundary; a false-positive surfaces as a 403
// toast rather than a successful mutation.
//
// Full-scope (any row): admin (admin:*), cancel-any:purchases, or
//   update-any:purchases.
// Creator-scope (own row only): delete:purchases OR cancel-own:purchases,
//   PLUS non-null created_by_user_id matching the current user.
//   delete:purchases mirrors the original ownership gate (issue #950).
//   cancel-own:purchases enables Standard users to cancel their own
//   upcoming purchases from the Home page (issue #1400).
// Legacy / scheduler-tick rows with undefined created_by_user_id ->
//   no Cancel button for non-privileged users (out of reach without a
//   full-scope verb).
export function canCancelUpcomingPurchase(purchase: UpcomingPurchase): boolean {
  // Full-scope: admin, cancel-any, or update-any grant unrestricted row access.
  if (
    canAccess('admin', '*') ||
    canAccess('cancel-any', 'purchases') ||
    canAccess('update-any', 'purchases')
  ) {
    return true;
  }
  // Creator-scope: delete:purchases or cancel-own:purchases + creator match.
  const user = state.getCurrentUser();
  if (!user) return false;
  if (!purchase.created_by_user_id) return false;
  if (purchase.created_by_user_id !== user.id) return false;
  return canAccess('delete', 'purchases') || canAccess('cancel-own', 'purchases');
}

// A failed fetch must not read as "nothing scheduled" (#713): show an inline
// error with a Retry that refetches only this widget.
function renderUpcomingError(): void {
  const container = document.getElementById('upcoming-list');
  if (!container) return;
  teardownSkeleton(container);
  container.textContent = '';
  const msg = document.createElement('p');
  msg.className = 'error';
  msg.setAttribute('role', 'alert');
  msg.textContent = 'Failed to load upcoming purchases.';
  const retry = document.createElement('button');
  retry.type = 'button';
  retry.className = 'btn btn-secondary';
  retry.dataset['action'] = 'retry-upcoming';
  retry.textContent = 'Retry';
  retry.addEventListener('click', () => { void retryUpcomingPurchases(); });
  container.append(msg, retry);
}

async function retryUpcomingPurchases(): Promise<void> {
  const container = document.getElementById('upcoming-list');
  if (container) {
    // The Retry button is about to be replaced; keep focus in the widget.
    container.tabIndex = -1;
    container.focus();
    showSkeletonBlock(container, '100%', '6rem');
  }
  try {
    const data = await api.getUpcomingPurchases();
    // `?.`: apiRequest resolves null for an empty 2xx body (#304).
    renderUpcomingPurchases(data?.purchases || []);
  } catch (error) {
    console.error('Failed to load upcoming purchases:', error);
    renderUpcomingError();
  }
}

function renderUpcomingPurchases(allPurchases: UpcomingPurchase[]): void {
  const container = document.getElementById('upcoming-list');
  if (!container) return;

  // The endpoint ignores the topbar chips, so apply the provider chip here
  // and flag the account chip as not applied (#713, see lib/upcoming-scope).
  const provider = state.getCurrentProvider();
  const purchases = scopeUpcomingToProvider(allPurchases, provider);

  // Refresh the in-memory index so viewPurchaseDetails can render its
  // dialog from local data — there is no execution row to look up yet
  // (the upcoming list shows plans whose next execution hasn't fired).
  upcomingPurchasesIndex = new Map(purchases.map(p => [p.execution_id, p]));

  container.textContent = '';
  if (purchases.length === 0) {
    const empty = document.createElement('p');
    empty.className = 'empty';
    empty.textContent = upcomingEmptyMessage(provider);
    container.appendChild(empty);
    return;
  }

  if (state.getCurrentAccountIDs().length > 0) {
    const note = document.createElement('p');
    note.className = 'help-text upcoming-scope-note';
    note.textContent = UPCOMING_ACCOUNT_SCOPE_NOTE;
    container.appendChild(note);
  }

  for (const p of purchases) {
    const dateParts = getDateParts(p.scheduled_date);

    const card = document.createElement('div');
    card.className = 'upcoming-card';

    // Info block
    const info = document.createElement('div');
    info.className = 'upcoming-info';

    const dateDiv = document.createElement('div');
    dateDiv.className = 'upcoming-date';
    const dayDiv = document.createElement('div');
    dayDiv.className = 'day';
    dayDiv.textContent = String(dateParts.day);
    const monthDiv = document.createElement('div');
    monthDiv.className = 'month';
    monthDiv.textContent = dateParts.month;
    dateDiv.appendChild(dayDiv);
    dateDiv.appendChild(monthDiv);

    const details = document.createElement('div');
    details.className = 'upcoming-details';
    const h4 = document.createElement('h4');
    h4.textContent = p.plan_name;
    const descP = document.createElement('p');
    const badge = document.createElement('span');
    badge.className = 'provider-badge';
    // Use shared whitelist helper (providerBadgeClass) for consistency with
    // plans.ts and history.ts (D1 / L4 -- aligns on aws|azure|gcp set).
    const safeProvider = providerBadgeClass(p.provider);
    if (safeProvider) badge.classList.add(safeProvider);
    badge.textContent = (p.provider || '').toUpperCase();
    descP.appendChild(badge);
    descP.appendChild(document.createTextNode(` ${p.service} - Step ${p.step_number} of ${p.total_steps}`));
    details.appendChild(h4);
    details.appendChild(descP);

    info.appendChild(dateDiv);
    info.appendChild(details);

    // Savings block
    const savings = document.createElement('div');
    savings.className = 'upcoming-savings';
    const amountDiv = document.createElement('div');
    amountDiv.className = 'amount';
    // platform#609: a step with no recommendations has nothing to buy, and
    // its savings is a placeholder. Say so rather than render $0.
    const hasNoRecommendations = p.has_recommendations === false;
    amountDiv.textContent = hasNoRecommendations ? '—' : formatCurrency(p.estimated_savings);
    const labelDiv = document.createElement('div');
    labelDiv.className = 'label';
    labelDiv.textContent = hasNoRecommendations ? 'No recommendations attached' : 'Est. monthly savings';
    savings.appendChild(amountDiv);
    savings.appendChild(labelDiv);

    // Actions block
    const actions = document.createElement('div');
    actions.className = 'upcoming-actions';
    const viewBtn = document.createElement('button');
    viewBtn.dataset['action'] = 'view-purchase';
    viewBtn.dataset['id'] = String(p.execution_id);
    viewBtn.textContent = 'View Details';
    actions.appendChild(viewBtn);
    // Issue #950: Cancel routes to DELETE /api/purchases/planned/{id},
    // which the backend now gates on creator-scope ownership. Hide the
    // button when the current session is not authorised so the operator
    // doesn't get a 403 toast on click. Plans page applies the same gate
    // via canManageScheduledPurchase + canAccess('delete','purchases').
    if (canCancelUpcomingPurchase(p)) {
      const cancelBtn = document.createElement('button');
      cancelBtn.dataset['action'] = 'cancel-purchase';
      cancelBtn.dataset['id'] = String(p.execution_id);
      // Outlined destructive (issue #695): the filled red `danger` style made
      // Cancel the loudest element on every row. It still reads as
      // destructive and fills on hover; the confirm dialog guards the action.
      cancelBtn.className = 'btn-danger';
      cancelBtn.textContent = 'Cancel';
      actions.appendChild(cancelBtn);
    }

    card.appendChild(info);
    card.appendChild(savings);
    card.appendChild(actions);
    container.appendChild(card);
  }

  // Add event listeners
  container.querySelectorAll<HTMLButtonElement>('[data-action="view-purchase"]').forEach(btn => {
    btn.addEventListener('click', () => viewPurchaseDetails(btn.dataset['id'] || ''));
  });
  container.querySelectorAll<HTMLButtonElement>('[data-action="cancel-purchase"]').forEach(btn => {
    btn.addEventListener('click', () => void cancelScheduledPurchase(btn.dataset['id'] || '', btn));
  });
}

function viewPurchaseDetails(executionId: string): void {
  // The widget enumerates pending purchase_executions, so each row in the
  // upcoming list maps to a real execution row. We still render the dialog
  // from the in-memory snapshot to avoid an extra roundtrip — every field
  // the dialog displays already came down with the upcoming list.
  const purchase = upcomingPurchasesIndex.get(executionId);
  if (!purchase) {
    showToast({ message: 'Failed to load purchase details: not in current view', kind: 'error' });
    return;
  }

  // Remove any existing details modal
  document.getElementById('purchase-details-modal')?.remove();

  const modal = buildUpcomingDetailsModal(purchase, executionId);
  document.body.appendChild(modal);
  // Focus moves into the dialog, Tab is trapped, Escape closes it and focus
  // returns to View Details (#731). Every close path removes the element.
  openModal(modal, { onClose: () => modal.remove() });
}

// buildUpcomingDetailsModal constructs the dialog via DOM-API, not
// innerHTML, so user-controlled fields (plan_name, service) can never be
// interpreted as HTML — closes a class of XSS regressions that the
// previous escapeHtml-+-innerHTML pattern only avoided by convention.
function buildUpcomingDetailsModal(p: UpcomingPurchase, executionId: string): HTMLDivElement {
  const modal = document.createElement('div');
  modal.id = 'purchase-details-modal';
  modal.className = 'modal';
  modal.setAttribute('role', 'dialog');
  modal.setAttribute('aria-modal', 'true');
  modal.setAttribute('aria-labelledby', 'purchase-details-modal-title');

  const content = document.createElement('div');
  content.className = 'modal-content modal-wide';
  modal.appendChild(content);

  const h2 = document.createElement('h2');
  h2.id = 'purchase-details-modal-title';
  h2.textContent = 'Upcoming Purchase Details';
  content.appendChild(h2);

  const section = document.createElement('div');
  section.className = 'form-section';
  content.appendChild(section);

  const sectionH3 = document.createElement('h3');
  sectionH3.textContent = 'Plan Info';
  section.appendChild(sectionH3);

  const table = document.createElement('table');
  const tbody = document.createElement('tbody');
  table.appendChild(tbody);
  section.appendChild(table);

  const addRow = (label: string, value: string): void => {
    const tr = document.createElement('tr');
    const tdLabel = document.createElement('td');
    const strong = document.createElement('strong');
    strong.textContent = label;
    tdLabel.appendChild(strong);
    const tdValue = document.createElement('td');
    tdValue.textContent = value;
    tr.appendChild(tdLabel);
    tr.appendChild(tdValue);
    tbody.appendChild(tr);
  };

  addRow('Plan name', p.plan_name);
  addRow('Plan ID', p.plan_id);
  addRow('Execution ID', executionId);
  addRow('Scheduled', p.scheduled_date);
  addRow('Provider', p.provider.toUpperCase());
  addRow('Service', p.service);
  addRow('Step', `${p.step_number} of ${p.total_steps}`);
  addRow(
    'Est. monthly savings',
    p.has_recommendations === false ? 'No recommendations attached' : formatCurrency(p.estimated_savings),
  );

  // Buttons row
  const btnRow = document.createElement('div');
  btnRow.className = 'modal-buttons';
  content.appendChild(btnRow);

  // Issue #950: gate the Cancel button on the same ownership check as
  // the card-level button above. Users land here from "View Details",
  // which is visible to everyone; only the destructive action needs the
  // creator-scope gate.
  if (canCancelUpcomingPurchase(p)) {
    const cancelBtn = document.createElement('button');
    cancelBtn.type = 'button';
    cancelBtn.id = 'cancel-purchase-detail-btn';
    cancelBtn.className = 'danger';
    cancelBtn.textContent = 'Cancel Purchase';
    // Same confirm copy and request path as the card's Cancel button (#731).
    cancelBtn.addEventListener('click', () => {
      void cancelScheduledPurchase(executionId, cancelBtn, () => closeModal(modal));
    });
    btnRow.appendChild(cancelBtn);
  }

  const closeBtn = document.createElement('button');
  closeBtn.type = 'button';
  closeBtn.id = 'close-purchase-details-btn';
  closeBtn.textContent = 'Close';
  closeBtn.addEventListener('click', () => closeModal(modal));
  btnRow.appendChild(closeBtn);

  return modal;
}

// Execution IDs with a cancel in progress. A second click, or a click on the
// other Cancel button for the same step (card vs details dialog), must not
// open another confirm or send a second DELETE (#731).
const cancelsInFlight = new Set<string>();

async function cancelScheduledPurchase(
  executionId: string,
  trigger?: HTMLButtonElement,
  onCancelled?: () => void,
): Promise<void> {
  // Cancel just THIS scheduled instance via DELETE /api/purchases/planned/{id}
  // (the deletePlannedPurchase backend handler operates on
  // purchase_executions). The plan template stays intact — the next
  // scheduler tick re-creates the next instance for the plan. Earlier
  // iterations got this wrong by either:
  //   - sending plan_id to deletePlannedPurchase (404 because
  //     deletePlannedPurchase expects an execution_id), or
  //   - falling back to deletePlan (which deleted the WHOLE plan).
  // The widget now enumerates real pending executions and surfaces
  // execution_id, so neither workaround is needed.
  if (cancelsInFlight.has(executionId)) return;
  cancelsInFlight.add(executionId);
  try {
    const ok = await confirmDialog({
      title: 'Cancel this scheduled purchase?',
      body: 'Cancelling removes this upcoming step from the plan. The plan itself stays in place; the next scheduled step (if any) is unaffected.',
      confirmLabel: 'Cancel purchase',
      destructive: true,
    });
    if (!ok) return;

    // Disabled only after the confirm closes, so the confirm can hand focus
    // back to the button first.
    if (trigger) trigger.disabled = true;
    await api.deletePlannedPurchase(executionId);
    onCancelled?.();
    await loadDashboard();
    showToast({ message: 'Purchase cancelled successfully', kind: 'success', timeout: 5_000 });
  } catch (error) {
    console.error('Failed to cancel purchase:', error);
    showToast({ message: 'Failed to cancel purchase', kind: 'error' });
  } finally {
    cancelsInFlight.delete(executionId);
    if (trigger) trigger.disabled = false;
  }
}

/**
 * Per-service savings statistics derived from a window of data points or
 * from a recommendations list.
 * Exported for unit testing only.
 */
export interface ServiceSavingsStats {
  min: number;
  max: number;
  sum: number;
  count: number;
  samples: number[];
  /** Label of the recommendation option that produced the minimum savings (e.g. "1yr no-upfront"). */
  minLabel?: string;
  /** Label of the recommendation option that produced the maximum savings (e.g. "3yr all-upfront"). */
  maxLabel?: string;
}

/**
 * Compute per-service savings stats from an array of data points. Each
 * data point carries a by_service map of { serviceName: savingsValue }.
 * Points with no by_service entry (omitempty from backend) are skipped.
 * Exported for unit testing.
 *
 * @deprecated Used only by historical-data tests. New production code uses
 * computeServiceStatsFromRecs for forward-looking potential savings.
 */
export function computeServiceStats(
  dataPoints: readonly SavingsDataPoint[],
): Map<string, ServiceSavingsStats> {
  const stats = new Map<string, ServiceSavingsStats>();
  for (const dp of dataPoints) {
    if (!dp.by_service) continue;
    for (const [svc, val] of Object.entries(dp.by_service)) {
      if (typeof val !== 'number') continue;
      const existing = stats.get(svc);
      if (existing) {
        existing.min = Math.min(existing.min, val);
        existing.max = Math.max(existing.max, val);
        existing.sum += val;
        existing.count += 1;
        existing.samples.push(val);
      } else {
        stats.set(svc, { min: val, max: val, sum: val, count: 1, samples: [val] });
      }
    }
  }
  return stats;
}

/**
 * Compute per-service potential-savings stats from a recommendations list.
 *
 * For each service, collects every recommendation row and treats each row's
 * `savings` value as one "option". The bar's:
 *   - floor  = min(savings) across all rows for that service
 *   - upside = max(savings) - min(savings)
 *
 * When a service has only one recommendation row, the bar collapses to a
 * single point (floor only, zero upside) — that is the correct visual.
 *
 * NOTE: The current recommendations response carries a single `savings` field
 * per row rather than per-variant breakdowns (e.g. 1yr/3yr × no-upfront/
 * all-upfront columns). When per-variant rows are shipped, min/max will
 * automatically reflect the full option range; no code change is required.
 * See #769 for context.
 *
 * Exported for unit testing.
 */
export function computeServiceStatsFromRecs(
  recs: readonly LocalRecommendation[],
): Map<string, ServiceSavingsStats> {
  const stats = new Map<string, ServiceSavingsStats>();
  for (const rec of recs) {
    const svc = rec.service;
    const val = typeof rec.savings === 'number' ? rec.savings : 0;
    const paymentLabel = rec.payment && rec.payment.trim().length > 0 ? rec.payment : 'unspecified';
    const label = `${rec.term}yr ${paymentLabel}`;
    const existing = stats.get(svc);
    if (existing) {
      if (val < existing.min) {
        existing.min = val;
        existing.minLabel = label;
      }
      if (val > existing.max) {
        existing.max = val;
        existing.maxLabel = label;
      }
      existing.sum += val;
      existing.count += 1;
      existing.samples.push(val);
    } else {
      stats.set(svc, { min: val, max: val, sum: val, count: 1, samples: [val], minLabel: label, maxLabel: label });
    }
  }
  return stats;
}


/**
 * Render the single merged per-service savings chart (issues #769 + #908).
 *
 * Each service bar is a single stack with three layers (bottom to top):
 *   1. Current / Committed (darkest shade): already-realized savings from
 *      active commitments, sourced from by_service[svc].current_savings.
 *   2. Lowest option (solid mid-shade): the lowest available recommendation
 *      option minus what is already committed, i.e. max(0, min(rec.savings) -
 *      current). Together with the Current layer this represents the floor.
 *   3. Upside (lightest / 35% opacity): max(rec.savings) - min(rec.savings),
 *      the variability between the cheapest and most-aggressive option.
 *
 * Services are sorted by (current + max potential) descending so the bar
 * with the most total visible value is leftmost. Services present in
 * byService but absent from recs still appear with only the Current band
 * (no further upside surfaced). Services present in recs but absent from
 * byService default to current = 0 (no committed savings yet).
 *
 * The top SAVINGS_BY_SERVICE_MAX services are shown; the heading notes
 * "+N more" when truncated.
 *
 * Empty state: shown when no bar (across all three layers) has any positive
 * value.
 */
export function renderSavingsByService(
  recs: readonly LocalRecommendation[],
  byService: Record<string, ServiceSavings> = {},
  filterDesc = '',
): void {
  const canvas = document.getElementById('savings-by-service-chart') as HTMLCanvasElement | null;
  const emptyEl = document.getElementById('savings-by-service-empty');
  const section = document.getElementById('savings-by-service-section');
  if (!canvas) return;

  // Destroy existing chart before rebuilding.
  if (savingsByServiceChart) {
    savingsByServiceChart.destroy();
    savingsByServiceChart = null;
  }

  const stats = computeServiceStatsFromRecs(recs);
  const heading = section?.querySelector('h3');

  // Build the union of services from both recs and byService. Services in
  // byService-only (commitment exists, no new rec) still show a Current band.
  const allServices = new Set<string>([
    ...stats.keys(),
    ...Object.keys(byService),
  ]);

  // Per-service totals used for sort and empty-state detection.
  type SvcEntry = {
    svc: string;
    current: number;
    minRec: number;
    maxRec: number;
    lowestOption: number;
    upside: number;
    visibleTotal: number;
  };
  const entries: SvcEntry[] = Array.from(allServices).map(svc => {
    const s = stats.get(svc);
    const current = byService[svc]?.current_savings ?? 0;
    const minRec = s?.min ?? 0;
    const maxRec = s?.max ?? 0;
    const lowestOption = Math.max(0, minRec - current);
    const upside = Math.max(0, maxRec - minRec);
    return {
      svc,
      current,
      minRec,
      maxRec,
      lowestOption,
      upside,
      visibleTotal: current + lowestOption + upside,
    };
  });

  // Keep only services with some positive value in any layer.
  const positive = entries.filter(e => e.visibleTotal > 0);

  // Sort by visible total desc so the most-valuable bar is leftmost.
  positive.sort((a, b) => b.visibleTotal - a.visibleTotal);

  if (positive.length === 0) {
    if (heading) heading.textContent = 'Potential savings range per service';
    canvas.classList.add('hidden');
    removeChartTable(SAVINGS_BY_SERVICE_TABLE_ID);
    if (emptyEl) {
      // When a filter is active and excluded all results, surface it so the
      // user understands why the chart is blank (mirrors #747 pattern).
      emptyEl.textContent = filterDesc
        ? `No recommendations for the selected filter (${filterDesc}).`
        : 'No positive potential savings found for current recommendations.';
      emptyEl.classList.remove('hidden');
    }
    return;
  }

  canvas.classList.remove('hidden');
  emptyEl?.classList.add('hidden');

  // Cap at maximum and update heading with "+N more" if truncated.
  const truncated = positive.length > SAVINGS_BY_SERVICE_MAX;
  const visible = positive.slice(0, SAVINGS_BY_SERVICE_MAX);
  if (heading) {
    heading.textContent = truncated
      ? `Potential savings range per service (+${positive.length - SAVINGS_BY_SERVICE_MAX} more)`
      : 'Potential savings range per service';
  }

  const labels = visible.map(e => e.svc);

  // Layer 1 (bottom): Current / Committed savings.
  const currentData = visible.map(e => e.current);

  // Layer 2: Lowest option -- the gap between the cheapest rec and the already-
  // committed amount. Clamped to 0 to avoid negative segments when commitments
  // exceed the current lowest-option savings figure.
  const lowestOptionData = visible.map(e => Math.max(0, e.minRec - e.current));

  // Layer 3 (top): Upside -- the spread between cheapest and most-aggressive option.
  const upsideData = visible.map(e => Math.max(0, e.maxRec - e.minRec));

  const totalSavings = positive.reduce((acc, e) => acc + e.visibleTotal, 0);

  // #744: the canvas is role="img", so its label and a table are the only
  // way a screen-reader user can read these values. Every service is listed,
  // including any beyond the SAVINGS_BY_SERVICE_MAX bars drawn.
  const top = positive[0]!;
  setChartSummary(canvas,
    `Stacked bar chart of monthly savings for ${positive.length} service${positive.length === 1 ? '' : 's'}, `
    + `${formatCurrency(totalSavings)} in total. Largest: ${top.svc} at ${formatCurrency(top.visibleTotal)}. `
    + 'Layers: Current / Committed, Lowest option, Upside. Values are in the table below the chart.');
  renderChartTable(canvas, SAVINGS_BY_SERVICE_TABLE_ID, {
    caption: 'Potential savings range per service (monthly)',
    columns: ['Service', 'Current / Committed', 'Lowest option', 'Upside', 'Total'],
    rows: positive.map(e => [e.svc, formatCurrency(e.current), formatCurrency(e.lowestOption),
      formatCurrency(e.upside), formatCurrency(e.visibleTotal)]),
  });

  savingsByServiceChart = new Chart(canvas, {
    type: 'bar',
    data: {
      labels,
      datasets: [
        // Current sits at the base of the stack so already-realized savings
        // are visually "under" the potential range -- dataset order in Chart.js
        // stacked bars determines bottom-to-top rendering.
        {
          label: 'Current / Committed',
          data: currentData,
          backgroundColor: SAVINGS_LAYER_COLORS.current,
          borderRadius: { topLeft: 0, topRight: 0, bottomLeft: 4, bottomRight: 4 },
          ...SEGMENT_GAP,
          stack: 'savings',
        },
        {
          label: 'Lowest option',
          data: lowestOptionData,
          backgroundColor: SAVINGS_LAYER_COLORS.lowestOption,
          borderRadius: { topLeft: 0, topRight: 0, bottomLeft: 0, bottomRight: 0 },
          ...SEGMENT_GAP,
          stack: 'savings',
        },
        {
          label: 'Upside',
          data: upsideData,
          backgroundColor: SAVINGS_LAYER_COLORS.upside,
          borderRadius: { topLeft: 4, topRight: 4, bottomLeft: 0, bottomRight: 0 },
          ...SEGMENT_GAP,
          stack: 'savings',
        },
      ],
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      plugins: {
        legend: {
          display: true,
          position: 'top',
          // Default swatches: each one is its dataset's single bar colour.
          labels: { boxWidth: 12 },
        },
        tooltip: {
          callbacks: {
            label: (ctx) => {
              const svc = ctx.label ?? '';
              const s = stats.get(svc);
              // The current-savings dataset gets a focused, single-line
              // tooltip; the range datasets share the full breakdown. This
              // runs before the stats guard: committed services without a
              // recommendation have no stats but still have a bar.
              if (ctx.dataset?.label === 'Current / Committed') {
                const current = byService[svc]?.current_savings ?? 0;
                return `Current / Committed: ${formatCurrency(current)}`;
              }
              if (!s) return '';
              const current = byService[svc]?.current_savings ?? 0;
              const maxRec = s?.max ?? 0;
              const minRec = s?.min ?? 0;
              const lowestOption = Math.max(0, minRec - current);
              const upside = Math.max(0, maxRec - minRec);
              const total = current + lowestOption + upside;
              const pct = totalSavings > 0 ? ((total / totalSavings) * 100).toFixed(1) : '0.0';
              const lines = [
                `Service: ${svc}`,
                `Total: ${formatCurrency(total)} (${pct}% of all services)`,
                `Current / Committed: ${formatCurrency(current)}`,
                `Lowest option: ${formatCurrency(lowestOption)}`,
                `Upside: ${formatCurrency(upside)}`,
              ];
              if (s?.minLabel) lines.push(`Min option: ${s.minLabel}`);
              if (s?.maxLabel) lines.push(`Max option: ${s.maxLabel}`);
              return lines;
            },
          },
        },
      },
      scales: {
        x: {
          stacked: true,
          // No axis title: the tick labels are the service names (#189).
          // Keep every service label on narrow screens (rotate instead of skipping).
          ticks: { autoSkip: false, maxRotation: 60 },
        },
        y: {
          stacked: true,
          beginAtZero: true,
          title: { display: true, text: 'Monthly savings ($)' },
          ticks: { callback: (v) => formatCurrency(v as number) },
        },
      },
    },
  });
}

/**
 * Load the savings-over-time trend chart for the dashboard. Fetches the
 * history analytics endpoint with the currently-selected range and renders
 * a line chart of cumulative savings spanning the full selected window on
 * the x-axis (QA row 405, step 3.1). When the response is empty (QA 2.3
 * supersedes QA 3.1's earlier empty-axes suggestion), the canvas is hidden
 * and an empty-state banner is shown: a filter-aware "No savings history
 * for <filter>" when a provider or account chip is active, otherwise the
 * generic "No purchase history yet." A single data point is too sparse to
 * draw a trend, so it gets a "Not enough history yet" message carrying that
 * point's value instead of a lone dot on a blank canvas (#188). Fetch failures
 * hide the canvas and show the "Savings history is not available yet." stub.
 */
export async function loadSavingsTrendChart(): Promise<void> {
  const canvas = document.getElementById('savings-trend-chart') as HTMLCanvasElement | null;
  const empty = document.getElementById('savings-trend-empty');
  if (!canvas) return;

  const now = new Date();
  const nowMs = now.getTime();
  const isAllRange = savingsTrendRange === 'all';
  const days = isAllRange ? null : parseInt(savingsTrendRange, 10);
  // For 'all', use a rolling ~365-day window — the maximum history the
  // analytics API allows (~366-day cap in handler_analytics.go). Sending a
  // 1970 epoch sentinel caused HTTP 400 "date range too large" which the
  // catch block rendered as an empty-state error (QA 3.1). Because it is
  // capped, the button is labeled "1y" ("Last 365 days"), not "All" (#52).
  // The axisMinMs anchor-to-earliest-point logic below still applies so
  // sparse data fills the full chart width correctly.
  const windowStartMs = isAllRange ? nowMs - 365 * 86400_000 : nowMs - (days as number) * 86400_000;
  const intervalDays = isAllRange ? 365 : (days as number);
  const interval: 'hourly' | 'daily' | 'weekly' = intervalDays <= 7 ? 'hourly' : intervalDays <= 90 ? 'daily' : 'weekly';

  try {
    // Always forward account_ids AND provider to the chart so its data scope
    // matches the KPI tiles above it. The backend /history/analytics handler
    // accepts a single `account_id`; api.getSavingsAnalytics also sets the
    // singular param for single-account requests (see api/history.ts). The
    // backend now applies the provider chip as a provider WHERE filter on the
    // savings-history query, so forward it to scope the chart (issue #498,
    // QA 2.3).
    const accountIDs = state.getCurrentAccountIDs();
    const provider = state.getCurrentProvider();
    const data = await api.getSavingsAnalytics({
      start: new Date(windowStartMs).toISOString(),
      end: now.toISOString(),
      interval,
      ...(provider ? { provider } : {}),
      ...(accountIDs.length > 0 ? { account_ids: accountIDs } : {}),
    });

    const points = data.data_points ?? [];

    // Determine the left edge of the x-axis.
    // For 'all': anchor to the earliest purchase so the line fills the
    // chart rather than clustering at the right end. Fall back to now-365d
    // when there are no purchases.
    let axisMinMs = windowStartMs;
    if (savingsTrendRange === 'all' && points.length > 0) {
      const earliest = points.reduce(
        (min: number, p: SavingsDataPoint) => Math.min(min, new Date(p.timestamp).getTime()),
        Infinity,
      );
      axisMinMs = earliest;
    }

    // Map each data point to {x: timestamp_ms, y: cumulative_savings} so
    // Chart.js positions it by its real date, not by label index.
    const chartData = points.map((p: SavingsDataPoint) => ({
      x: new Date(p.timestamp).getTime(),
      y: p.cumulative_savings || 0,
    }));

    // Policy (QA 2.3, supersedes QA 3.1 empty-axis approach):
    // - No data + active account filter: show empty-state with the filter
    //   name so the user understands why the chart is blank.
    // - No data + no filter active: show a generic "No purchase history yet"
    //   message rather than blank axes.
    // - Data present: always show the chart (hide empty-state).
    // The stub is also shown on fetch errors (catch block below).
    if (points.length === 0) {
      canvas.classList.add('hidden');
      if (empty) {
        // Both provider and account are forwarded to the backend (see call
        // above), so naming the active filter in the empty-state is accurate
        // (issue #498). buildFilterDesc returns '' when no filter is active.
        const filterDesc = buildFilterDesc(provider, accountIDs);
        empty.textContent = filterDesc
          ? `No savings history for ${filterDesc}.`
          : 'No purchase history yet.';
        empty.classList.remove('hidden');
      }
      if (savingsTrendChart) { savingsTrendChart.destroy(); savingsTrendChart = null; }
      removeChartTable(SAVINGS_TREND_TABLE_ID);
      return;
    }
    // Sparse data (#188): one point would paint a lone dot on a blank canvas,
    // so show "not enough history yet" with the value we have instead.
    if (points.length < MIN_TREND_POINTS) {
      canvas.classList.add('hidden');
      if (empty) {
        empty.textContent = formatSparseTrendMessage(points[0] as SavingsDataPoint);
        empty.classList.remove('hidden');
      }
      if (savingsTrendChart) { savingsTrendChart.destroy(); savingsTrendChart = null; }
      // The sparse message carries the one value; drop any table left from
      // an earlier render, as the empty state does (#744).
      removeChartTable(SAVINGS_TREND_TABLE_ID);
      return;
    }
    canvas.classList.remove('hidden');
    empty?.classList.add('hidden');

    if (savingsTrendChart) savingsTrendChart.destroy();

    // #744: summarize the plotted series and list every point as text.
    const rangeText = isAllRange ? 'the last year' : `the last ${days} days`;
    const latest = chartData[chartData.length - 1]!;
    setChartSummary(canvas,
      `Line chart of cumulative savings over ${rangeText}: ${formatCurrency(latest.y)} `
      + `as of ${formatTrendAxisTick(latest.x, interval)}, ${chartData.length} data point${chartData.length === 1 ? '' : 's'}. `
      + 'Values are in the table below the chart.');
    renderChartTable(canvas, SAVINGS_TREND_TABLE_ID, {
      caption: `Cumulative savings over ${rangeText}`,
      columns: ['Date', 'Cumulative savings'],
      rows: chartData.map(p => [formatTrendAxisTick(p.x, interval), formatCurrency(p.y)]),
    });

    savingsTrendChart = new Chart(canvas, {
      type: 'line',
      data: {
        datasets: [{
          label: 'Cumulative savings',
          data: chartData,
          borderColor: '#1a73e8',
          backgroundColor: 'rgba(26, 115, 232, 0.1)',
          fill: true,
          tension: 0.25,
          pointRadius: 2,
        }],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: { display: false },
          tooltip: {
            callbacks: {
              // Format the x timestamp as a human-readable date so the tooltip
              // header shows a date string rather than the raw 13-digit
              // millisecond value (QA 3.2). Unlike the compact axis ticks it
              // includes the year, so points a year apart don't read the same (#53).
              title: (items) => {
                const raw = items[0]?.raw as { x: number; y: number } | undefined;
                return raw?.x != null ? formatTrendTooltipTitle(raw.x, interval) : '';
              },
              label: (ctx) => `Cumulative savings: ${formatCurrency((ctx.raw as { x: number; y: number }).y)}`,
            },
          },
        },
        scales: {
          x: {
            type: 'linear',
            min: axisMinMs,
            max: nowMs,
            ticks: {
              maxTicksLimit: 6,
              callback: (value) => formatTrendAxisTick(value as number, interval),
            },
          },
          y: {
            beginAtZero: true,
            ticks: { callback: (v) => formatCurrency(v as number) },
          },
        },
      },
    });
  } catch (err) {
    // Analytics is Postgres-backed now, but the endpoint still guards with
    // a 503 when the analytics client isn't wired. Don't break the rest of
    // the dashboard — hide the widget and fall back to a neutral message.
    console.warn('Savings trend chart unavailable:', err);
    if (savingsTrendChart) { savingsTrendChart.destroy(); savingsTrendChart = null; }
    removeChartTable(SAVINGS_TREND_TABLE_ID);
    canvas.classList.add('hidden');
    if (empty) {
      empty.textContent = 'Savings history is not available yet.';
      empty.classList.remove('hidden');
    }
  }
}

let ytdSparklineGeneration = 0;

/**
 * Sparkline for the YTD Savings tile. It used to reuse the trend chart's
 * points, whose window follows the 7d/30d/90d/All toggle, so the line under
 * "YTD" changed shape with a control that does not change the number. It now
 * plots its own series from 1 January (UTC, the same year start the backend
 * uses for ytd_savings) to now, scoped by the same provider/account chips.
 */
export async function loadYtdSparkline(): Promise<void> {
  if (!document.querySelector('.kpi-tile-spark[data-spark-key="ytd"]')) return;
  const generation = ++ytdSparklineGeneration;
  const now = new Date();
  const yearStartMs = Date.UTC(now.getUTCFullYear(), 0, 1);
  const days = (now.getTime() - yearStartMs) / 86400_000;
  const interval: 'hourly' | 'daily' | 'weekly' = days <= 7 ? 'hourly' : days <= 90 ? 'daily' : 'weekly';
  const accountIDs = state.getCurrentAccountIDs();
  const provider = state.getCurrentProvider();
  try {
    const data = await api.getSavingsAnalytics({
      start: new Date(yearStartMs).toISOString(),
      end: now.toISOString(),
      interval,
      ...(provider ? { provider } : {}),
      ...(accountIDs.length > 0 ? { account_ids: accountIDs } : {}),
    });
    if (generation !== ytdSparklineGeneration) return;
    attachSparkline('ytd', (data.data_points ?? []).map((p: SavingsDataPoint) => p.cumulative_savings || 0));
  } catch (err) {
    if (generation !== ytdSparklineGeneration) return;
    console.warn('YTD sparkline unavailable:', err);
    attachSparkline('ytd', []);
  }
}

// Wire up the range-toggle buttons. Called once during dashboard setup.
export function setupSavingsTrendHandlers(): void {
  document.querySelectorAll<HTMLButtonElement>('.trend-range').forEach((btn) => {
    btn.addEventListener('click', () => {
      const range = btn.dataset['range'];
      if (range !== '7' && range !== '30' && range !== '90' && range !== 'all') return;
      savingsTrendRange = range;
      document.querySelectorAll('.trend-range').forEach((b) => b.classList.remove('active'));
      btn.classList.add('active');
      void loadSavingsTrendChart();
    });
  });
}
