/**
 * RI Exchange module for CUDly
 * Manages convertible RI listing, reshape recommendations, and exchange operations
 */

import * as api from './api';
import * as state from './state';
import { formatDate, formatDateTime, escapeHtml, escapeHtmlAttr, formatCurrency } from './utils';
import { switchTab, switchSettingsSubTab } from './navigation';
import { confirmDialog } from './confirmDialog';
import { buildApproveConfirmBody, buildExchangeConfirmBody, hourlyPriceDelta } from './riexchange-confirm';
import { applyColumnFilters as applyColumnFiltersLib } from './lib/column-filters';
import {
  openColumnFilterPopover,
  rebindColumnFilterPopover,
  sortedDistinctValues,
} from './lib/column-filter-popover';
import type {
  ConvertibleRI,
  ExchangeableAzureRI,
  RIUtilization,
  ReshapeRecommendation,
  ExchangeQuoteSummary,
  RIExchangeConfig,
  RIExchangeHistoryRecord,
  OfferingOption,
  TargetOffering,
  ReshapeRecommendationsResponse,
  ExcludedRI,
  Provider,
} from './api';
import { openModal, closeModal } from './modal';
import { showSkeletonRows, showSkeletonText, teardownSkeleton } from './lib/skeleton';
import { statusBadgeHtml } from './lib/status-badge';
import { canAccess } from './permissions';
import { createIcon, iconSvg } from './icons';
import { applyReadOnlySettings, isPermissionDeniedError } from './settings';
import { showToast } from './toast';
import { getCurrentUser } from './state';
import { attachAzureExchangeToolbar, buildAzureSelectCell, buildAzureSelectHeader, canPreviewAzureExchange } from './riexchange-azure';
import { formatDecimalStringCurrency, formatIsoTermShort, formatPaymentOption } from './lib/table-format';

// Module state
let currentRIs: ConvertibleRI[] = [];
// True once currentRIs holds the convertible-RI list of the scope being
// shown. The reshape empty state only quotes currentRIs.length while this is
// set, so it never prints the previous scope's count, or a count from a list
// that failed to load (issue #1135).
let currentRIsLoaded = false;
let currentUtilization: Map<string, RIUtilization> = new Map();
let currentRecommendations: ReshapeRecommendation[] = [];
const exchangePaymentPattern = /^\d+(?:\.\d+)?$/;

// Generation counter to prevent stale utilization data from overwriting fresh data
let utilizationGeneration = 0;

// Generation counter so a superseded loadRIExchange (chip changed while the
// account lookup was in flight) never issues its list call.
let scopeGeneration = 0;

// The AWS account the convertible-RI list is currently scoped to (the
// single-select account chip value, or undefined for all-accounts). Tracked
// at module scope so the asynchronous utilization re-render preserves the
// scoped empty-state copy instead of falling back to the unscoped message
// (issue #871).
let currentRIAccountID: string | undefined;

// Human-readable provider labels for empty-state copy and the not-available
// message. Mirrors the map in inventory.ts.
const PROVIDER_LABELS: Record<string, string> = { aws: 'AWS', azure: 'Azure', gcp: 'GCP' };

function providerLabel(provider: string): string {
  return PROVIDER_LABELS[provider] ?? provider.toUpperCase();
}

// resolveScope reads the Main Header global Provider/Account chips so every
// RI Exchange load is scoped consistently with the other Inventory sub-tabs
// (issue #871). The account chip holds a CloudAccount UUID, but the RI
// endpoints expect the provider account number / subscription GUID
// (external_id), so the single selected account is translated here.
async function resolveScope(): Promise<{ provider: Provider | ''; accountID?: string }> {
  const provider = state.getCurrentProvider();
  const accountIDs = state.getCurrentAccountIDs();
  if (accountIDs.length !== 1) return { provider };
  const accounts = await api.listAccountsMinimal();
  const account = accounts.find(a => a.id === accountIDs[0]);
  if (!account) throw new Error(`Selected account ${accountIDs[0]} was not found`);
  if (!account.external_id) throw new Error(`Selected account ${accountIDs[0]} has no provider account ID`);
  return { provider, accountID: account.external_id };
}

// Mode label mapping — single source of truth
const MODE_LABELS: Record<string, string> = { manual: "Manual Approval", auto: "Fully Automated" };

/**
 * Load the RI Exchange tab — called when tab is activated and whenever the
 * Main Header global Provider/Account filter changes (issue #871).
 *
 * RI Exchange is multi-provider:
 *   - AWS:   convertible RIs + reshape recommendations + exchange history.
 *   - Azure: exchangeable VM reservations + exchange history. Reshape
 *            recommendations are a Cost-Explorer/AWS concept, so that
 *            section shows a provider-aware "not applicable" empty state.
 *   - GCP:   no RI-exchange concept; every section shows a "not available
 *            for GCP" empty state and no list endpoint is called.
 */
export async function loadRIExchange(): Promise<void> {
  const generation = ++scopeGeneration;
  let scope: Awaited<ReturnType<typeof resolveScope>>;
  try {
    scope = await resolveScope();
  } catch (error) {
    if (generation !== scopeGeneration) return;
    utilizationGeneration++;
    currentRIs = [];
    currentRIsLoaded = false;
    currentUtilization = new Map();
    currentRecommendations = [];
    currentRIAccountID = undefined;
    const message = 'Select a valid account to load this section.';
    for (const id of ['ri-exchange-recommendations-list', 'ri-exchange-history-list']) {
      const el = document.getElementById(id);
      if (el) renderEmptyState(el, message);
    }
    renderReshapeStalenessBanner('', null);
    renderExcludedRIsNotice(undefined);
    const container = document.getElementById('ri-exchange-instances-list');
    if (container) {
      container.innerHTML = `<p class="error">Failed to resolve the selected account: ${escapeHtml((error as Error).message)}</p>`;
    }
    return;
  }
  if (generation !== scopeGeneration) return;
  const { provider, accountID } = scope;

  if (provider === 'gcp') {
    renderGCPEmptyStates();
    return;
  }

  if (provider === 'azure') {
    await Promise.all([
      loadExchangeableAzureRIs(accountID, generation),
      loadExchangeHistory(generation),
    ]);
    renderReshapeNotApplicable('azure');
    return;
  }

  // AWS (default / empty provider): full convertible-RI flow.
  const [, recsLoaded] = await Promise.all([
    loadConvertibleRIs(accountID, generation),
    loadReshapeRecommendations(generation),
    loadExchangeHistory(generation),
  ]);
  // The three loads race. When the reshape list came back empty before the
  // RI list, its empty state was written without this scope's RI count, so
  // render it again now that both are in. A failed reshape load keeps its
  // error message (issue #1135).
  if (generation !== scopeGeneration) return;
  if (recsLoaded && currentRecommendations.length === 0) rerenderReshape();
}

/**
 * Render the "not available for GCP" empty state across all three RI Exchange
 * sections. GCP has no Reserved-Instance exchange concept, so we never call a
 * list endpoint and never leave stale AWS/Azure rows behind.
 */
function renderGCPEmptyStates(): void {
  currentRIs = [];
  currentRIsLoaded = false;
  currentUtilization = new Map();
  currentRecommendations = [];
  currentRIAccountID = undefined;

  const instances = document.getElementById('ri-exchange-instances-list');
  if (instances) renderEmptyState(instances, "RI Exchange isn't available for GCP.");

  const recs = document.getElementById('ri-exchange-recommendations-list');
  if (recs) renderEmptyState(recs, "RI Exchange isn't available for GCP.");

  const history = document.getElementById('ri-exchange-history-list');
  if (history) renderEmptyState(history, "RI Exchange isn't available for GCP.");

  renderReshapeStalenessBanner('', null);
  renderExcludedRIsNotice(undefined);
}

/**
 * Render a provider-aware "reshape recommendations not applicable" message.
 * Reshape recommendations are derived from AWS Cost Explorer, so they do not
 * apply to Azure (or any non-AWS) provider.
 */
function renderReshapeNotApplicable(provider: string): void {
  currentRecommendations = [];
  const container = document.getElementById('ri-exchange-recommendations-list');
  if (container) {
    renderEmptyState(container, `Reshape recommendations are not available for ${providerLabel(provider)}.`);
  }
  renderReshapeStalenessBanner('', null);
  renderExcludedRIsNotice(undefined);
}

/**
 * Render a plain `.empty` paragraph via textContent (no innerHTML) so backend
 * or provider-derived strings can never inject markup. Wipes any prior
 * children (skeleton, stale table) for a clean handoff.
 */
function renderEmptyState(container: HTMLElement, message: string): void {
  container.textContent = '';
  const p = document.createElement('p');
  p.className = 'empty';
  p.textContent = message;
  container.appendChild(p);
}

/**
 * True when the RI Exchange sub-tab is the currently visible panel.
 * The sub-tab panel id is "inventory-ri-exchange" (see index.html).
 * Used by the provider/account change subscriptions below to avoid
 * unnecessary fetches while the user is on a different tab.
 */
function isRIExchangeSubtabActive(): boolean {
  const panel = document.getElementById('inventory-ri-exchange');
  return panel !== null && !panel.classList.contains('hidden');
}

/**
 * Setup RI Exchange event handlers.
 *
 * Wires the refresh button, the settings deep-link, and
 * provider/account state subscriptions so the convertible-RI list
 * and reshape recommendations reload when the operator switches the
 * global account filter (issue #186). An active-subtab guard
 * mirrors the Recommendations tab pattern to avoid redundant fetches
 * while the panel is off-screen.
 */
export function setupRIExchangeHandlers(): void {
  // Refresh button. Quote + execute flow lives in the per-row "Exchange"
  // button handlers, which open the exchange modal (openExchangeModal).
  const refreshBtn = document.getElementById('ri-exchange-refresh-btn');
  if (refreshBtn) {
    refreshBtn.addEventListener('click', () => void loadRIExchange());
  }

  const settingsBtn = document.getElementById('ri-exchange-settings-btn');
  if (settingsBtn) {
    settingsBtn.addEventListener('click', () => {
      // switchSettingsSubTab owns the sub-tab DOM, title and history push, so
      // switchTab only has to reveal the Admin panel.
      switchTab('admin', { push: false, skipDefaultLoad: true });
      switchSettingsSubTab('purchasing');
      const target = document.getElementById('ri-exchange-automation-settings');
      if (target) {
        requestAnimationFrame(() => {
          target.scrollIntoView({ behavior: 'smooth', block: 'start' });
        });
      }
    });
  }

  // issue #186 / #871: reload (scoped to the new provider/account) when the
  // global Main Header filter changes so the RI Exchange tables stay
  // consistent with the rest of the Inventory sub-tabs. Coalesce the two
  // events into a single reload (provider change also fires an account change
  // via the topbar-filters.ts clearing logic).
  //
  // RI Exchange is a mutating workflow, so we ALSO close any in-progress
  // exchange modal the moment the filter changes — the source RI it targets
  // may no longer be visible (e.g. provider switched AWS -> Azure), and acting
  // on a now-hidden RI would be confusing and potentially wrong. We close
  // synchronously on the chip event (not inside the coalescing microtask) so
  // the stale selection is torn down even if the reload is skipped because the
  // sub-tab is off-screen.
  let reloadQueued = false;
  const scheduleReload = (): void => {
    closeExchangeModalIfOpen();
    if (!isRIExchangeSubtabActive() || reloadQueued) return;
    reloadQueued = true;
    queueMicrotask(() => {
      reloadQueued = false;
      if (isRIExchangeSubtabActive()) void loadRIExchange();
    });
  };
  state.subscribeProvider(scheduleReload);
  state.subscribeAccount(scheduleReload);
}

