import { closeModal, openModal } from './modal';
import { wirePlanDateInteraction } from './plan-date-interaction';
import { renderSchedulePlot, updateSchedulePlot, schedulePlotFrame, SchedulePlotFrame } from './plan-schedule-plot';
import { escapeHtml } from './utils';
import { buildLocalScheduleIntent, PlanScheduleIntentInput } from './plan-schedule-intent';
import { redistributeDraftDates, validateDraftDates } from './lib/plan-draft-dates';

export type PlanSchedulePreviewInput = PlanScheduleIntentInput;

export function openPlanSchedulePreview(input: PlanSchedulePreviewInput, parent: HTMLElement): void {
  const { coverage, notificationDays: notice, dates: proposedDates, shares } = buildLocalScheduleIntent(input);
  const count = proposedDates.length;
  const points = proposedDates.map((date, index) => ({ step: index + 1, date, target: coverage * shares[index]! / 100 }));
  const initialDates = points.map(point => point.date);
  let dates = [...initialDates], selected = 0;
  let dirty: 'step' | 'range' | null = null;
  let gesture: SchedulePlotFrame | null = null;
  const previous = document.getElementById('plan-schedule-preview');
  if (previous) { closeModal(previous); previous.remove(); }
  const modal = document.createElement('div');
  modal.id = 'plan-schedule-preview';
  modal.className = 'modal hidden';
  modal.setAttribute('role', 'dialog');
  modal.setAttribute('aria-modal', 'true');
  modal.setAttribute('aria-labelledby', 'schedule-preview-title');
  const frame = (): SchedulePlotFrame => gesture ?? schedulePlotFrame(dates);
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
              ${renderSchedulePlot(points, frame(), selected, true)}
            </div>
            <p class="help-text">○ Draft date · Drag dates or interval handles horizontally inside the plot. Arrow keys select steps; Alt+Left/Right moves a date one day. Handle arrows move a day; Enter edits its date. Use the Step selector or date inputs when points overlap or dates are farther away.</p>
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
  const handles = Array.from(modal.querySelectorAll<SVGGElement>('[data-preview-boundary]'));
  const svg = modal.querySelector('svg')!;
  const dateInput = modal.querySelector<HTMLInputElement>('#preview-selected-date')!;
  const rangeStart = modal.querySelector<HTMLInputElement>('#preview-range-start')!;
  const rangeEnd = modal.querySelector<HTMLInputElement>('#preview-range-end');
  const stepSelect = modal.querySelector<HTMLSelectElement>('#preview-step-select')!;
  const error = (message: string): void => { modal.querySelector('#preview-date-error')!.textContent = message; };
  const lockControls = (): void => {
    dateInput.disabled = !!gesture || dirty === 'range';
    rangeStart.disabled = !!gesture || dirty === 'step';
    if (rangeEnd) rangeEnd.disabled = !!gesture || dirty === 'step';
    stepSelect.disabled = dirty !== null || !!gesture;
    modal.querySelector<HTMLButtonElement>('#preview-reset-dates')!.disabled = !!gesture;
    [...buttons, ...handles].forEach(button => button.setAttribute('aria-disabled', String(dirty !== null || !!gesture)));
  };
  const select = (index: number, focus: boolean): void => {
    if (dirty || (gesture && index !== selected)) return;
    selected = index;
    const point = points[index]!;
    updateSchedulePlot(svg, points.map((point, i) => ({ ...point, date: dates[i]! })), frame(), selected, true);
    modal.querySelector('#preview-selected-step')!.textContent = `Step ${index + 1}`;
    if (dateInput.value !== dates[index]) dateInput.value = dates[index]!;
    stepSelect.value = String(index);
    modal.querySelector('#preview-selected-target')!.textContent = `Planned target: ${point.target}%`;
    if (focus) buttons[index]!.focus();
  };
  const renderDates = (): void => {
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
      if (gesture || (dirty && dirty !== group)) return false;
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
  modal.querySelector('#preview-reset-dates')!.addEventListener('click', () => { if (gesture) return; apply(() => {
    try { validateDraftDates(initialDates); }
    catch { throw new Error('Initial dates are no longer future dates. Close and reopen the preview for fresh dates.'); }
    return [...initialDates];
  }); });
  const disposeInteraction = wirePlanDateInteraction(svg, {
    read: () => ({ dates, blocked: dirty !== null }),
    select: index => select(index, false),
    gesture: current => { gesture = current; lockControls(); renderDates(); },
    apply: ({ index, boundary, date, before }) => apply(() => boundary ? redistributeDraftDates(before,
      boundary === 'start' ? date : before[0]!, boundary === 'end' ? date : before[before.length - 1]!)
      : before.map((value, i) => i === index ? date : value)),
    restore: before => { dates = [...before]; renderDates(); },
    focusDate: boundary => (boundary === 'start' ? rangeStart : rangeEnd!).focus(),
    error,
  });
  modal.querySelector('#close-schedule-preview')!.addEventListener('click', () => closeModal(modal));
  const wasInert = parent.inert;
  parent.inert = true;
  renderDates();
  openModal(modal, { onClose: () => {
    disposeInteraction(); parent.inert = wasInert; modal.remove();
  } });
}
