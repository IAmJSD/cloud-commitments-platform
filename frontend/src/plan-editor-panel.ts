import { wirePlanDateInteraction } from './plan-date-interaction';
import { buildLocalScheduleIntent, PlanSchedulePreviewInput } from './plan-schedule-preview';
import { createEditorDraft, draftDateChanges, EditorDraft, redistributeEditorBlock, resetEditorDraft, selectDraftStep, setDraftDate } from './lib/plan-editor-draft';
import { renderSchedulePlot, schedulePlotFrame, updateSchedulePlot, SchedulePlotFrame } from './plan-schedule-plot';

export function readPlanScheduleInput(form: HTMLFormElement): PlanSchedulePreviewInput {
  const value = (id: string): string => form.querySelector<HTMLInputElement | HTMLSelectElement>(`#${id}`)?.value ?? '';
  return { name: value('plan-name'), preset: form.querySelector<HTMLInputElement>('input[name="ramp-schedule"]:checked')?.value ?? '',
    coverage: value('plan-coverage'), stepPercent: value('ramp-step-percent'), intervalDays: value('ramp-interval-days'),
    notificationDays: value('plan-notify-days'), term: value('plan-term'), payment: value('plan-payment') };
}

export interface PlanEditorPanelOptions {
  // Create Save does not persist step dates yet, so the graph stays display-only until it does.
  editableDates?: boolean;
}

const readOnlyReason = 'Custom step dates can be set once saving them is supported.';