/**
 * Close the RI Exchange quote/execute modal if it is currently open. Called
 * on a global filter change so the user can't act on an exchange selection
 * whose source RI may no longer be in scope (issue #871). No-op when the
 * modal is absent or already hidden.
 */
function closeExchangeModalIfOpen(): void {
  const modal = document.getElementById('ri-exchange-modal');
  if (modal && !modal.classList.contains('hidden')) {
    closeModal(modal);
  }
}

// ──────────────────────────────────────────────
// Convertible RIs table
// ──────────────────────────────────────────────

async function loadConvertibleRIs(accountID?: string, generation: number = scopeGeneration): Promise<void> {
  const container = document.getElementById('ri-exchange-instances-list');
  if (!container) return;

  // Issue #344 T3: shimmer skeleton replaces the static "Loading…" text.
  // 3 rows × 8 cols matches the convertible-RI table's column shape
  // (RI ID / Instance Type / AZ / Count / Offering / Expiry /
  // Utilization / Actions — see renderRIsTable); renderRIsTable swaps
  // the children on success for a clean handoff.
  showSkeletonRows(container, 3, 8);

  currentRIAccountID = accountID;
  currentRIsLoaded = false;

  try {
    // issue #871: scope to the selected account so the page honours the
    // Main Header global filter.
    const ris = await api.listConvertibleRIs(accountID);
    if (generation !== scopeGeneration) return;
    currentRIs = ris;
    currentRIsLoaded = true;
    renderRIsTable(container, accountID);
    // Load utilization asynchronously (Cost Explorer is slow)
    utilizationGeneration++;
    void loadUtilization(utilizationGeneration);
  } catch (error) {
    if (generation !== scopeGeneration) return;
    teardownSkeleton(container);
    const err = error as Error;
    container.innerHTML = `<p class="error">Failed to load convertible RIs: ${escapeHtml(err.message)}</p>`;
  }
}

/**
 * Load and render the Azure exchangeable VM reservations (issue #871).
 * Azure reservations have no utilization/reshape pipeline, so this renders a
 * standalone table into the same Active-list container the AWS path uses.
 * The optional accountID is the selected subscription chip; it scopes the
 * capacity-provider registration check on the backend.
 */
async function loadExchangeableAzureRIs(accountID: string | undefined, generation: number): Promise<void> {
  const container = document.getElementById('ri-exchange-instances-list');
  if (!container) return;

  // Azure table has 6 columns: Reservation / SKU / Quantity / Region /
  // Term / Expiry (see renderAzureRIsTable).
  showSkeletonRows(container, 3, 6);

  // AWS-only module state is irrelevant on the Azure path; reset it so a
  // later provider switch back to AWS starts clean and reshape copy that
  // reads currentRIs.length isn't skewed by stale AWS rows.
  currentRIs = [];
  currentRIsLoaded = false;
  currentUtilization = new Map();
  currentRIAccountID = undefined;

  try {
    const reservations = await api.listExchangeableAzureRIs(accountID);
    if (generation !== scopeGeneration) return;
    renderAzureRIsTable(container, reservations, accountID);
  } catch (error) {
    if (generation !== scopeGeneration) return;
    teardownSkeleton(container);
    const err = error as Error;
    container.innerHTML = `<p class="error">Failed to load Azure reservations: ${escapeHtml(err.message)}</p>`;
  }
}

async function loadUtilization(generation: number): Promise<void> {
  try {
    const utilization = await api.getRIUtilization();
    // Discard if a newer load was started while we were waiting
    if (generation !== utilizationGeneration) return;
    currentUtilization = new Map(utilization.map(u => [u.reserved_instance_id, u]));
    // Re-render table with utilization data, preserving the active account
    // scope so a scoped empty-state message isn't lost (issue #871).
    const container = document.getElementById('ri-exchange-instances-list');
    if (container) renderRIsTable(container, currentRIAccountID);
  } catch (error) {
    console.error('Failed to load RI utilization:', error);
  }
}

function renderRIsTable(container: HTMLElement, accountID?: string): void {
  if (!currentRIs || currentRIs.length === 0) {
    // issue #871: name the active scope so the user knows the result is
    // filtered (by the account chip) rather than globally empty.
    const msg = accountID
      ? `No active convertible Reserved Instances for AWS account ${accountID}.`
      : 'No active convertible Reserved Instances found for AWS.';
    renderEmptyState(container, msg);
    return;
  }

  // Issue #365: RI exchanges mutate cloud-provider RI state and are
  // admin-only by default on the backend. Hide the per-row Exchange
  // button for non-admin sessions so readonly users don't get a 403
  // on click. Defense in depth, backend still enforces.
  const canExchange = canAccess('admin', '*');

  // Apply per-column filters (issue #1414). Mirrors the pattern in
  // renderRecommendations; broken numeric expressions are skipped so
  // the popover can surface the inline error without forcing a clear.
  const filters = state.getActiveRiColumnFilters();
  const visibleRIs = applyActiveRiColumnFilters(currentRIs, currentUtilization, filters);

  const filterBtn = (column: state.ActiveRiColumnId): string => {
    const active = filters[column] ? ' active' : '';
    const lbl = activeRiLabelFor(column);
    const label = filters[column] ? `Filter ${lbl} — currently active` : `Filter ${lbl}`;
    return `<button type="button" class="column-filter-btn${active}" data-column="${column}" aria-haspopup="dialog" aria-expanded="false" aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}">${iconSvg('filter')}</button>`;
  };

  container.innerHTML = `
    <table class="table-compact${canExchange ? ' table-sticky-end' : ''}">
      <thead>
        <tr>
          <th>RI ID</th>
          <th>Instance Type${filterBtn('instance_type')}</th>
          <th>AZ${filterBtn('availability_zone')}</th>
          <th class="num">Count${filterBtn('instance_count')}</th>
          <th>Offering${filterBtn('offering_type')}</th>
          <th>Expiry</th>
          <th>Utilization${filterBtn('utilization_pct')}</th>
          ${canExchange ? '<th>Actions</th>' : ''}
        </tr>
      </thead>
      <tbody>
        ${visibleRIs.map(ri => {
          const util = currentUtilization.get(ri.reserved_instance_id);
          const utilPct = util ? util.utilization_percent : null;
          const utilClass = utilPct === null ? '' : utilPct >= 95 ? 'util-green' : utilPct >= 70 ? 'util-yellow' : 'util-red';
          const utilText = utilPct === null ? '<span class="loading-inline">...</span>' : `${utilPct.toFixed(1)}%`;
          return `
          <tr>
            <td class="monospace">${escapeHtml(ri.reserved_instance_id)}</td>
            <td>${escapeHtml(ri.instance_type)}</td>
            <td>${escapeHtml(ri.availability_zone)}</td>
            <td class="num">${ri.instance_count}</td>
            <td>${escapeHtml(formatPaymentOption(ri.offering_type))}</td>
            <td>${formatDate(ri.end)}</td>
            <td class="${utilClass}">${utilText}</td>
            ${canExchange ? `<td><button class="btn-small" data-action="quote-ri" data-ri-id="${escapeHtml(ri.reserved_instance_id)}" data-count="${ri.instance_count}" aria-label="Exchange ${escapeHtml(ri.reserved_instance_id)}">Exchange</button></td>` : ''}
          </tr>`;
        }).join('')}
      </tbody>
    </table>
  `;

  // Wire per-column filter buttons (same pattern as renderRecommendations).
  // e.stopPropagation prevents the surrounding <th> from also handling
  // the click so future sort handlers won't conflict.
  container.querySelectorAll<HTMLButtonElement>('.column-filter-btn').forEach((btn) => {
    const column = btn.dataset['column'] as state.ActiveRiColumnId | undefined;
    if (!column) return;
    btn.addEventListener('click', (e) => {
      e.stopPropagation();
      activeRiOpenPopoverFor(column, btn);
    });
  });
  // Keep an open filter popover attached to the re-rendered trigger.
  rebindColumnFilterPopover(ACTIVE_RI_POPOVER_SCOPE);

  // Attach "Exchange" handlers for individual RIs
  container.querySelectorAll<HTMLButtonElement>('[data-action="quote-ri"]').forEach(btn => {
    btn.addEventListener('click', () => {
      const count = parseInt(btn.dataset['count'] || '1', 10);
      openExchangeModal(btn.dataset['riId'] || '', isNaN(count) ? 1 : count);
    });
  });
}

/**
 * Render the Azure exchangeable-reservations table (issue #871). Built
 * entirely via DOM construction / textContent — never innerHTML — so any
 * Azure-derived field (SKU, display name, region) cannot inject markup.
 *
 * Users who can preview an exchange get a selection column and an
 * "Exchange selected" action that opens the Azure exchange modal
 * (riexchange-azure.ts, issue #81).
 */
function renderAzureRIsTable(
  container: HTMLElement,
  reservations: ExchangeableAzureRI[],
  accountID?: string,
): void {
  if (!reservations || reservations.length === 0) {
    const msg = accountID
      ? `No exchangeable reservations for Azure subscription ${accountID}.`
      : 'No exchangeable reservations found for Azure.';
    renderEmptyState(container, msg);
    return;
  }

  container.textContent = '';
  const table = document.createElement('table');
  const canExchange = canPreviewAzureExchange();
  table.className = canExchange ? 'table-compact table-sticky-start' : 'table-compact';

  const thead = document.createElement('thead');
  const headerRow = document.createElement('tr');
  const labels = ['Reservation', 'SKU', 'Quantity', 'Region', 'Term', 'Expiry'];
  if (canExchange) headerRow.appendChild(buildAzureSelectHeader());
  for (const label of labels) {
    const th = document.createElement('th');
    th.textContent = label;
    if (label === 'Quantity') th.className = 'num';
    headerRow.appendChild(th);
  }
  thead.appendChild(headerRow);
  table.appendChild(thead);

  const tbody = document.createElement('tbody');
  reservations.forEach((r, i) => {
    const tr = buildAzureRIRow(r);
    if (canExchange) tr.prepend(buildAzureSelectCell(r, i));
    tbody.appendChild(tr);
  });
  table.appendChild(tbody);
  container.appendChild(table);
  if (canExchange) attachAzureExchangeToolbar(container, table, reservations, () => { void loadRIExchange(); });
}

function buildAzureRIRow(r: ExchangeableAzureRI): HTMLTableRowElement {
  const tr = document.createElement('tr');

  appendAzureCell(tr, r.display_name || r.reservation_id);
  appendAzureCell(tr, r.sku);
  appendAzureCell(tr, String(r.quantity), true);
  appendAzureCell(tr, r.region || '—');
  appendAzureCell(tr, formatIsoTermShort(r.term) || '—');
  appendAzureCell(tr, r.expiry_date ? formatDate(r.expiry_date) : '—');

  return tr;
}

// numeric right-aligns the cell with tabular digits (td.num, issue #739).
function appendAzureCell(tr: HTMLTableRowElement, text: string, numeric = false): void {
  const td = document.createElement('td');
  td.textContent = text;
  if (numeric) td.className = 'num';
  tr.appendChild(td);
}

