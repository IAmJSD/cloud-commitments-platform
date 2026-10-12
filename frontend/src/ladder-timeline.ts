import { Chart, registerables } from 'chart.js';
import { listAccountsMinimal, type AccountSummary } from './api/accounts';
import { getLadderEvents, getLadderRuns, type LadderConfig, type LadderEvent, type LadderRun } from './api/ladder';
import { canAccess } from './permissions';
import { escapeHtml } from './utils';
import { emptyLadderFilters, filterLadderEvents, ladderDimensions, ladderFilterOptions, ladderEditReason, ladderTotal, ladderLabel, formatLadderAmount, formatLadderDateTime, type LadderFilters } from './ladder-timeline-filters';
import { openLadderEventEditor } from './ladder-timeline-editor';

Chart.register(...registerables);
let disposeTimeline: (() => void) | undefined;

export async function initLadderTimeline(root: HTMLElement, configs: readonly LadderConfig[], globalEnabled: boolean): Promise<void> {
  disposeTimeline?.();
  let chart: Chart<'scatter'> | undefined;
  let generation = 0;
  let disposed = false;
  let events: LadderEvent[] = [];
  let runs: LadderRun[] = [];
  let accounts: AccountSummary[] = [];
  let accountsLoaded = false;
  let filters: LadderFilters = { ...emptyLadderFilters(), status: 'scheduled', from: new Date().toISOString().slice(0, 19) };
  let selectedID = '';
  disposeTimeline = () => { disposed = true; generation++; chart?.destroy(); };
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
    <div class="ladder-timeline-chart"><canvas aria-label="Planned purchase timeline. Use the purchase table below to select a purchase with the keyboard." role="img"></canvas></div>
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
    summary.textContent = `${visible.length} of ${events.length} retained tranches; ${formatLadderAmount(ladderTotal(visible))} in this selection.`;
    const snapshot = root.querySelector<HTMLElement>('[data-snapshot]')!;
    const run = runs.find(item => item.id === filters.run_id);
    snapshot.textContent = run ? `Original run ${formatLadderDateTime(run.started_at)}: baseline ${formatLadderAmount(run.baseline_usd_hr)}, existing ${formatLadderAmount(run.existing_usd_hr)}, target ${formatLadderAmount(run.target_usd_hr)}. These snapshots are not a coverage forecast. ${run.actions.map(action => `${action.rationale}${action.data_sources?.length ? ` Sources: ${action.data_sources.join(', ')}.` : ''}`).join(' ')}` : 'Select a run to inspect its original baseline, existing commitment and target snapshots.';
    chart?.destroy();
    const groups = Array.from(new Set(visible.map(event => event.layer_type))).map(layer => ({ layer, events: visible.filter(event => event.layer_type === layer) }));
    const colors: Record<string, string> = { 'compute-sp': '#2563eb', 'ec2-instance-sp': '#0891b2', 'convertible-ri': '#7c3aed' };
    chart = new Chart(root.querySelector<HTMLCanvasElement>('canvas')!, {
      type: 'scatter', data: { datasets: groups.map(group => ({ label: `${group.layer} (USD/hour)`, backgroundColor: colors[group.layer] ?? '#64748b',
        data: group.events.map(event => ({ x: Date.parse(event.scheduled_date), y: Number(event.amount_usd_hr) })),
        pointRadius: group.events.map(event => event.id === selectedID ? 8 : 5),
        pointBackgroundColor: group.events.map(event => event.status === 'scheduled' ? colors[group.layer] ?? '#64748b' : '#64748b'),
      })) }, options: { responsive: true, maintainAspectRatio: false,
        scales: { x: { type: 'linear', title: { display: true, text: 'Scheduled date (UTC)' }, ticks: { callback: value => new Date(Number(value)).toISOString().slice(0, 10) } },
          y: { beginAtZero: true, title: { display: true, text: 'Additional planning budget (USD/hour)' } } },
        plugins: { legend: { onClick: (_event, item) => { const group = groups[item.datasetIndex ?? -1]; if (group) { filters.layer_type = filters.layer_type === group.layer ? '' : group.layer; render(); } } }, tooltip: { callbacks: { label: context => {
          const event = groups[context.datasetIndex]?.events[context.dataIndex];
          return event ? `${formatLadderDateTime(event.scheduled_date)}: ${formatLadderAmount(event.amount_usd_hr)} (${ladderLabel('layer_type', event.layer_type)}, ${ladderLabel('status', event.status)})` : '';
        } } } }, onClick: (_event, points) => { const point = points[0]; const item = point && groups[point.datasetIndex]?.events[point.index]; if (item) selectEvent(item); },
      },
    });
    if (visible.length === 0) {
      const rationale = runs.flatMap(item => item.actions.map(action => action.rationale)).filter(Boolean).join(' ');
      table.textContent = events.length ? 'No purchases match these filters. Clear filters to see the retained plan.' : `No planned purchase tranches. ${rationale}`;
      return;
    }
    table.innerHTML = `<table class="data-table" aria-label="Planned purchase tranches"><thead><tr><th>Scheduled (UTC)</th><th>Type</th><th>Term</th><th>Payment</th><th>Budget</th><th>Status</th><th>Select or edit</th></tr></thead><tbody>${visible.map(event => {
      const reason = ladderEditReason(event);
      return `<tr class="${event.id === selectedID ? 'ladder-selected' : ''}"><td>${escapeHtml(formatLadderDateTime(event.scheduled_date))}</td><td>${escapeHtml(ladderLabel('layer_type', event.layer_type))}</td><td>${escapeHtml(ladderLabel('term', event.term))}</td><td>${escapeHtml(ladderLabel('payment_option', event.payment_option))}</td><td>${escapeHtml(formatLadderAmount(event.amount_usd_hr))}</td><td>${escapeHtml(ladderLabel('status', event.status))}</td><td><button type="button" data-event="${escapeHtml(event.id)}" ${event.id === selectedID ? 'aria-current="true"' : ''}>${!reason && canAccess('update', 'config') ? 'Edit' : 'Select'}</button>${reason ? `<span>${escapeHtml(reason)}</span>` : ''}</td></tr>`;
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
