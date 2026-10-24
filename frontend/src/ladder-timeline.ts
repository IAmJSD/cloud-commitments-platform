import { Chart, registerables } from 'chart.js';
import { listAccountsMinimal, type AccountSummary } from './api/accounts';
import { getLadderEvents, getLadderRuns, type LadderConfig, type LadderEvent, type LadderRun } from './api/ladder';
import { canAccess } from './permissions';
import { escapeHtml } from './utils';
import { emptyLadderFilters, filterLadderEvents, ladderDimensions, ladderFilterOptions, ladderEditReason, ladderTotalOd, ladderOdAmount, ladderLabel, formatLadderAmount, formatLadderUnits, ladderNotCountedNote, LADDER_COUNTED_NOTE, formatLadderDateTime, type LadderFilters } from './ladder-timeline-filters';
import { openLadderEventEditor } from './ladder-timeline-editor';
import { ladderChartConfig, ladderChartModel, ladderChartSummary, LADDER_UNIT_LABEL } from './ladder-timeline-chart';

Chart.register(...registerables);
let disposeTimeline: (() => void) | undefined;

/**
 * Destroys the planned-purchase chart (Chart.js removes its resize and event
 * listeners on destroy) and drops any in-flight timeline load. Navigation
 * calls this when the Purchasing settings section is left; loadGlobalSettings
 * re-runs initLadderTimeline when the section is shown again (issue #667).
 */
export function disposeLadderTimeline(): void {
  disposeTimeline?.();
  disposeTimeline = undefined;
}