// ──────────────────────────────────────────────
// Reshape recommendations
// ──────────────────────────────────────────────

// Resolves true when this call rendered a fresh reshape list (empty or not),
// false when it was superseded, failed, or had nowhere to render.
export async function loadReshapeRecommendations(generation: number = scopeGeneration): Promise<boolean> {
  const container = document.getElementById('ri-exchange-recommendations-list');
  if (!container) return false;

  // Issue #344 T3: skeleton rows for the reshape-recommendations table.
  // 3 rows × 8 cols matches the rendered table shape — see
  // renderRecommendations: Source RI / Current / Suggested /
  // Alternatives / Utilization / Normalized Units / Reason / Actions.
  showSkeletonRows(container, 3, 8);

  try {
    const resp: ReshapeRecommendationsResponse = await api.getReshapeRecommendations();
    if (generation !== scopeGeneration) return false;
    currentRecommendations = resp.recommendations ?? [];
    renderRecommendations(container);
    renderReshapeStalenessBanner(resp.recs_staleness, resp.recs_collected_at);
    renderExcludedRIsNotice(resp.excluded_ris);
    return true;
  } catch (error) {
    if (generation !== scopeGeneration) return false;
    teardownSkeleton(container);
    renderExcludedRIsNotice(undefined);
    const err = error as Error;
    container.innerHTML = `<p class="error">Failed to load recommendations: ${escapeHtml(err.message)}</p>`;
    return false;
  }
}

/**
 * Render (or clear) the notice listing RIs the backend left out of the
 * reshape analysis. Sits in a sibling slot before the recommendations list;
 * ids are inserted as text, never HTML.
 */
export function renderExcludedRIsNotice(excluded: ExcludedRI[] | undefined): void {
  const NOTICE_ID = 'ri-exchange-recommendations-excluded';
  let notice = document.getElementById(NOTICE_ID);
  if (!notice) {
    const listEl = document.getElementById('ri-exchange-recommendations-list');
    if (!listEl || !listEl.parentElement) return;
    notice = document.createElement('div');
    notice.id = NOTICE_ID;
    listEl.parentElement.insertBefore(notice, listEl);
  }
  if (!excluded || excluded.length === 0) {
    notice.textContent = '';
    notice.className = '';
    notice.removeAttribute('role');
    return;
  }
  const byReason = new Map<string, string[]>();
  for (const e of excluded) {
    const reason = e.reason || 'AWS returned no term length';
    const ids = byReason.get(reason);
    if (ids) ids.push(e.ri_id);
    else byReason.set(reason, [e.ri_id]);
  }
  // textContent keeps server-provided reasons and ids inert. The server
  // writes reasons as sentences ("Expires in 35 days; ... the term."), so
  // the trailing full stop and a leading capital are dropped to read
  // inside "... left out because <reason>: <ids>" (it used to end ".:").
  const lines = [...byReason]
    .map(([reason, ids]) => `${ids.length} convertible RI${ids.length === 1 ? ' was' : 's were'} left out because ${inlineReason(reason)}: ${ids.join(', ')}`);
  renderTableCallout(notice, 'warning', lines);
}

/** "Expires in 35 days; extend the term." -> "expires in 35 days; extend the term". */
function inlineReason(reason: string): string {
  const trimmed = reason.trim().replace(/[.\s]+$/, '');
  // Lower-case a capitalised first word, but leave acronyms ("AWS") alone.
  return /^[A-Z][a-z]/.test(trimmed) ? trimmed[0]!.toLowerCase() + trimmed.slice(1) : trimmed;
}

/**
 * Fill `el` as a table callout: an alert icon and a muted body, one line
 * per entry. Used for the reshape staleness and excluded-RI notices, which
 * used to be bold run-on paragraphs with a "!" prefix.
 */
function renderTableCallout(el: HTMLElement, tone: 'warning' | 'error', lines: string[]): void {
  el.className = `freshness-banner ${tone} table-callout`;
  el.setAttribute('role', 'note');
  el.textContent = '';
  el.appendChild(createIcon('alert-triangle'));
  const body = document.createElement('div');
  body.className = 'table-callout-body';
  for (const line of lines) {
    const p = document.createElement('p');
    p.textContent = line;
    body.appendChild(p);
  }
  el.appendChild(body);
}

/**
 * Render (or clear) the staleness banner above the reshape-recommendations
 * table. The banner slot is a sibling element with id
 * "ri-exchange-recommendations-freshness"; it is created here if absent.
 *
 * staleness: "" or undefined clears any existing banner (fresh data).
 * "soft"  : soft-warning copy ("data may be up to 12 h old").
 * "hard"  : hard-warning copy ("data is more than 24 h old").
 */
export function renderReshapeStalenessBanner(
  staleness: string | undefined,
  collectedAt: string | null | undefined,
): void {
  const BANNER_ID = 'ri-exchange-recommendations-freshness';
  // Locate or create the banner slot. It lives immediately before
  // ri-exchange-recommendations-list in the DOM.
  let banner = document.getElementById(BANNER_ID);
  if (!banner) {
    const listEl = document.getElementById('ri-exchange-recommendations-list');
    if (!listEl || !listEl.parentElement) return;
    banner = document.createElement('div');
    banner.id = BANNER_ID;
    listEl.parentElement.insertBefore(banner, listEl);
  }

  if (!staleness) {
    banner.textContent = '';
    banner.className = '';
    banner.removeAttribute('role');
    return;
  }

  // Build the age label from collectedAt when available.
  let ageLabel = '';
  if (collectedAt) {
    const ms = Date.now() - new Date(collectedAt).getTime();
    const hours = Math.floor(ms / (1000 * 60 * 60));
    const mins = Math.floor((ms % (1000 * 60 * 60)) / (1000 * 60));
    ageLabel = hours > 0 ? ` (last collected ${hours}h${mins > 0 ? ` ${mins}m` : ''} ago)` : ` (last collected ${mins}m ago)`;
  }

  const isSoft = staleness === 'soft';
  const copy = isSoft
    ? `Cross-family alternatives are based on Cost Explorer recommendations that may be up to 24h old${ageLabel}. Some prices may be stale.`
    : `Cross-family alternatives are based on Cost Explorer recommendations older than 24h${ageLabel}. Prices may be significantly out of date.`;
  renderTableCallout(banner, isSoft ? 'warning' : 'error', [copy]);
}

// ──────────────────────────────────────────────
// Column filters (issue #166 follow-up to merged #570)
//
// Wires inline column filters to the reshape-recommendations table via the
// shared lib/column-filters helpers. Categorical columns (Source RI, Current,
// Suggested, Reason) get a checkbox-list popover; numeric columns (Count,
// Utilization, Normalized Used/Purchased) get a free-text expression popover.
// Display-rounded numeric values match what the cell renders so a typed
// value like "95.0" matches the visible utilization figure.
// ──────────────────────────────────────────────

interface RiExchangeColumnDef {
  key: state.RiExchangeColumnId;
  label: string;
  kind: 'numeric' | 'categorical';
}

const RIEX_COLUMN_DEFS: readonly RiExchangeColumnDef[] = [
  { key: 'source_ri_id',          label: 'Source RI',          kind: 'categorical' },
  { key: 'source_count',          label: 'Source count',       kind: 'numeric'     },
  { key: 'source_instance_type',  label: 'Source instance',    kind: 'categorical' },
  { key: 'target_count',          label: 'Target count',       kind: 'numeric'     },
  { key: 'target_instance_type',  label: 'Target instance',    kind: 'categorical' },
  { key: 'utilization_percent',   label: 'Utilization %',      kind: 'numeric'     },
  { key: 'normalized_used',       label: 'Normalized used',    kind: 'numeric'     },
  { key: 'normalized_purchased',  label: 'Normalized purchased', kind: 'numeric'   },
  { key: 'reason',                label: 'Reason',             kind: 'categorical' },
];

const RIEX_NUMERIC_COLUMNS: ReadonlySet<state.RiExchangeColumnId> = new Set(
  RIEX_COLUMN_DEFS.filter((c) => c.kind === 'numeric').map((c) => c.key),
);

function riexCategoricalCellValue(
  r: import('./api').ReshapeRecommendation,
  col: state.RiExchangeColumnId,
): string {
  switch (col) {
    case 'source_ri_id':         return r.source_ri_id ?? '';
    case 'source_instance_type': return r.source_instance_type ?? '';
    case 'target_instance_type': return r.target_instance_type ?? '';
    case 'reason':               return r.reason ?? '';
    // Numeric columns never reach this branch — return '' so the type matches.
    case 'source_count':
    case 'target_count':
    case 'utilization_percent':
    case 'normalized_used':
    case 'normalized_purchased': return '';
  }
}

function riexNumericCellValue(
  r: import('./api').ReshapeRecommendation,
  col: state.RiExchangeColumnId,
): number {
  switch (col) {
    case 'source_count':         return r.source_count ?? 0;
    case 'target_count':         return r.target_count ?? 0;
    case 'utilization_percent':  return r.utilization_percent ?? Number.NaN;
    case 'normalized_used':      return r.normalized_used ?? Number.NaN;
    case 'normalized_purchased': return r.normalized_purchased ?? Number.NaN;
    // Categorical columns never reach this branch — NaN fails every predicate.
    case 'source_ri_id':
    case 'source_instance_type':
    case 'target_instance_type':
    case 'reason':               return Number.NaN;
  }
}

// Decimal places the cell renders with — mirrors the toFixed() calls below
// in the row markup so a user typing the displayed value matches the
// rounded cell value. Counts render as integers; utilization and
// normalized units render with one decimal place.
function riexDisplayPrecision(col: state.RiExchangeColumnId): number {
  switch (col) {
    case 'source_count':
    case 'target_count':
      return 0;
    case 'utilization_percent':
    case 'normalized_used':
    case 'normalized_purchased':
      return 1;
    case 'source_ri_id':
    case 'source_instance_type':
    case 'target_instance_type':
    case 'reason':
      return 0;
  }
}

function riexRoundForDisplay(n: number, precision: number): number {
  if (!Number.isFinite(n)) return n;
  return Number(n.toFixed(precision));
}

export function applyRiExchangeColumnFilters(
  recs: readonly import('./api').ReshapeRecommendation[],
  filters: state.RiExchangeColumnFilters,
): import('./api').ReshapeRecommendation[] {
  return applyColumnFiltersLib<
    import('./api').ReshapeRecommendation,
    state.RiExchangeColumnId
  >(recs, filters, {
    categorical: riexCategoricalCellValue,
    numeric: (r, col) => riexRoundForDisplay(riexNumericCellValue(r, col), riexDisplayPrecision(col)),
  });
}

// ── Active Convertible RIs column filters (issue #1414) ──────────────────

interface ActiveRiColumnDef {
  key: state.ActiveRiColumnId;
  label: string;
  kind: 'numeric' | 'categorical';
}

const ACTIVE_RI_COLUMN_DEFS: readonly ActiveRiColumnDef[] = [
  { key: 'instance_type',     label: 'Instance Type', kind: 'categorical' },
  { key: 'availability_zone', label: 'AZ',            kind: 'categorical' },
  { key: 'offering_type',     label: 'Offering',      kind: 'categorical' },
  { key: 'instance_count',    label: 'Count',         kind: 'numeric'     },
  { key: 'utilization_pct',   label: 'Utilization %', kind: 'numeric'     },
];