export function mountPlanEditorPanel(host: HTMLElement, form: HTMLFormElement, options: PlanEditorPanelOptions = {}): () => void {
  const editable = options.editableDates ?? false;
  host.hidden = false;
  host.closest('.plan-editor-layout')?.classList.add('has-editor-draft');
  host.innerHTML = `<h3>Proposed purchase dates</h3><p class="editor-draft-notice">${editable ? 'Local draft. These dates are not scheduled. Save Plan does not save date changes.' : readOnlyReason}</p>
    <div class="schedule-preview-scroll" tabindex="0" aria-label="Proposed purchase calendar"></div>
    <p class="editor-target help-text"></p>${editable ? `<p class="help-text">Drag a point to change its date, or Start/End to space the dates.</p>
    <details class="editor-help"><summary>Keyboard and touch help</summary>
      <p class="help-text">Drag dates or Start/End handles. Arrow keys select; Alt+Left/Right moves a date. Enter on a handle opens its date input. Escape cancels a drag. Use Step/date inputs when points overlap.</p>
    </details>` : ''}
    <p class="help-text">Amount unavailable. Saved purchases and history are not shown.</p>
    <label>Step<select data-editor="step"></select></label><label>Date (UTC)<input data-editor="date" type="date"></label>
    <div class="form-row"><label>Proposed start (UTC)<input data-editor="start" type="date"></label>
    <label data-editor="end-label">Proposed end (UTC)<input data-editor="end" type="date"></label></div>
    <button type="button" data-editor="reset">Reset draft dates</button><button type="button" data-editor="rebuild" hidden>Rebuild proposed dates</button>
    <p data-editor="error" role="alert"></p><details data-editor="changes"><summary>Local date changes</summary><ul></ul></details>`;
  const field = <T extends HTMLElement>(key: string): T => host.querySelector<T>(`[data-editor="${key}"]`)!;
  const step = field<HTMLSelectElement>('step'), date = field<HTMLInputElement>('date');
  const start = field<HTMLInputElement>('start'), end = field<HTMLInputElement>('end');
  const rebuild = field<HTMLButtonElement>('rebuild'), reset = field<HTMLButtonElement>('reset');
  let coverage: number, gestureFrame: SchedulePlotFrame | null = null, gestureDraft: EditorDraft | null = null;
  let disposeInteraction: (() => void) | undefined;
  const inertBefore = new Map<HTMLElement, boolean>();
  const inertTargets = [form, ...host.closest('.modal')!.querySelectorAll<HTMLElement>('.modal-buttons button:not(#close-plan-modal-btn)')];
  let state: EditorDraft | null = null, dirty: 'step' | 'range' | null = null;
  const rebuildMessage = "Schedule settings changed. Rebuild proposed dates to replace your local date edits.";
  const error = (message: string): void => {
    try { buildLocalScheduleIntent(readPlanScheduleInput(form)); }
    catch (failure) { message = (failure as Error).message; }
    field('error').textContent = message;
  };
  const paint = (): void => {
    if (!editable) {
      step.disabled = date.disabled = start.disabled = end.disabled = reset.disabled = rebuild.disabled = true;
    } else {
      step.disabled = !state || !!gestureFrame || dirty !== null; date.disabled = !state || !!gestureFrame || dirty === 'range';
      start.disabled = !state || !!gestureFrame || dirty === 'step'; end.disabled = !state || !!gestureFrame || dirty === 'step';
      reset.disabled = !state || !!gestureFrame; rebuild.disabled = !!gestureFrame;
    }
    if (!state) return;
    const dates = state.dates.map(value => { if (value === null) throw new Error('Enter a proposed date for every step.'); return value; });
    const context = state.context;
    if (context.kind !== 'create') throw new Error('Create draft context required.');
    const points = dates.map((value, index) => ({ step: index + 1, date: value, target: coverage * context.shares[index]! / 100 }));
    const frame = gestureFrame ?? schedulePlotFrame(dates), plot = host.querySelector('.schedule-preview-scroll')!;
    const svg = plot.querySelector<SVGSVGElement>('svg');
    if (svg && svg.querySelectorAll('[data-preview-step]').length === points.length) updateSchedulePlot(svg, points, frame, state.selected, editable);
    else {
      disposeInteraction?.(); plot.innerHTML = renderSchedulePlot(points, frame, state.selected, editable);
      if (editable) disposeInteraction = wire(plot.querySelector<SVGSVGElement>('svg')!);
    }
    if (step.options.length !== dates.length) step.replaceChildren(...dates.map((_, index) => new Option(`Step ${index + 1}`, String(index))));
    step.value = String(state.selected);
    if (dirty !== 'step') date.value = dates[state.selected]!;
    if (dirty !== 'range') { start.value = dates[0]!; end.value = dates[dates.length - 1]!; }
    field('end-label').hidden = dates.length === 1; end.disabled ||= dates.length === 1;
    host.querySelector('.editor-target')!.textContent = `Proposed cumulative coverage (%), target ${coverage}%. All dates use UTC.`;
    const changes = draftDateChanges(state);
    field('changes').querySelector('summary')!.textContent = `Local date changes (${changes.length})`;
    field('changes').querySelector('ul')!.replaceChildren(...changes.map(change => {
      const item = document.createElement('li'); item.textContent = `Step ${change.step}: ${change.before} → ${change.after}`; return item;
    }));
  };
  const wire = (svg: SVGSVGElement): (() => void) => wirePlanDateInteraction(svg, {
    read: () => ({ dates: state!.dates.map(value => value!), blocked: dirty !== null }),
    select: index => { state = selectDraftStep(state!, index); paint(); },
    gesture: current => {
      gestureFrame = current;
      if (current) {
        gestureDraft = state;
        inertTargets.forEach(target => { inertBefore.set(target, target.inert); target.inert = true; });
      } else {
        inertBefore.forEach((value, target) => { target.inert = value; }); inertBefore.clear(); gestureDraft = null;
      }
      paint();
    },
    apply: ({ index, boundary, date: proposed, before }) => {
      try {
        const base = gestureDraft ?? state!;
        state = boundary ? redistributeEditorBlock(base, boundary === 'start' ? proposed : before[0]!,
          boundary === 'end' ? proposed : before[before.length - 1]!) : setDraftDate(base, index, proposed);
        dirty = null; error(rebuild.hidden ? '' : rebuildMessage); paint();
      } catch (failure) { error((failure as Error).message); }
    },
    restore: () => { if (gestureDraft) state = gestureDraft; paint(); },
    focusDate: boundary => (boundary === 'start' ? start : end).focus(),
    error: message => error(message || (rebuild.hidden ? '' : rebuildMessage)),
  });
  const build = (): void => {
    if (gestureFrame) return;
    try {
      const intent = buildLocalScheduleIntent(readPlanScheduleInput(form));
      state = createEditorDraft({ kind: 'create', totalSteps: intent.dates.length, shares: intent.shares, dates: intent.dates });
      coverage = intent.coverage; dirty = null; rebuild.hidden = true; error(''); paint();
    } catch (failure) { error((failure as Error).message); paint(); }
  };
  const commit = (): void => {
    if (!state || gestureFrame) return;
    try {
      state = dirty === 'range' ? redistributeEditorBlock(state, start.value, end.value) : setDraftDate(state, state.selected, date.value);
      dirty = null; error(rebuild.hidden ? '' : rebuildMessage); paint();
    } catch (failure) { error((failure as Error).message); paint(); }
  };
  const input = (event: Event): void => {
    if (!editable || gestureFrame) return;
    if (event.target === date) dirty = 'step';
    else if (event.target === start || event.target === end) dirty = 'range';
    else return;
    paint();
  };
  const change = (event: Event): void => {
    if (!editable || gestureFrame) return;
    if (event.target === step && state && !dirty) {
      try { state = selectDraftStep(state, step.value === '' ? NaN : Number(step.value)); paint(); }
      catch (failure) { error((failure as Error).message); }
    }
    else if (event.target === date || event.target === start || event.target === end) commit();
  };
  const settings = (event: Event): void => {
    if (gestureFrame) return;
    const target = event.target as HTMLInputElement;
    if (!state) { if (target.id) build(); return; }
    if (target.id === 'plan-coverage') {
      try { coverage = buildLocalScheduleIntent(readPlanScheduleInput(form)).coverage; if (!dirty && rebuild.hidden) error(''); paint(); }
      catch (failure) { error((failure as Error).message); }
    } else if (target.name === 'ramp-schedule' || ['ramp-step-percent', 'ramp-interval-days', 'plan-notify-days'].includes(target.id)) {
      if (state && (dirty || draftDateChanges(state).length)) { rebuild.hidden = false; error(rebuildMessage); }
      else build();
    }
  };
  const keydown = (event: KeyboardEvent): void => {
    if (editable && event.key === 'Enter' && [date, start, end].includes(event.target as HTMLInputElement)) { event.preventDefault(); commit(); }
  };
  reset.onclick = () => { if (editable && state && !gestureFrame) { state = resetEditorDraft(state); dirty = null; error(rebuild.hidden ? '' : rebuildMessage); paint(); } };
  rebuild.onclick = build;
  host.addEventListener('input', input); host.addEventListener('change', change); host.addEventListener('keydown', keydown);
  form.addEventListener('input', settings); form.addEventListener('change', settings);
  const submit = (event: Event): void => {
    if (gestureFrame) { event.preventDefault(); event.stopImmediatePropagation(); error('Finish or cancel the date drag before saving.'); }
  };
  form.addEventListener('submit', submit, true);
  build();
  return () => {
    host.removeEventListener('input', input); host.removeEventListener('change', change); host.removeEventListener('keydown', keydown);
    form.removeEventListener('input', settings); form.removeEventListener('change', settings);
    disposeInteraction?.(); form.removeEventListener('submit', submit, true);
    host.replaceChildren(); host.hidden = true; host.closest('.plan-editor-layout')?.classList.remove('has-editor-draft');
    state = null;
  };
}
