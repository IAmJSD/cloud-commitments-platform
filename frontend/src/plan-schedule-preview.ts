import { closeModal, openModal } from './modal';
import { escapeHtml } from './utils';
import { suggestedPurchaseStartDate } from './lib/calendar-date';
import { draftDateDay, redistributeDraftDates, validateDraftDates } from './lib/plan-draft-dates';

export interface PlanSchedulePreviewInput {
  name: string;
  preset: string;
  coverage: string;
  stepPercent: string;
  intervalDays: string;
  notificationDays: string;
  term: string;
  payment: string;
}

function integer(value: string, label: string, min: number, max: number): number {
  const parsed = value.trim() === '' ? NaN : Number(value);
  if (!Number.isInteger(parsed) || parsed < min || parsed > max) throw new Error(`${label} must be a whole number between ${min} and ${max}`);
  return parsed;
}

export function openPlanSchedulePreview(input: PlanSchedulePreviewInput, parent: HTMLElement): void {
  const coverage = integer(input.coverage, 'Target coverage', 0, 100);
  const notice = integer(input.notificationDays, 'Notification days', 1, 30);
  // Mirrors presets/shares in internal/config/types.go and custom
  // step counting in PlanRequest.buildCustomRampSchedule, internal/api/types.go.
  const presets: Record<string, readonly [number, number]> = { immediate: [100, 0], 'weekly-25pct': [25, 7], 'monthly-10pct': [10, 30] };
  const preset = Object.prototype.hasOwnProperty.call(presets, input.preset) ? presets[input.preset] : undefined;
  if (!preset && input.preset !== 'custom') throw new Error('Choose a valid ramp schedule before previewing');
  const percent = preset?.[0] ?? integer(input.stepPercent, 'Step percent', 1, 100);
  const interval = preset?.[1] ?? integer(input.intervalDays, 'Interval days', 1, 365);
  const count = Math.floor(100 / percent);
  const start = Date.parse(`${suggestedPurchaseStartDate(notice)}T00:00:00Z`);
  const points = Array.from({ length: count }, (_, index) => ({
    date: new Date(start + index * interval * 86_400_000).toISOString().slice(0, 10),
    target: coverage * Math.min(100, (index + 1) * percent) / 100,
  }));
  const initialDates = points.map(point => point.date);
  let dates = [...initialDates], selected = 0;
  let dirty: 'step' | 'range' | null = null;
  const previous = document.getElementById('plan-schedule-preview');
  if (previous) { closeModal(previous); previous.remove(); }
  const modal = document.createElement('div');
  modal.id = 'plan-schedule-preview';
  modal.className = 'modal hidden';
  modal.setAttribute('role', 'dialog');
  modal.setAttribute('aria-modal', 'true');
  modal.setAttribute('aria-labelledby', 'schedule-preview-title');
  const width = Math.max(640, count * 48 + 96);
  const x = (index: number): number => 48 + (draftDateDay(dates[index]!) - draftDateDay(dates[0]!) + 3)
    / (draftDateDay(dates[count - 1]!) - draftDateDay(dates[0]!) + 8) * (width - 96);
  const y = (target: number): number => 260 - target * 2.2;
  let path = `M 48 ${y(0)}`;
  points.forEach((point, index) => { path += ` H ${x(index)} V ${y(point.target)}`; });
  modal.innerHTML = `
    <div class="modal-content schedule-preview-panel">
      <header><h2 id="schedule-preview-title">Schedule preview</h2><p>${escapeHtml(input.name)}</p></header>
      <div class="schedule-preview-body">
        <p class="schedule-preview-notice">Draft preview. These dates are not saved or scheduled. Saved purchases and history are not shown.
          Draft date changes are local to this preview. Closing discards them; Save Plan does not apply these date edits.</p>
        <div class="schedule-preview-layout">
          <section aria-label="Draft schedule">
            <h3>Planned cumulative target (%)</h3>
            <p class="help-text">Hypothetical dates, not observed utilization. All dates use UTC.</p>
            <div class="schedule-preview-scroll" tabindex="0" aria-label="Scrollable draft calendar">
              <svg viewBox="0 0 ${width} 310" width="${width}" role="group" aria-label="Hypothetical purchase dates">
                ${[0, 20, 40, 60, 80, 100].map(value => `<path class="preview-grid" d="M 48 ${y(value)} H ${width - 24}"/><text x="8" y="${y(value) + 4}">${value}</text>`).join('')}
                <path class="preview-staircase" d="${path}"/>
                ${points.map((point, index) => `<g role="button" tabindex="${index === 0 ? 0 : -1}" data-preview-step="${index}" aria-pressed="${index === 0}" aria-label="Step ${index + 1}, ${point.date}, target ${point.target}%">
                  <circle class="preview-hit" cx="${x(index)}" cy="${y(point.target)}" r="22"/>
                  <circle class="preview-marker" cx="${x(index)}" cy="${y(point.target)}" r="6"/>
                  ${index % Math.ceil(count / 5) === 0 || index === count - 1 ? `<text x="${x(index)}" y="292" text-anchor="middle">${point.date.slice(5)}</text>` : ''}
                </g>`).join('')}
              </svg>
            </div>
            <p class="help-text">○ Draft date · Arrow keys select steps; Alt+Left/Right moves a date one day. Use the Step selector or date inputs when points overlap.</p>
            <details id="preview-date-changes"><summary>Draft date changes (0)</summary><ul></ul></details>
            <p id="preview-date-status" class="help-text" role="status"></p>
          </section>
          <aside aria-label="Draft plan controls">
            <h3>Plan summary</h3>
            <dl><dt>Target</dt><dd>${coverage}%</dd><dt>Steps</dt><dd>${count}</dd><dt>Start</dt><dd id="preview-start">${points[0]!.date}</dd><dt>End</dt><dd id="preview-end">${points[count - 1]!.date}</dd></dl>
            <label>All draft steps: start (UTC)<input id="preview-range-start" type="date" value="${dates[0]}"></label>
            ${count > 1 ? `<label>All draft steps: end (UTC)<input id="preview-range-end" type="date" value="${dates[count - 1]}"></label>` : ''}
            <label>Step<select id="preview-step-select">${points.map((_, index) => `<option value="${index}">Step ${index + 1}</option>`).join('')}</select></label>
            <h3 id="preview-selected-step">Step 1</h3>
            <label>Preview date (UTC)<input id="preview-selected-date" type="date" value="${points[0]!.date}"></label>
            <p id="preview-date-error" role="alert"></p>
            <button type="button" id="preview-reset-dates">Reset draft dates</button>
            <p id="preview-selected-target" role="status"></p>
            <p class="help-text">Amount unavailable in this draft preview.</p>
            <p class="help-text">Saved purchases and approval windows are not checked here.</p>
            <details><summary>Advanced settings</summary><dl><dt>Term</dt><dd>${escapeHtml(input.term)} years</dd><dt>Payment</dt><dd>${escapeHtml(input.payment)}</dd><dt>Notification window</dt><dd>${notice} days</dd></dl></details>
          </aside>
        </div>
      </div>
      <footer><button type="button" id="close-schedule-preview">Close preview</button></footer>
    </div>`;
  document.body.appendChild(modal);
  const buttons = Array.from(modal.querySelectorAll<SVGGElement>('[data-preview-step]'));
  const dateInput = modal.querySelector<HTMLInputElement>('#preview-selected-date')!;
  const rangeStart = modal.querySelector<HTMLInputElement>('#preview-range-start')!;
  const rangeEnd = modal.querySelector<HTMLInputElement>('#preview-range-end');
  const stepSelect = modal.querySelector<HTMLSelectElement>('#preview-step-select')!;
  const error = (message: string): void => { modal.querySelector('#preview-date-error')!.textContent = message; };
  const lockControls = (): void => {
    dateInput.disabled = dirty === 'range';
    rangeStart.disabled = dirty === 'step';
    if (rangeEnd) rangeEnd.disabled = dirty === 'step';
    stepSelect.disabled = dirty !== null;
    buttons.forEach(button => button.setAttribute('aria-disabled', String(dirty !== null)));
  };
  const select = (index: number, focus: boolean): void => {
    if (dirty) return;
    selected = index;
    const point = points[index]!;
    buttons.forEach((button, i) => { button.setAttribute('aria-pressed', String(i === index)); button.setAttribute('tabindex', i === index ? '0' : '-1'); });
    modal.querySelector('#preview-selected-step')!.textContent = `Step ${index + 1}`;
    if (dateInput.value !== dates[index]) dateInput.value = dates[index]!;
    stepSelect.value = String(index);
    modal.querySelector('#preview-selected-target')!.textContent = `Planned target: ${point.target}%`;
    if (focus) buttons[index]!.focus();
  };
  const renderDates = (): void => {
    let staircase = `M 48 ${y(0)}`;
    buttons.forEach((button, index) => {
      const left = x(index), date = dates[index]!;
      staircase += ` H ${left} V ${y(points[index]!.target)}`;
      button.querySelectorAll('circle').forEach(circle => circle.setAttribute('cx', String(left)));
      const tick = button.querySelector('text');
      if (tick) { tick.setAttribute('x', String(left)); tick.textContent = date.slice(5); }
      button.setAttribute('aria-label', `Step ${index + 1}, ${date}, target ${points[index]!.target}%`);
    });
    modal.querySelector('.preview-staircase')!.setAttribute('d', staircase);
    modal.querySelector('#preview-start')!.textContent = dates[0]!;
    modal.querySelector('#preview-end')!.textContent = dates[count - 1]!;
    if (rangeStart.value !== dates[0]) rangeStart.value = dates[0]!;
    if (rangeEnd && rangeEnd.value !== dates[count - 1]) rangeEnd.value = dates[count - 1]!;
    const changes = dates.flatMap((date, index) => date === initialDates[index] ? [] : [`Step ${index + 1}: ${initialDates[index]} → ${date}`]);
    modal.querySelector('#preview-date-changes summary')!.textContent = `Draft date changes (${changes.length})`;
    const items = changes.map(change => { const item = document.createElement('li'); item.textContent = change; return item; });
    modal.querySelector('#preview-date-changes ul')!.replaceChildren(...items);
    modal.querySelector('#preview-date-status')!.textContent = `Draft dates changed: ${changes.length}. Nothing is saved.`;
    select(selected, false);
  };
  const apply = (candidate: () => string[]): void => {
    try {
      const next = candidate();
      validateDraftDates(next);
      dates = next; dirty = null; error(''); lockControls(); renderDates();
    } catch (reason) { error(reason instanceof Error ? reason.message : String(reason)); }
  };
  [dateInput, rangeStart, ...(rangeEnd ? [rangeEnd] : [])].forEach(control => {
    const group = control === dateInput ? 'step' : 'range';
    const begin = (): boolean => {
      if (dirty && dirty !== group) return false;
      dirty = group; lockControls(); return true;
    };
    control.addEventListener('input', () => { begin(); });
    control.addEventListener('change', () => {
      if (begin()) apply(() => control === dateInput
        ? dates.map((date, index) => index === selected ? dateInput.value : date)
        : redistributeDraftDates(dates, rangeStart.value, rangeEnd?.value ?? rangeStart.value));
    });
  });
  stepSelect.addEventListener('change', () => select(Number(stepSelect.value), false));
  modal.querySelector('#preview-reset-dates')!.addEventListener('click', () => apply(() => {
    try { validateDraftDates(initialDates); }
    catch { throw new Error('Initial dates are no longer future dates. Close and reopen the preview for fresh dates.'); }
    return [...initialDates];
  }));
  buttons.forEach((button, index) => {
    button.addEventListener('click', () => select(index, true));
    button.addEventListener('keydown', event => {
      if (['Enter', ' ', 'ArrowLeft', 'ArrowRight'].includes(event.key)) {
        event.preventDefault();
        if (dirty) return;
        if (event.altKey && ['ArrowLeft', 'ArrowRight'].includes(event.key)) {
          apply(() => dates.map((date, i) => i === index ? new Date((draftDateDay(date)
            + (event.key === 'ArrowRight' ? 1 : -1)) * 86_400_000).toISOString().slice(0, 10) : date));
          return;
        }
        const next = event.key === 'ArrowLeft' ? Math.max(0, index - 1) : event.key === 'ArrowRight' ? Math.min(count - 1, index + 1) : index;
        select(next, true);
      }
    });
  });
  modal.querySelector('#close-schedule-preview')!.addEventListener('click', () => closeModal(modal));
  const wasInert = parent.inert;
  parent.inert = true;
  select(0, false);
  openModal(modal, { onClose: () => { parent.inert = wasInert; modal.remove(); } });
}