const ACTIVE_RI_NUMERIC_COLUMNS: ReadonlySet<state.ActiveRiColumnId> = new Set(
  ACTIVE_RI_COLUMN_DEFS.filter((c) => c.kind === 'numeric').map((c) => c.key),
);

function activeRiLabelFor(col: state.ActiveRiColumnId): string {
  return ACTIVE_RI_COLUMN_DEFS.find((c) => c.key === col)?.label ?? col;
}

function activeRiCategoricalCellValue(ri: ConvertibleRI, col: state.ActiveRiColumnId): string {
  switch (col) {
    case 'instance_type':     return ri.instance_type ?? '';
    case 'availability_zone': return ri.availability_zone ?? '';
    case 'offering_type':     return ri.offering_type ?? '';
    case 'instance_count':
    case 'utilization_pct':   return '';
  }
}

function activeRiNumericCellValue(
  ri: ConvertibleRI,
  util: Map<string, RIUtilization>,
  col: state.ActiveRiColumnId,
): number {
  switch (col) {
    case 'instance_count':  return ri.instance_count ?? 0;
    case 'utilization_pct': {
      const u = util.get(ri.reserved_instance_id);
      return u !== undefined ? u.utilization_percent : Number.NaN;
    }
    case 'instance_type':
    case 'availability_zone':
    case 'offering_type':   return Number.NaN;
  }
}

function activeRiDisplayPrecision(col: state.ActiveRiColumnId): number {
  switch (col) {
    case 'instance_count':  return 0;
    case 'utilization_pct': return 1;
    case 'instance_type':
    case 'availability_zone':
    case 'offering_type':   return 0;
  }
}

/**
 * Pure filter function for the Active Convertible RIs table. Exported so
 * unit tests can exercise the filter pipeline without DOM setup. Mirrors
 * applyRiExchangeColumnFilters.
 */
export function applyActiveRiColumnFilters(
  ris: readonly ConvertibleRI[],
  util: Map<string, RIUtilization>,
  filters: state.ActiveRiColumnFilters,
): ConvertibleRI[] {
  return applyColumnFiltersLib<ConvertibleRI, state.ActiveRiColumnId>(ris, filters, {
    categorical: activeRiCategoricalCellValue,
    numeric: (ri, col) => {
      const raw = activeRiNumericCellValue(ri, util, col);
      if (!Number.isFinite(raw)) return raw;
      return Number(raw.toFixed(activeRiDisplayPrecision(col)));
    },
  });
}

// ── Active RI popover ─────────────────────────────────────────────────────
// Lifecycle lives in lib/column-filter-popover.ts (issue #18).

const ACTIVE_RI_POPOVER_SCOPE = 'active-ri';

function rerenderRIs(): void {
  const container = document.getElementById('ri-exchange-instances-list');
  if (container) renderRIsTable(container, currentRIAccountID);
}

function activeRiTrigger(column: state.ActiveRiColumnId): HTMLButtonElement | null {
  return document.querySelector<HTMLButtonElement>(
    `#ri-exchange-instances-list .column-filter-btn[data-column="${column}"]`,
  );
}

function activeRiOpenPopoverFor(column: state.ActiveRiColumnId, anchor: HTMLElement): void {
  openColumnFilterPopover({
    scope: ACTIVE_RI_POPOVER_SCOPE,
    column,
    label: activeRiLabelFor(column),
    kind: ACTIVE_RI_NUMERIC_COLUMNS.has(column) ? 'numeric' : 'categorical',
    placeholder: 'e.g. >50, 10..20, 4',
    distinctValues: sortedDistinctValues(currentRIs, (ri) => activeRiCategoricalCellValue(ri, column)),
    getFilter: () => state.getActiveRiColumnFilters()[column],
    onCommit: (filter) => {
      state.setActiveRiColumnFilter(column, filter);
      rerenderRIs();
    },
    findTrigger: () => activeRiTrigger(column),
  }, anchor);
}

// ── Reshape recommendations popover ──────────────────────────────────────

const RIEX_POPOVER_SCOPE = 'riex';

function riexLabelFor(col: state.RiExchangeColumnId): string {
  return RIEX_COLUMN_DEFS.find((c) => c.key === col)?.label ?? col;
}

function riexTrigger(column: state.RiExchangeColumnId): HTMLButtonElement | null {
  return document.querySelector<HTMLButtonElement>(
    `#ri-exchange-recommendations-list .column-filter-btn[data-column="${column}"]`,
  );
}

function riexOpenPopoverFor(column: state.RiExchangeColumnId, anchor: HTMLElement): void {
  openColumnFilterPopover({
    scope: RIEX_POPOVER_SCOPE,
    column,
    label: riexLabelFor(column),
    kind: RIEX_NUMERIC_COLUMNS.has(column) ? 'numeric' : 'categorical',
    placeholder: 'e.g. >50, 70..95, 1',
    distinctValues: sortedDistinctValues(currentRecommendations, (r) => riexCategoricalCellValue(r, column)),
    getFilter: () => state.getRiExchangeColumnFilters()[column],
    onCommit: (filter) => {
      state.setRiExchangeColumnFilter(column, filter);
      rerenderReshape();
    },
    findTrigger: () => riexTrigger(column),
  }, anchor);
}

function rerenderReshape(): void {
  const container = document.getElementById('ri-exchange-recommendations-list');
  if (container) renderRecommendations(container);
}

function renderRecommendations(container: HTMLElement): void {
  if (!currentRecommendations || currentRecommendations.length === 0) {
    // The "well-utilized" copy is only truthful when the RI fleet actually
    // contains convertibles to be utilized. Prior to this commit the same
    // message fired regardless of fleet presence, so a brand-new tenant saw
    // a claim about utilization of a non-existent fleet.
    // Until this scope's RI list is in (or when it failed to load), say
    // nothing about the fleet rather than quote a stale count (#1135).
    const copy = !currentRIsLoaded
      ? 'No reshape recommendations right now.'
      : currentRIs.length === 0
        ? 'Reshape recommendations appear once your accounts have active convertible Reserved Instances — none are registered yet.'
        : `All ${currentRIs.length} convertible RI${currentRIs.length === 1 ? '' : 's'} meet your utilization threshold. No reshape needed.`;
    container.innerHTML = `<p class="empty">${escapeHtml(copy)}</p>`;
    return;
  }

  // Issue #365: same admin-only gate as the convertible-RI table.
  const canExchange = canAccess('admin', '*');

  // Apply per-column filters before rendering. Each filter is ANDed with
  // the others; broken numeric expressions are skipped so the popover can
  // surface the error without forcing the user to clear the field first.
  const filters = state.getRiExchangeColumnFilters();
  const visible = applyRiExchangeColumnFilters(currentRecommendations, filters);

  const filterBtn = (column: state.RiExchangeColumnId): string => {
    const active = filters[column] ? ' active' : '';
    const lbl = riexLabelFor(column);
    const label = filters[column] ? `Filter ${lbl} — currently active` : `Filter ${lbl}`;
    return `<button type="button" class="column-filter-btn${active}" data-column="${column}" aria-haspopup="dialog" aria-expanded="false" aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}">${iconSvg('filter')}</button>`;
  };
  // Columns that pack two values ("4x m5.large", "26.6 / 64.0") have one
  // filter per value. Two bare funnels side by side gave no hint which was
  // which, so each one is named under the header label. The visible word is
  // part of the button's accessible name ("Filter Source count").
  const namedFilterBtn = (column: state.RiExchangeColumnId, name: string): string =>
    filterBtn(column).replace('</button>', `<span class="column-filter-btn-name">${name}</span></button>`);
  const filterGroup = (...btns: string[]): string => `<span class="th-filter-group">${btns.join('')}</span>`;

  // Render an indexable list so per-row Exchange buttons can find the
  // matching original recommendation (idx into currentRecommendations).
  // Index the full list once instead of an indexOf scan per visible row,
  // which made the row build quadratic (#319). The first index wins, as
  // with indexOf.
  const indexByRec = new Map<ReshapeRecommendation, number>();
  currentRecommendations.forEach((rec, i) => {
    if (!indexByRec.has(rec)) indexByRec.set(rec, i);
  });
  const visibleWithIdx = visible.map((rec) => ({
    rec,
    idx: indexByRec.get(rec) ?? -1,
  }));

  container.innerHTML = `
    <table class="table-compact${canExchange ? ' table-sticky-end' : ''}">
      <thead>
        <tr>
          <th>Source RI${filterBtn('source_ri_id')}</th>
          <th>Current${filterGroup(namedFilterBtn('source_count', 'count'), namedFilterBtn('source_instance_type', 'instance'))}</th>
          <th>Suggested${filterGroup(namedFilterBtn('target_count', 'count'), namedFilterBtn('target_instance_type', 'instance'))}</th>
          <th>Alternatives</th>
          <th>Utilization${filterBtn('utilization_percent')}</th>
          <th>Normalized Units${filterGroup(namedFilterBtn('normalized_used', 'used'), namedFilterBtn('normalized_purchased', 'purchased'))}</th>
          <th>Reason${filterBtn('reason')}</th>
          ${canExchange ? '<th>Actions</th>' : ''}
        </tr>
      </thead>
      <tbody>
        ${visibleWithIdx.map(({ rec, idx }) => {
          // The API can omit utilization_percent (the filter extractor
          // already maps it to NaN). Unknown renders as a neutral em-dash:
          // calling toFixed on it threw and left the panel on its skeleton,
          // and undefined >= 70 is false, which painted unknown as red.
          const utilPct = typeof rec.utilization_percent === 'number' && Number.isFinite(rec.utilization_percent)
            ? rec.utilization_percent
            : null;
          const utilClass = utilPct === null ? '' : utilPct >= 95 ? 'util-green' : utilPct >= 70 ? 'util-yellow' : 'util-red';
          const altCell = renderAlternativesCell(rec.alternative_targets);
          return `
          <tr>
            <td class="monospace"><span class="cell-truncate cell-truncate--xs" title="${escapeHtml(rec.source_ri_id)}">${escapeHtml(rec.source_ri_id)}</span></td>
            <td>${rec.source_count}x ${escapeHtml(rec.source_instance_type)}</td>
            <td>${renderSuggestedCell(rec)}</td>
            <td class="cell-wrap">${altCell}</td>
            <td class="${utilClass}">${utilPct === null ? '—' : `${utilPct.toFixed(1)}%`}</td>
            <td>${rec.normalized_used.toFixed(1)} / ${rec.normalized_purchased.toFixed(1)}</td>
            <td class="cell-wrap">${escapeHtml(rec.reason)}</td>
            ${canExchange ? `<td>${renderReshapeExchangeButton(rec, idx)}</td>` : ''}
          </tr>`;
        }).join('')}
      </tbody>
    </table>
  `;

  // Wire per-column filter buttons.  e.stopPropagation prevents the
  // surrounding <th> from also handling the click (no sort handler today,
  // but matches the pattern in recommendations.ts so future <th>-level
  // handlers won't conflict).
  container.querySelectorAll<HTMLButtonElement>('.column-filter-btn').forEach((btn) => {
    const column = btn.dataset['column'] as state.RiExchangeColumnId | undefined;
    if (!column) return;
    btn.addEventListener('click', (e) => {
      e.stopPropagation();
      riexOpenPopoverFor(column, btn);
    });
  });
  // Keep an open filter popover attached to the re-rendered trigger.
  rebindColumnFilterPopover(RIEX_POPOVER_SCOPE);

  // Attach "Exchange" handlers. Same as renderRIsTable: for non-admin
  // sessions the selector matches zero elements and this loop is a no-op.
  container.querySelectorAll<HTMLButtonElement>('[data-action="fill-quote"]').forEach(btn => {
    btn.addEventListener('click', () => {
      const idx = parseInt(btn.dataset['index'] || '0', 10);
      const rec = currentRecommendations[idx];
      if (rec) fillQuoteFromRecommendation(rec);
    });
  });
}