export async function initLadderTimeline(root: HTMLElement, configs: readonly LadderConfig[], globalEnabled: boolean): Promise<void> {
  disposeLadderTimeline();
  // Settings loads also run for the General sub-tab, where the Purchasing
  // panel is hidden; building a chart there would leak it until the next init.
  if (root.closest('.hidden')) return;
  let chart: Chart<'line'> | undefined;
  let rowTotals = new Map<string, bigint | null>();
  let generation = 0;
  let disposed = false;
  let events: LadderEvent[] = [];
  let runs: LadderRun[] = [];
  let accounts: AccountSummary[] = [];
  let accountsLoaded = false;
  let filters: LadderFilters = { ...emptyLadderFilters(), status: 'scheduled', from: new Date().toISOString().slice(0, 19) };
  let selectedID = '';
  disposeTimeline = () => { disposed = true; generation++; chart?.destroy(); chart = undefined; };
  const labels = { run_id: 'Run', layer_type: 'Commitment type', term: 'Commitment term', payment_option: 'Payment option', status: 'Status' };
  root.innerHTML = `<h4>Planned purchases</h4><p>Inspect retained planning tranches and edit future scheduled budgets.
    Ladder purchases are not executed yet. Coverage and expiry forecasts are unavailable.</p>
    <div class="ladder-timeline-controls"><label>Cloud provider <select data-provider><option value="aws">AWS</option><option value="azure">Azure</option><option value="gcp">GCP</option></select></label>
    <label>Cloud account <select data-account></select></label><button type="button" data-reload>Reload timeline</button></div>
    <p data-state role="status"></p><div data-content hidden>
    <div class="ladder-timeline-controls">${ladderDimensions.map(key => `<label>${labels[key]} <select data-filter="${key}"></select></label>`).join('')}
    <label>From (UTC) <input type="datetime-local" step="1" data-date="from"></label>
    <label>To (UTC) <input type="datetime-local" step="1" data-date="to"></label>
    <button type="button" data-reset>Clear filters</button></div>
    <p>Service, instance type, region and currency filters are unavailable because saved ladder tranches do not contain those fields.</p>
    <p data-summary aria-live="polite"></p><p data-snapshot></p>
    <div class="ladder-timeline-chart"><canvas aria-label="Cumulative planned commitment chart. The purchase table below lists the same figures." role="img"></canvas></div>
    <div data-table></div></div>`;
  const provider = root.querySelector<HTMLSelectElement>('[data-provider]')!;
  const account = root.querySelector<HTMLSelectElement>('[data-account]')!;
  const state = root.querySelector<HTMLElement>('[data-state]')!;
  const content = root.querySelector<HTMLElement>('[data-content]')!;
  const summary = root.querySelector<HTMLElement>('[data-summary]')!;
  const table = root.querySelector<HTMLElement>('[data-table]')!;
  root.querySelector<HTMLInputElement>('[data-date="from"]')!.value = filters.from;
  const focusEvent = (id: string): void => {
    Array.from(table.querySelectorAll<HTMLButtonElement>('[data-event]')).find(button => button.dataset['event'] === id)?.focus();
  };
  const selectEvent = (event: LadderEvent): void => {
    selectedID = event.id;
    render();
    focusEvent(event.id);
    const reason = ladderEditReason(event);
    const run = runs.find(item => item.id === event.run_id);
    if (!reason && run && canAccess('update', 'config')) {
      openLadderEventEditor(event, run, events.filter(item => item.run_id === event.run_id), () => { void load().then(() => focusEvent(event.id)); });
    }
  };
  const cumulativeCell = (event: LadderEvent): string => {
    const note = ladderNotCountedNote(event);
    return note ?? formatLadderAmount(formatLadderUnits(rowTotals.get(event.id)!));
  };
  const render = (): void => {
    const visible = filterLadderEvents(events, filters).sort((a, b) => Date.parse(a.scheduled_date) - Date.parse(b.scheduled_date));
    if (!visible.some(event => event.id === selectedID)) selectedID = '';
    root.querySelectorAll<HTMLSelectElement>('[data-filter]').forEach(select => {
      const key = select.dataset['filter'] as typeof ladderDimensions[number];
      const options = ladderFilterOptions(events, filters, key);
      if (filters[key] && !options.includes(filters[key])) options.unshift(filters[key]);
      select.innerHTML = `<option value="">All</option>${options.map(value => `<option value="${escapeHtml(value)}">${escapeHtml(key === 'run_id' ? formatLadderDateTime(runs.find(run => run.id === value)?.started_at ?? value) : ladderLabel(key, value))}</option>`).join('')}`;
      select.value = filters[key];
    });
    summary.textContent = `${visible.length} of ${events.length} retained tranches; ${formatLadderAmount(ladderTotalOd(visible))} in this selection.`;
    const snapshot = root.querySelector<HTMLElement>('[data-snapshot]')!;
    const run = runs.find(item => item.id === filters.run_id);
    snapshot.textContent = run ? `Original run ${formatLadderDateTime(run.started_at)}: baseline ${formatLadderAmount(run.baseline_usd_hr)}, existing ${formatLadderAmount(run.existing_usd_hr)}, target ${formatLadderAmount(run.target_usd_hr)}. These snapshots are not a coverage forecast. ${run.actions.map(action => `${action.rationale}${action.data_sources?.length ? ` Sources: ${action.data_sources.join(', ')}.` : ''}`).join(' ')}` : 'Select a run to inspect its original baseline, existing commitment and target snapshots.';
    const canvas = root.querySelector<HTMLCanvasElement>('canvas')!;
    const chartBox = canvas.parentElement!;
    const model = visible.length ? ladderChartModel(visible, run) : undefined;
    if (!model) {
      chart?.destroy(); chart = undefined;
      chartBox.hidden = true;
      rowTotals = new Map();
    } else {
      chartBox.hidden = false;
      const selected = visible.find(event => event.id === selectedID);
      const config = ladderChartConfig(model, selected && Date.parse(selected.scheduled_date), {
        legendClick: layer => { filters.layer_type = filters.layer_type === layer ? '' : layer; render(); },
        pointClick: (series, index) => {
          const item = visible.find(event => event.layer_type === series.layer && Date.parse(event.scheduled_date) === model.cumulative.times[index]);
          if (item) selectEvent(item);
        },
      });
      canvas.setAttribute('aria-label', ladderChartSummary(model));
      rowTotals = model.cumulative.rowTotals;
      if (chart) { chart.data = config.data; chart.options = config.options!; chart.update('none'); } else chart = new Chart(canvas, config);
    }
    if (visible.length === 0) {
      const rationale = runs.flatMap(item => item.actions.map(action => action.rationale)).filter(Boolean).join(' ');
      table.textContent = events.length ? 'No purchases match these filters. Clear filters to see the retained plan.' : `No planned purchase tranches. ${rationale}`;
      return;
    }
    table.innerHTML = `<table class="data-table" aria-label="Planned purchase tranches"><caption>Planned purchases with running cumulative total. ${LADDER_COUNTED_NOTE} The base is the selected run's existing commitment, or 0 (planned additions only) when no run is selected.</caption><thead><tr><th>Scheduled (UTC)</th><th>Type</th><th>Term</th><th>Payment</th><th>Budget (${LADDER_UNIT_LABEL.toLowerCase()})</th><th>Cumulative (${LADDER_UNIT_LABEL.toLowerCase()})</th><th>Status</th><th>Select or edit</th></tr></thead><tbody>${visible.map(event => {
      const reason = ladderEditReason(event);
      return `<tr class="${event.id === selectedID ? 'ladder-selected' : ''}"><td>${escapeHtml(formatLadderDateTime(event.scheduled_date))}</td><td>${escapeHtml(ladderLabel('layer_type', event.layer_type))}</td><td>${escapeHtml(ladderLabel('term', event.term))}</td><td>${escapeHtml(ladderLabel('payment_option', event.payment_option))}</td><td>${escapeHtml(formatLadderAmount(ladderOdAmount(event)))}</td><td>${escapeHtml(cumulativeCell(event))}</td><td>${escapeHtml(ladderLabel('status', event.status))}</td><td><button type="button" data-event="${escapeHtml(event.id)}" ${event.id === selectedID ? 'aria-current="true"' : ''}>${!reason && canAccess('update', 'config') ? 'Edit' : 'Select'}</button>${reason ? `<span>${escapeHtml(reason)}</span>` : ''}</td></tr>`;
    }).join('')}</tbody></table>`;
    table.querySelectorAll<HTMLButtonElement>('[data-event]').forEach(button => button.addEventListener('click', () => {
      const event = visible.find(item => item.id === button.dataset['event']);
      if (event) selectEvent(event);
    }));
  };
  const load = async (): Promise<void> => {
    if (disposed) return;
    const request = ++generation;
    chart?.destroy(); chart = undefined;
    content.hidden = true;
    state.textContent = 'Loading complete retained timeline...';
    try {
      if (!accountsLoaded) {
        accounts = await listAccountsMinimal();
        if (request !== generation) return;
        accountsLoaded = true; setAccounts();
      }
      if (provider.value !== 'aws') { state.textContent = 'Ladder planning is currently available only for AWS accounts.'; return; }
      if (!account.value) { state.textContent = 'No cloud accounts are available for this provider.'; return; }
      const scope = { account: account.value, provider: provider.value };
      const [loadedEvents, loadedRuns] = await Promise.all([getLadderEvents(scope.account, scope.provider), getLadderRuns(scope.account, scope.provider)]);
      if (request !== generation) return;
      events = loadedEvents; runs = loadedRuns;
      const cfg = configs.find(item => item.cloud_account_id === scope.account && item.provider === scope.provider);
      state.textContent = !cfg ? 'No ladder configuration. Add an account config above to start planning.' : !globalEnabled || !cfg.enabled ? 'Ladder planning is disabled. Retained plans remain visible.' : runs.length === 0 ? 'No planning runs have been recorded for this account.' : 'Timeline loaded. All amounts are planning budgets.';
      content.hidden = false; render();
    } catch (err) {
      if (request === generation) state.textContent = `Timeline unavailable: ${err instanceof Error ? err.message : 'load failed'}. Use Reload timeline to retry.`;
    }
  };
  const setAccounts = (): void => {
    account.innerHTML = accounts.filter(item => item.provider === provider.value).map(item => `<option value="${escapeHtml(item.id)}">${escapeHtml(item.name)} (${escapeHtml(item.external_id)})</option>`).join('');
  };
  const resetFilters = (): void => {
    filters = emptyLadderFilters();
    root.querySelectorAll<HTMLInputElement>('[data-date]').forEach(input => { input.value = ''; });
  };
  provider.addEventListener('change', () => { resetFilters(); setAccounts(); void load(); });
  account.addEventListener('change', () => { resetFilters(); void load(); });
  root.querySelector('[data-reload]')!.addEventListener('click', () => { void load(); });
  root.querySelectorAll<HTMLSelectElement>('[data-filter]').forEach(select => select.addEventListener('change', () => { filters[select.dataset['filter'] as typeof ladderDimensions[number]] = select.value; render(); }));
  root.querySelectorAll<HTMLInputElement>('[data-date]').forEach(input => input.addEventListener('change', () => { filters[input.dataset['date'] as 'from' | 'to'] = input.value; render(); }));
  root.querySelector('[data-reset]')!.addEventListener('click', () => { resetFilters(); render(); });
  await load();
}