// currencyPrefix is the prefix formatCurrency puts before an amount in
// `code`. USD keeps the app-wide "$"; any other ISO-4217 code is shown
// as "EUR " so a non-USD price is never read as dollars (issue #284). An
// absent code keeps "$": exchanges are USD-only end to end (the backend
// refuses non-USD quotes), so a missing code is a USD offering.
export function currencyPrefix(code: string | undefined): string {
  const c = code?.trim().toUpperCase();
  return !c || c === 'USD' ? '$' : `${c} `;
}

// formatOfferingCost renders an offering's monthly cost in its own currency.
function formatOfferingCost(alt: OfferingOption, cost: number = alt.effective_monthly_cost): string {
  return formatCurrency(cost, currencyPrefix(alt.currency_code), 2);
}

// renderAlternativesCell formats the cross-family alternative targets
// as a comma-separated list of "<instance_type> $X.XX/mo" chips inside
// a single table cell. Each instance_type is escapeHtml'd; cost values
// go through the shared formatCurrency helper (digits: 2) and the
// currency prefix is escaped too, so there's no HTML injection vector.
function renderAlternativesCell(alternatives: OfferingOption[] | undefined): string {
  if (!alternatives || alternatives.length === 0) {
    return '—';
  }
  return alternatives
    .map((alt) => `<span class="cost-chip">${escapeHtml(alt.instance_type)} ${escapeHtml(formatOfferingCost(alt))}/mo</span>`)
    .join(' ');
}

// hasReshapeTarget reports whether a reshape recommendation names a primary
// exchange target. The analyzer emits target_instance_type "" and
// target_count 0 when it has none, for example for a completely unused RI
// or one whose target would exceed the units the account owns (#1410).
function hasReshapeTarget(rec: ReshapeRecommendation): boolean {
  return Boolean(rec.target_instance_type) && rec.target_count > 0;
}

// canExchangeReshape reports whether the row's Exchange button has anything
// to offer: a primary target, or cross-family alternatives the modal's picker
// can preselect from.
function canExchangeReshape(rec: ReshapeRecommendation): boolean {
  return hasReshapeTarget(rec) || (rec.alternative_targets?.length ?? 0) > 0;
}

function renderSuggestedCell(rec: ReshapeRecommendation): string {
  if (!hasReshapeTarget(rec)) {
    return '<span class="text-muted">No exchange target</span>';
  }
  return `${rec.target_count}x ${escapeHtml(rec.target_instance_type)}`;
}

// renderReshapeExchangeButton renders the row's Exchange action. A row with
// nothing to exchange into gets a disabled button with no data-action, so no
// click handler is attached and the reason is still discoverable on hover.
function renderReshapeExchangeButton(rec: ReshapeRecommendation, idx: number): string {
  if (!canExchangeReshape(rec)) {
    const why = 'No exchange target available for this RI';
    return `<button type="button" class="btn-small" disabled title="${why}" aria-label="Exchange: ${why}">Exchange</button>`;
  }
  return `<button type="button" class="btn-small" data-action="fill-quote" data-index="${idx}">Exchange</button>`;
}

function fillQuoteFromRecommendation(rec: ReshapeRecommendation): void {
  // With no primary target the modal opens on an empty target row for the
  // alternatives; a count of 0 would only fail validation.
  const count = hasReshapeTarget(rec) ? rec.target_count : 1;
  openExchangeModal(rec.source_ri_id, count, rec.target_instance_type, rec.alternative_targets);
}

export function fillQuoteFromRI(riId: string, count: number): void {
  openExchangeModal(riId, count);
}

// ──────────────────────────────────────────────
// RI Exchange Modal
// ──────────────────────────────────────────────

// TargetRow captures the DOM inputs for one target-offering entry in
// the multi-target modal. Tests assert on the posted request shape by
// finding inputs through their class attributes.
//
// offeringInput is a hidden <input type="hidden"> that holds the resolved
// AWS ReservedInstancesOfferingId UUID. The visible picker
// (modal-exchange-target-select) drives this field on change so the
// submission path always sees a UUID and never a free-text instance type.
interface TargetRow {
  offeringInput: HTMLInputElement;  // hidden; holds the UUID
  pickerSelect: HTMLSelectElement;  // visible; drives offeringInput
  countInput: HTMLInputElement;
  chipEl: HTMLSpanElement; // cost chip; shows "$X.XX/mo each" or "—".
  rowEl: HTMLDivElement;
}

export function openExchangeModal(riId: string, count: number, suggestedTargetType?: string, alternativeTargets?: OfferingOption[]): void {
  const modalEl = document.getElementById('ri-exchange-modal');
  if (!modalEl) return;
  const modal = modalEl; // non-null const for use in closures

  const content = modal.querySelector('.modal-content');
  if (!content) return;

  // Scoped state for this modal session. Stored against the modal
  // across quote/execute so execute can re-post the same shape the
  // user saw quoted.
  type QuoteReqShape = {
    ri_ids: string[];
    targets?: Array<{ offering_id: string; count: number }>;
    target_offering_id?: string;
    target_count?: number;
  };
  let modalQuote: ExchangeQuoteSummary | null = null;
  let modalQuoteReq: QuoteReqShape | null = null;
  let quoteRevision = 0;
  let sessionOpen = true;

  // Build header
  const h3 = document.createElement('h3');
  h3.textContent = 'RI Exchange';
  content.textContent = '';
  content.appendChild(h3);

  // RI ID display
  const riRow = document.createElement('div');
  riRow.className = 'setting-row';
  const riLabel = document.createElement('label');
  riLabel.textContent = 'RI ID: ';
  const riSpan = document.createElement('span');
  riSpan.className = 'monospace';
  riSpan.textContent = riId;
  riLabel.appendChild(riSpan);
  riRow.appendChild(riLabel);
  content.appendChild(riRow);

  // Targets container: one or more rows, each with offering picker +
  // count. Users click "+ Add target" to split a source RI across
  // multiple target shapes in a single atomic AWS exchange.
  const targetsContainer = document.createElement('div');
  targetsContainer.id = 'modal-exchange-targets';
  targetsContainer.className = 'exchange-targets-container';
  content.appendChild(targetsContainer);

  const targetRows: TargetRow[] = [];

  // awsOfferings holds the list loaded from the target-offerings endpoint.
  // Starts empty; populateAwsOfferings() fills it once after the modal opens.
  let awsOfferings: TargetOffering[] = [];
  // offeringsLoaded tracks whether the async load has completed so new
  // rows added after completion are seeded with the already-loaded list.
  let offeringsLoaded = false;
  let offeringsError = false;

  // buildPickerOptions rebuilds the <select> options for all existing
  // rows. Called once after the AWS offerings are loaded, and for any
  // row added after that point.
  const buildPickerOptions = (
    select: HTMLSelectElement,
    initialOfferingId?: string,
  ): void => {
    // Remove all existing options to avoid duplicate listeners.
    select.innerHTML = '';

    const placeholder = document.createElement('option');
    placeholder.value = '';
    if (offeringsError) {
      // The AWS list failed; CE recommendations (if any) stay selectable,
      // and the Retry button below the targets reloads the AWS list.
      placeholder.textContent = alternativeTargets && alternativeTargets.length > 0
        ? 'AWS offerings unavailable -- pick a recommendation or retry'
        : 'Could not load offerings';
    } else if (!offeringsLoaded) {
      placeholder.textContent = 'Loading offerings...';
    } else {
      placeholder.textContent = 'Select a target offering';
    }
    select.appendChild(placeholder);

    // Group 1: AWS-driven target offerings from DescribeReservedInstancesOfferings
    if (awsOfferings.length > 0) {
      const grp = document.createElement('optgroup');
      grp.label = 'AWS Target Offerings';
      for (const o of awsOfferings) {
        const opt = document.createElement('option');
        opt.value = o.offering_id;
        // Display: "m5.large -- No Upfront" (no HTML injection: all fields from API)
        // textContent escapes on its own; pre-escaping would show "&amp;".
        opt.textContent = o.instance_type + (o.offering_type ? ' -- ' + o.offering_type : '');
        if (initialOfferingId && o.offering_id === initialOfferingId) {
          opt.selected = true;
        }
        grp.appendChild(opt);
      }
      select.appendChild(grp);
    }

    // Group 2: CE recommendations (alternativeTargets from reshape recs)
    if (alternativeTargets && alternativeTargets.length > 0) {
      const grp = document.createElement('optgroup');
      grp.label = 'CE Recommendations';
      for (const alt of alternativeTargets) {
        const opt = document.createElement('option');
        opt.value = alt.offering_id;
        const costStr = formatOfferingCost(alt);
        opt.textContent = alt.instance_type + ' -- ' + costStr + '/mo';
        if (initialOfferingId && alt.offering_id === initialOfferingId) {
          opt.selected = true;
        }
        grp.appendChild(opt);
      }
      select.appendChild(grp);
    }
  };

  const refreshPickers = (): void => {
    for (const row of targetRows) {
      buildPickerOptions(row.pickerSelect, row.offeringInput.value || undefined);
    }
  };

  // populateAwsOfferings loads target offerings from the backend and
  // refreshes all existing pickers. On failure it shows an error with a
  // Retry button instead of leaving the user at a dead end (issue #740).
  const populateAwsOfferings = async (): Promise<void> => {
    offeringsLoaded = false;
    offeringsError = false;
    offeringsErrorRow.classList.add('hidden');
    refreshPickers();
    try {
      awsOfferings = await api.listTargetOfferings(riId);
    } catch (error) {
      awsOfferings = [];
      offeringsError = true;
      const reason = error instanceof Error && error.message ? `: ${error.message}` : '.';
      offeringsErrorText.textContent = `Could not load target offerings${reason}`;
      offeringsErrorRow.classList.remove('hidden');
    }
    offeringsLoaded = true;
    refreshPickers();
  };

  // Error state for a failed offerings load: the message is an alert so
  // screen readers hear it, and Retry reloads the list in place.
  const offeringsErrorRow = document.createElement('div');
  offeringsErrorRow.className = 'setting-row exchange-offerings-error hidden';
  const offeringsErrorText = document.createElement('p');
  offeringsErrorText.className = 'error';
  offeringsErrorText.setAttribute('role', 'alert');
  const retryOfferingsBtn = document.createElement('button');
  retryOfferingsBtn.type = 'button';
  retryOfferingsBtn.className = 'btn';
  retryOfferingsBtn.textContent = 'Retry';
  retryOfferingsBtn.addEventListener('click', () => {
    void populateAwsOfferings();
  });
  offeringsErrorRow.append(offeringsErrorText, retryOfferingsBtn);
  content.appendChild(offeringsErrorRow);

  const addTargetRow = (initialOfferingId?: string, initialCount?: number): void => {
    const rowIndex = targetRows.length;
    const rowEl = document.createElement('div');
    rowEl.className = 'setting-row exchange-target-row';
    rowEl.dataset.rowIndex = String(rowIndex);

    // Hidden input holds the resolved offering-id UUID. The picker
    // select drives this field; collectTargets reads from it. Tests can
    // set this directly to inject a UUID without going through the UI.
    const offeringInput = document.createElement('input');
    offeringInput.type = 'hidden';
    offeringInput.className = 'modal-exchange-target';
    if (initialOfferingId) offeringInput.value = initialOfferingId;
    rowEl.appendChild(offeringInput);

    // Visible picker: a <select> with two optgroups (AWS + CE recs).
    const pickerLabel = document.createElement('label');
    pickerLabel.textContent = 'Target offering: ';
    const pickerSelect = document.createElement('select');
    pickerSelect.className = 'modal-exchange-target-select';
    buildPickerOptions(pickerSelect, initialOfferingId);
    pickerLabel.appendChild(pickerSelect);
    rowEl.appendChild(pickerLabel);

    const countLabel = document.createElement('label');
    countLabel.textContent = 'Count: ';
    const countInput = document.createElement('input');
    countInput.type = 'number';
    countInput.min = '1';
    countInput.value = String(initialCount ?? 1);
    countInput.className = 'modal-exchange-count';
    countLabel.appendChild(countInput);
    rowEl.appendChild(countLabel);

    const removeBtn = document.createElement('button');
    removeBtn.type = 'button';
    removeBtn.className = 'btn exchange-remove-target';
    removeBtn.appendChild(createIcon('x'));
    removeBtn.setAttribute('aria-label', 'Remove target');
    removeBtn.addEventListener('click', () => {
      if (targetRows.length <= 1) return; // keep at least one row
      const idx = targetRows.findIndex((r) => r.rowEl === rowEl);
      if (idx >= 0) {
        targetRows.splice(idx, 1);
        rowEl.remove();
        updateRunningTotal();
        invalidateQuote();
      }
    });
    rowEl.appendChild(removeBtn);

    // Cost chip: shows per-instance monthly cost for the selected offering
    // when it appears in alternativeTargets; otherwise shows "—".
    const chipEl = document.createElement('span');
    chipEl.className = 'cost-chip';
    chipEl.textContent = '—';
    rowEl.appendChild(chipEl);

    // pickerSelect drives the hidden offeringInput and refreshes the chip.
    pickerSelect.addEventListener('change', () => {
      offeringInput.value = pickerSelect.value;
      updateRowChip(pickerSelect.value, chipEl);
      updateRunningTotal();
      invalidateQuote();
    });
    countInput.addEventListener('input', () => {
      updateRunningTotal();
      invalidateQuote();
    });

    targetsContainer.appendChild(rowEl);
    targetRows.push({ offeringInput, pickerSelect, countInput, chipEl, rowEl });
    // Initial chip population for pre-filled rows.
    updateRowChip(offeringInput.value, chipEl);
  };

  // lookupCEAlt returns the alternativeTargets entry (cost and currency)
  // for an offering_id, or undefined when absent.
  function lookupCEAlt(offeringId: string): OfferingOption | undefined {
    if (!alternativeTargets || !offeringId) return undefined;
    return alternativeTargets.find((a) => a.offering_id === offeringId);
  }

  function updateRowChip(offeringId: string, chip: HTMLSpanElement): void {
    const alt = lookupCEAlt(offeringId);
    chip.textContent = alt ? `${formatOfferingCost(alt)}/mo each` : '—';
  }

  // Seed the modal with one row, optionally pre-selecting by offering_id.
  // suggestedTargetType is an instance type (from reshape recs); we
  // match it against CE alternatives to find the offering_id to pre-select.
  const suggestedOfferingId = suggestedTargetType && alternativeTargets
    ? (alternativeTargets.find((a) => a.instance_type === suggestedTargetType)?.offering_id)
    : undefined;
  addTargetRow(suggestedOfferingId, count);

  // Kick off the async AWS offerings load after the first row exists.
  void populateAwsOfferings();

  const addTargetBtnRow = document.createElement('div');
  addTargetBtnRow.className = 'setting-row';
  const addTargetBtn = document.createElement('button');
  addTargetBtn.type = 'button';
  addTargetBtn.className = 'btn';
  addTargetBtn.id = 'modal-exchange-add-target';
  addTargetBtn.textContent = '+ Add target';
  addTargetBtn.addEventListener('click', () => {
    addTargetRow();
    updateRunningTotal();
    invalidateQuote();
  });
  addTargetBtnRow.appendChild(addTargetBtn);
  content.appendChild(addTargetBtnRow);

  // Running total line. Hidden when openExchangeModal is called without
  // alternativeTargets (Convertible RIs table path) since there's no
  // pricing to sum.
  const runningTotalEl = document.createElement('div');
  runningTotalEl.className = 'setting-row exchange-running-total';
  runningTotalEl.id = 'modal-exchange-running-total';
  if (!alternativeTargets || alternativeTargets.length === 0) {
    runningTotalEl.classList.add('hidden');
  }
  content.appendChild(runningTotalEl);

  function updateRunningTotal(): void {
    if (!alternativeTargets || alternativeTargets.length === 0) {
      return;
    }
    let total = 0;
    let anyMissing = false;
    const prefixes = new Set<string>();
    for (const row of targetRows) {
      const alt = lookupCEAlt(row.offeringInput.value);
      const rawCount = parseInt(row.countInput.value, 10);
      const cnt = isNaN(rawCount) || rawCount < 1 ? 1 : rawCount;
      if (!alt) {
        anyMissing = true;
        continue;
      }
      prefixes.add(currencyPrefix(alt.currency_code));
      total += alt.effective_monthly_cost * cnt;
    }
    if (prefixes.size > 1) {
      // Adding amounts in different currencies would print a meaningless sum.
      runningTotalEl.textContent = 'Estimated monthly cost for the quoted target set: not totaled -- targets are priced in different currencies';
      return;
    }
    const [prefix = '$'] = prefixes;
    const suffix = anyMissing ? ' (incomplete -- some targets have no pricing data)' : '';
    runningTotalEl.textContent = `Estimated monthly cost for the quoted target set: ${formatCurrency(total, prefix, 2)}/mo${suffix}`;
  }
  updateRunningTotal();

  // Result container. A polite live region so quote results, progress and
  // the exchange receipt are announced; setResultText marks errors as alerts.
  const resultContainer = document.createElement('div');
  resultContainer.id = 'modal-exchange-result';
  resultContainer.setAttribute('role', 'status');
  resultContainer.setAttribute('aria-live', 'polite');
  content.appendChild(resultContainer);

  // Execute button (hidden until valid quote)
  const executeBtn = document.createElement('button');
  executeBtn.className = 'btn primary hidden';
  executeBtn.textContent = 'Execute Exchange';

  // Buttons row
  const btnRow = document.createElement('div');
  btnRow.className = 'modal-buttons';

  const quoteBtn = document.createElement('button');
  quoteBtn.className = 'btn primary';
  quoteBtn.textContent = 'Get Quote';

  const cancelBtn = document.createElement('button');
  cancelBtn.className = 'btn';
  cancelBtn.textContent = 'Cancel';

  btnRow.appendChild(quoteBtn);
  btnRow.appendChild(executeBtn);
  btnRow.appendChild(cancelBtn);
  content.appendChild(btnRow);

  // Show modal
  openModal(modal, { onClose: () => {
    sessionOpen = false;
    invalidateQuote();
  } });

  cancelBtn.addEventListener('click', () => {
    closeModal(modal);
  });

  // invalidateQuote discards any previously fetched quote whenever the
  // target set changes (picker, count, remove-row), so Execute can never
  // submit a request that no longer matches what the form displays.
  function invalidateQuote(): void {
    quoteRevision++;
    modalQuote = null;
    modalQuoteReq = null;
    executeBtn.classList.add('hidden');
    resultContainer.textContent = '';
  }

  quoteBtn.addEventListener('click', () => {
    void submitModalQuote();
  });

  executeBtn.addEventListener('click', () => {
    void submitModalExecute();
  });

  // lockCompletedExchange freezes the form after a successful exchange so
  // no edit can clear the receipt (edits call invalidateQuote) and no second
  // quote is offered for RIs that no longer exist. Cancel becomes Close.
  function lockCompletedExchange(): void {
    targetsContainer.querySelectorAll<HTMLInputElement | HTMLSelectElement | HTMLButtonElement>('input, select, button')
      .forEach((el) => { el.disabled = true; });
    addTargetBtn.disabled = true;
    executeBtn.classList.add('hidden');
    quoteBtn.classList.add('hidden');
    cancelBtn.textContent = 'Close';
    cancelBtn.focus();
  }

  // offeringUUIDPattern mirrors the backend regex for AWS offering UUIDs.
  const offeringUUIDPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

  // collectTargets reads each row into a validated target. Rows with
  // empty or non-UUID offering IDs are treated as an error; the first
  // offending row's message is surfaced to the user. Count defaults to 1
  // when the input is empty or non-numeric.
  function collectTargets(): { targets: Array<{ offering_id: string; count: number }>; error?: string } {
    const targets: Array<{ offering_id: string; count: number }> = [];
    for (let i = 0; i < targetRows.length; i++) {
      const row = targetRows[i];
      if (!row) continue;
      const offeringId = row.offeringInput.value.trim();
      if (!offeringId) {
        return { targets: [], error: `Please select a target offering for target ${i + 1}.` };
      }
      if (!offeringUUIDPattern.test(offeringId)) {
        return {
          targets: [],
          error: `Target ${i + 1}: "${offeringId}" is not a valid offering UUID. Please select an offering from the dropdown.`,
        };
      }
      const rawCount = parseInt(row.countInput.value, 10);
      const targetCount = isNaN(rawCount) || rawCount < 1 ? 1 : rawCount;
      targets.push({ offering_id: offeringId, count: targetCount });
    }
    return { targets };
  }

  // buildQuoteReq shapes the request body. With one target we post
  // the singleton shape (target_offering_id + target_count) for
  // backward-compat with older backends and simpler CI harnesses.
  // With >1 target we post targets[] so the backend builds a
  // multi-element TargetConfigurations slice in the AWS Accept call.
  function buildQuoteReq(targets: Array<{ offering_id: string; count: number }>): QuoteReqShape {
    const first = targets[0];
    if (targets.length === 1 && first) {
      return {
        ri_ids: [riId],
        target_offering_id: first.offering_id,
        target_count: first.count,
      };
    }
    return { ri_ids: [riId], targets };
  }

  async function submitModalQuote(): Promise<void> {
    if (quoteBtn.disabled) return; // guard against overlapping quote requests
    const { targets, error } = collectTargets();
    if (error) {
      setResultText(resultContainer, error, 'error');
      return;
    }

    invalidateQuote();
    const revision = quoteRevision;
    setResultText(resultContainer, 'Getting exchange quote...', 'loading');
    quoteBtn.disabled = true;

    const quoteReq = buildQuoteReq(targets);
    try {
      const quote = await api.getExchangeQuote(quoteReq);
      if (!sessionOpen || revision !== quoteRevision) return;
      modalQuote = quote;
      modalQuoteReq = quoteReq;
      renderModalQuoteResult(resultContainer, modalQuote);
      if (modalQuote.IsValidExchange) executeBtn.classList.remove('hidden');
    } catch (error) {
      if (!sessionOpen || revision !== quoteRevision) return;
      const err = error as Error;
      setResultText(resultContainer, 'Quote failed: ' + err.message, 'error');
    } finally {
      quoteBtn.disabled = false;
    }
  }

  async function submitModalExecute(): Promise<void> {
    if (executeBtn.disabled || !sessionOpen || !modalQuote?.IsValidExchange || !modalQuoteReq) return;
    const quote = modalQuote;
    const request = modalQuoteReq;
    if (!exchangePaymentPattern.test(quote.PaymentDueRaw) || !quote.CurrencyCode?.trim()) {
      setResultText(resultContainer, 'Cannot confirm exchange without a quoted payment and currency. Get a new quote.', 'error');
      return;
    }
    const targets = request.targets ?? [{ offering_id: request.target_offering_id, count: request.target_count }];
    executeBtn.disabled = true;
    quoteBtn.disabled = true;
    let confirmed = false;
    try {
      confirmed = await confirmDialog({
        title: 'Execute RI Exchange',
        body: buildExchangeConfirmBody({
          quote,
          sources: request.ri_ids.map(id => {
            const ri = currentRIs.find(r => r.reserved_instance_id === id);
            return ri ? `${ri.instance_count} \u00d7 ${ri.instance_type} (${id})` : id;
          }),
          targets: targets.map(target => {
            const o = awsOfferings.find(x => x.offering_id === target.offering_id);
            if (o) return `${target.count} \u00d7 ${o.instance_type} (${o.offering_type}, offering ${target.offering_id})`;
            const alt = alternativeTargets?.find(x => x.offering_id === target.offering_id);
            return alt
              ? `${target.count} \u00d7 ${alt.instance_type} (offering ${target.offering_id})`
              : `${target.count} \u00d7 offering ${target.offering_id}`;
          }),
        }),
        confirmLabel: 'Execute Exchange',
        destructive: true,
        initialFocus: 'cancel',
      });
      if (!confirmed) return;
      setResultText(resultContainer, 'Executing exchange...', 'loading');
      const result = await api.executeExchange({
        ...request,
        max_payment_due_usd: quote.PaymentDueRaw,
        region: quote.Region,
      });
      void loadRIExchange();
      // The toast is the receipt even if the modal was closed mid-request;
      // the exchange ID also stays in the open modal until the user closes it.
      showToast({
        kind: 'success',
        message: `RI exchange completed. Exchange ID: ${result.exchange_id}`,
        actions: [{ label: 'View history', onClick: () => {
          if (sessionOpen) closeModal(modal);
          revealExchangeHistory();
        } }],
      });
      if (!sessionOpen) return;
      setResultText(resultContainer, 'Exchange completed. ID: ' + result.exchange_id, 'success-message');
      modalQuote = null;
      modalQuoteReq = null;
      lockCompletedExchange();
    } catch (error) {
      if (!sessionOpen) return;
      const err = error as Error;
      setResultText(resultContainer, 'Exchange failed: ' + err.message, 'error');
    } finally {
      executeBtn.disabled = false;
      quoteBtn.disabled = false;
      if (!confirmed) executeBtn.focus();
    }
  }
}

// revealExchangeHistory scrolls the Exchange History card into view and
// moves focus to its list, where the completed exchange is listed.
function revealExchangeHistory(): void {
  const list = document.getElementById('ri-exchange-history-list');
  if (!list) return;
  list.tabIndex = -1;
  (list.closest('section') ?? list).scrollIntoView({ block: 'start' });
  list.focus({ preventScroll: true });
}

function setResultText(container: HTMLElement, message: string, cls: string): void {
  container.textContent = '';
  const p = document.createElement('p');
  p.className = cls;
  // Errors interrupt; progress and success are announced politely by the
  // container's own role="status".
  if (cls === 'error') p.setAttribute('role', 'alert');
  p.textContent = message;
  container.appendChild(p);
}

function renderModalQuoteResult(container: HTMLElement, quote: ExchangeQuoteSummary): void {
  container.textContent = '';

  const div = document.createElement('div');
  div.className = 'quote-summary ' + (quote.IsValidExchange ? 'quote-valid' : 'quote-invalid');

  const h4 = document.createElement('h4');
  h4.textContent = quote.IsValidExchange ? 'Valid Exchange' : 'Invalid Exchange';
  div.appendChild(h4);

  if (quote.ValidationFailureReason) {
    const p = document.createElement('p');
    p.className = 'error';
    p.textContent = quote.ValidationFailureReason;
    div.appendChild(p);
  }

  const rows: [string, string][] = [
    ['Currency', quote.CurrencyCode],
    ['Payment Due', quote.PaymentDueRaw],
    ['Source Hourly Price', quote.SourceHourlyPriceRaw],
    ['Target Hourly Price', quote.TargetHourlyPriceRaw],
  ];
  const delta = hourlyPriceDelta(quote);
  if (delta) rows.push(['Hourly Price Change', delta]);
  if (quote.OutputReservedInstancesExp) {
    rows.push(['New RI Expiry', quote.OutputReservedInstancesExp]);
  }

  const details = document.createElement('div');
  details.className = 'quote-details';
  for (const [label, value] of rows) {
    const row = document.createElement('div');
    row.className = 'quote-row';
    const span = document.createElement('span');
    span.textContent = label + ':';
    const strong = document.createElement('strong');
    strong.textContent = value;
    row.appendChild(span);
    row.appendChild(strong);
    details.appendChild(row);
  }
  div.appendChild(details);
  container.appendChild(div);
}

// ──────────────────────────────────────────────
// Automation Settings
// ──────────────────────────────────────────────

export async function loadAutomationSettings(): Promise<void> {
  const container = document.getElementById('ri-exchange-automation-settings');
  if (!container) return;

  // Hold the section's place with its own titled frame while the config
  // loads (issue #190): a bare "Loading settings..." line between the GCP
  // block and Commitment Laddering read as the whole page half-loaded.
  // Inside the frame, the shared text skeleton (issue #186) stands in for
  // the form: it brings the screen-reader "Loading…" status and the
  // "Still loading" note, so no visible loading paragraph is needed.
  const loadingBody = document.createElement('div');
  const loadingShell = automationSectionShell(loadingBody);
  loadingShell.setAttribute('aria-busy', 'true');
  container.replaceChildren(loadingShell);
  showSkeletonText(loadingBody, 4);

  try {
    const config = await api.getRIExchangeConfig();
    renderAutomationSettings(container, config);
  } catch (error) {
    // Issue #1413: when the backend returns 403 permission denied the user
    // legitimately lacks access to the Exchange Automation section. Hide it
    // silently (same graceful-degradation pattern as loadGlobalSettings for
    // issue #979) instead of showing an error paragraph at the bottom of the
    // Purchasing Policies panel. Any other failure (network, 5xx) still
    // surfaces the error so operators can diagnose the problem.
    if (isPermissionDeniedError(error)) {
      container.textContent = '';
      return;
    }
    const err = error as Error;
    const errorP = document.createElement('p');
    errorP.className = 'error';
    errorP.textContent = 'Failed to load settings: ' + err.message;
    const retryBtn = document.createElement('button');
    retryBtn.type = 'button';
    retryBtn.className = 'btn btn-small mt-2';
    retryBtn.textContent = 'Retry';
    retryBtn.addEventListener('click', () => { void loadAutomationSettings(); });
    container.replaceChildren(automationSectionShell(errorP, retryBtn));
  }
}

/**
 * The "Exchange Automation" fieldset frame used while the real form is not
 * there yet (loading or failed), so the state is attributed to this section
 * rather than floating between its neighbours.
 */
function automationSectionShell(...children: HTMLElement[]): HTMLFieldSetElement {
  const fieldset = document.createElement('fieldset');
  fieldset.className = 'settings-category';
  const legend = document.createElement('legend');
  legend.textContent = 'Exchange Automation';
  fieldset.append(legend, ...children);
  return fieldset;
}

function buildAutoWarningBanner(): HTMLDivElement {
  const banner = document.createElement('div');
  banner.className = 'warning-message';
  const strong = document.createElement('strong');
  strong.textContent = 'Warning:';
  banner.appendChild(strong);
  banner.appendChild(document.createTextNode(' Fully Automated mode will execute RI exchanges without manual approval. Ensure spending caps are configured properly.'));
  return banner;
}

/**
 * Render a config number into a double-quoted value="..." attribute. The
 * RIExchangeConfig number types are erased at runtime, so escape rather than
 * trust them (issue #37); a missing field renders as an empty input.
 */
function numberAttr(value: number | null | undefined): string {
  return escapeHtmlAttr(String(value ?? ''));
}

function renderAutomationSettings(container: HTMLElement, config: RIExchangeConfig): void {
  const modeOptions = Object.entries(MODE_LABELS)
    .map(([value, label]) => {
      const selected = value === config.mode ? ' selected' : '';
      return '<option value="' + escapeHtml(value) + '"' + selected + '>' + escapeHtml(label) + '</option>';
    })
    .join('');

  // Build the form via innerHTML since this is all developer-controlled markup
  // (all dynamic values are escaped via escapeHtml / numberAttr)
  const formHTML = '<form id="ri-exchange-settings-form" class="settings-form">'
    + '<fieldset class="settings-category">'
    + '<legend>Exchange Automation</legend>'
    + '<div id="ri-exchange-warning-slot"></div>'
    + '<div class="setting-row">'
    +   '<div class="setting-info"><label for="ri-exchange-enabled">Enable Automated Exchange</label></div>'
    +   '<div class="setting-input"><label class="toggle-label">'
    +     '<input type="checkbox" id="ri-exchange-enabled"' + (config.auto_exchange_enabled ? ' checked' : '') + '>'
    +     '<span class="slider"></span>'
    +   '</label></div>'
    + '</div>'
    + '<div class="setting-row">'
    +   '<div class="setting-info"><label for="ri-exchange-mode">Mode</label></div>'
    +   '<div class="setting-input"><select id="ri-exchange-mode">' + modeOptions + '</select></div>'
    + '</div>'
    + '<div class="setting-row">'
    +   '<div class="setting-info"><label for="ri-exchange-threshold">Utilization Threshold (%)</label></div>'
    +   '<div class="setting-input"><input type="number" id="ri-exchange-threshold" value="' + numberAttr(config.utilization_threshold) + '" min="0" max="100" step="0.1"></div>'
    + '</div>'
    + '<div class="setting-row">'
    +   '<div class="setting-info"><label for="ri-exchange-max-per-exchange">Max Payment Per Exchange (USD)</label></div>'
    +   '<div class="setting-input"><input type="number" id="ri-exchange-max-per-exchange" value="' + numberAttr(config.max_payment_per_exchange_usd) + '" min="0" step="0.01"></div>'
    + '</div>'
    + '<div class="setting-row">'
    +   '<div class="setting-info"><label for="ri-exchange-max-daily">Max Payment Daily (USD)</label></div>'
    +   '<div class="setting-input"><input type="number" id="ri-exchange-max-daily" value="' + numberAttr(config.max_payment_daily_usd) + '" min="0" step="0.01"></div>'
    + '</div>'
    + '<div class="setting-row">'
    +   '<div class="setting-info"><label for="ri-exchange-lookback">Lookback Days</label></div>'
    +   '<div class="setting-input"><input type="number" id="ri-exchange-lookback" value="' + numberAttr(config.lookback_days) + '" min="1" max="365"></div>'
    + '</div>'
    + '</fieldset>'
    + '<div id="ri-exchange-settings-message" class="mt-3"></div>'
    + '</form>';

  container.textContent = '';
  const wrapper = document.createElement('div');
  wrapper.innerHTML = formHTML;
  while (wrapper.firstChild) {
    container.appendChild(wrapper.firstChild);
  }

  // Insert warning banner via DOM if mode is auto
  if (config.mode === 'auto') {
    const slot = document.getElementById('ri-exchange-warning-slot');
    if (slot) slot.appendChild(buildAutoWarningBanner());
  }

  // No per-form Save button: the Purchasing panel's sticky "Save Settings"
  // bar is the single source of truth and calls saveAutomationSettings()
  // alongside the global config save. We still stop any stray submit (e.g.
  // Enter inside a number field) so it doesn't navigate the page.
  const form = document.getElementById('ri-exchange-settings-form');
  if (form) {
    form.addEventListener('submit', (e) => {
      e.preventDefault();
    });
  }

  // Update warning banner when mode changes
  const modeSelect = document.getElementById('ri-exchange-mode') as HTMLSelectElement | null;
  if (modeSelect) {
    modeSelect.addEventListener('change', () => {
      const slot = document.getElementById('ri-exchange-warning-slot');
      if (!slot) return;
      if (modeSelect.value === 'auto') {
        if (!slot.firstChild) slot.appendChild(buildAutoWarningBanner());
      } else {
        slot.textContent = '';
      }
    });
  }

  // Issue #870: re-apply the purchasing-panel read-only gate after rendering
  // so the dynamically-injected RI Exchange inputs are covered for non-admin
  // sessions (loadGlobalSettings runs concurrently and may complete before
  // these inputs exist).
  applyReadOnlySettings(null);
}

export async function saveAutomationSettings(): Promise<void> {
  const messageContainer = document.getElementById('ri-exchange-settings-message');
  if (!messageContainer) return;

  const enabledInput = document.getElementById('ri-exchange-enabled') as HTMLInputElement | null;
  const modeInput = document.getElementById('ri-exchange-mode') as HTMLSelectElement | null;
  const thresholdInput = document.getElementById('ri-exchange-threshold') as HTMLInputElement | null;
  const maxPerExchangeInput = document.getElementById('ri-exchange-max-per-exchange') as HTMLInputElement | null;
  const maxDailyInput = document.getElementById('ri-exchange-max-daily') as HTMLInputElement | null;
  const lookbackInput = document.getElementById('ri-exchange-lookback') as HTMLInputElement | null;

  if (!enabledInput || !modeInput || !thresholdInput || !maxPerExchangeInput || !maxDailyInput || !lookbackInput) return;

  const threshold = parseFloat(thresholdInput.value);
  const maxPerExchange = parseFloat(maxPerExchangeInput.value);
  const maxDaily = parseFloat(maxDailyInput.value);
  const lookback = parseInt(lookbackInput.value, 10);
  const mode = modeInput.value as RIExchangeConfig['mode'];

  // Client-side validation
  if (isNaN(threshold) || threshold < 0 || threshold > 100) {
    messageContainer.textContent = '';
    const p = document.createElement('p');
    p.className = 'error';
    p.textContent = 'Utilization threshold must be between 0 and 100.';
    messageContainer.appendChild(p);
    return;
  }
  if (isNaN(lookback) || lookback < 1 || lookback > 365) {
    messageContainer.textContent = '';
    const p = document.createElement('p');
    p.className = 'error';
    p.textContent = 'Lookback days must be between 1 and 365.';
    messageContainer.appendChild(p);
    return;
  }
  if (isNaN(maxPerExchange) || maxPerExchange < 0) {
    messageContainer.textContent = '';
    const p = document.createElement('p');
    p.className = 'error';
    p.textContent = 'Max payment per exchange must be >= 0.';
    messageContainer.appendChild(p);
    return;
  }
  if (isNaN(maxDaily) || maxDaily < 0) {
    messageContainer.textContent = '';
    const p = document.createElement('p');
    p.className = 'error';
    p.textContent = 'Max daily payment must be >= 0.';
    messageContainer.appendChild(p);
    return;
  }

  // Confirm auto mode — financial operations ship without further review
  // once enabled, so this gets the destructive-style confirm.
  if (mode === 'auto') {
    const confirmed = await confirmDialog({
      title: 'Enable Fully Automated mode?',
      body: 'RI exchanges will be executed automatically without manual approval. Make sure the payment caps below are set to values you are comfortable spending.',
      confirmLabel: 'Enable automation',
      destructive: true,
    });
    if (!confirmed) return;
  }

  const config: RIExchangeConfig = {
    auto_exchange_enabled: enabledInput.checked,
    mode,
    utilization_threshold: threshold,
    max_payment_per_exchange_usd: maxPerExchange,
    max_payment_daily_usd: maxDaily,
    lookback_days: lookback,
  };

  messageContainer.textContent = '';
  const loadingP = document.createElement('p');
  loadingP.className = 'loading';
  loadingP.textContent = 'Saving settings...';
  messageContainer.appendChild(loadingP);

  try {
    await api.updateRIExchangeConfig(config);
    messageContainer.textContent = '';
    const successP = document.createElement('p');
    successP.className = 'success-message';
    successP.textContent = 'Settings saved successfully.';
    messageContainer.appendChild(successP);
    setTimeout(() => {
      messageContainer.textContent = '';
    }, 3000);
  } catch (error) {
    const err = error as Error;
    messageContainer.textContent = '';
    const errorP = document.createElement('p');
    errorP.className = 'error';
    errorP.textContent = 'Failed to save settings: ' + err.message;
    messageContainer.appendChild(errorP);
  }
}

// ──────────────────────────────────────────────
// Exchange History
// ──────────────────────────────────────────────

// canApproveRIExchangeRow returns true when the current session may approve
// the given pending RI exchange via the inline Approve button (issue #300).
// UX gate only — the backend authorizeSessionApproveRIExchange remains the
// security boundary; a false-positive here surfaces as a 403 toast on click.
//
// Heuristic mirrors canApprovePendingRow in history.ts:
//   * status must be "pending";
//   * admin -> always yes;
//   * non-admin matching created_by_user_id -> yes (approve-own);
//   * legacy rows without created_by_user_id -> no (email-link path only).
function canApproveRIExchangeRow(rec: RIExchangeHistoryRecord): boolean {
  if ((rec.status || '').toLowerCase() !== 'pending') return false;
  const user = getCurrentUser();
  if (!user) return false;
  if (canAccess('admin', '*') || canAccess('approve-any', 'purchases')) return true;
  if (!rec.created_by_user_id) return false;
  return canAccess('approve-own', 'purchases') && rec.created_by_user_id === user.id;
}

async function loadExchangeHistory(generation: number = scopeGeneration): Promise<void> {
  const container = document.getElementById('ri-exchange-history-list');
  if (!container) return;

  // Issue #186: skeleton rows shaped like the 7-column history table.
  showSkeletonRows(container, 3, 7);

  try {
    const records = await api.getRIExchangeHistory();
    if (generation !== scopeGeneration) return;
    renderExchangeHistory(container, records);
  } catch (error) {
    if (generation !== scopeGeneration) return;
    const err = error as Error;
    container.textContent = '';
    const errorP = document.createElement('p');
    errorP.className = 'error';
    errorP.textContent = 'Failed to load exchange history: ' + err.message;
    container.appendChild(errorP);
  }
}

function renderExchangeHistory(container: HTMLElement, records: RIExchangeHistoryRecord[]): void {
  if (!records || records.length === 0) {
    container.textContent = '';
    const emptyP = document.createElement('p');
    emptyP.className = 'empty';
    emptyP.textContent = 'No exchange history found.';
    container.appendChild(emptyP);
    return;
  }

  // Build table rows — all dynamic values go through escapeHtml
  const rowsHTML = records.map(rec => {
    const exchangeIdCell = rec.exchange_id
      ? '<span class="monospace">' + escapeHtml(rec.exchange_id) + '</span>'
      : '&mdash;';
    // A failed exchange carries the AWS reason in rec.error; show it under the
    // badge so the operator can see why without reading backend logs.
    const errorLine = rec.error
      ? '<div class="status-detail">' + escapeHtml(rec.error) + '</div>'
      : '';
    const approveBtn = canApproveRIExchangeRow(rec)
      ? '<button type="button" class="btn-link riexchange-approve-btn" data-approve-id="' + escapeHtml(rec.id) + '">Approve</button>'
      : '';
    return '<tr>'
      + '<td>' + escapeHtml(formatDateTime(rec.created_at)) + '</td>'
      + '<td>' + escapeHtml(String(rec.source_count)) + 'x ' + escapeHtml(rec.source_instance_type) + '</td>'
      + '<td>' + escapeHtml(String(rec.target_count)) + 'x ' + escapeHtml(rec.target_instance_type) + '</td>'
      // payment_due is USD (the execute cap is max_payment_due_usd); an
      // empty amount renders as a dash, not a lone "$" (issue #284).
      + '<td class="num">' + (rec.payment_due ? escapeHtml(formatDecimalStringCurrency(rec.payment_due)) : '&mdash;') + '</td>'
      + '<td>' + statusBadgeHtml(rec.status) + errorLine + '</td>'
      + '<td>' + exchangeIdCell + '</td>'
      + '<td>' + approveBtn + '</td>'
      + '</tr>';
  }).join('');

  const tableHTML = '<table class="table-compact table-sticky-end">'
    + '<thead><tr>'
    + '<th>Date</th><th>Source Type</th><th>Target Type</th><th class="num">Payment</th><th>Status</th><th>Exchange ID</th><th>Actions</th>'
    + '</tr></thead>'
    + '<tbody>' + rowsHTML + '</tbody>'
    + '</table>';

  container.textContent = '';
  const wrapper = document.createElement('div');
  wrapper.innerHTML = tableHTML;
  while (wrapper.firstChild) {
    container.appendChild(wrapper.firstChild);
  }

  // Wire Approve button click handlers
  container.querySelectorAll<HTMLButtonElement>('.riexchange-approve-btn[data-approve-id]').forEach(btn => {
    const record = records.find(rec => rec.id === btn.dataset.approveId);
    if (record) btn.addEventListener('click', () => handleRIExchangeApproveClick(btn, record));
  });
}

async function handleRIExchangeApproveClick(btn: HTMLButtonElement, record: RIExchangeHistoryRecord): Promise<void> {
  if (btn.disabled) return;
  if (!exchangePaymentPattern.test(record.payment_due)) {
    showToast({ kind: 'error', message: 'Cannot approve exchange without a previously quoted payment.' });
    return;
  }

  btn.disabled = true;
  const confirmed = await confirmDialog({
    title: 'Approve RI Exchange',
    body: buildApproveConfirmBody(record),
    confirmLabel: 'Approve',
    destructive: true,
  });
  if (!confirmed) {
    btn.disabled = false;
    btn.focus();
    return;
  }

  try {
    await api.approveRIExchange(record.id);
    showToast({ kind: 'success', message: 'RI exchange approved and executing.' });
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    showToast({ kind: 'error', message: 'Failed to approve exchange: ' + msg });
    btn.disabled = false;
    return;
  }
  try {
    await loadExchangeHistory();
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    showToast({ kind: 'error', message: 'Approved, but failed to refresh history: ' + msg });
  }
}
