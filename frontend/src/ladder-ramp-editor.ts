/**
 * Visual editor for the ladder config's ramp schedule (markup in
 * ladder-config-modal.ts, rules in ladder-ramp-schedule.ts).
 *
 * The JSON textarea #ladder-cfg-ramp-schedule stays the value the save path
 * reads, so the request body is built exactly as before. The step rows are a
 * view over it: editing a row rewrites the JSON, and valid JSON typed into the
 * textarea (behind "Edit as JSON") redraws the rows. Invalid JSON leaves the
 * rows alone and shows the parser error under the textarea.
 *
 * Each row keeps its exact fraction in data-fraction, so a share the user did
 * not touch is written back unchanged even though the input shows a rounded
 * percent. A blank or non-whole row value cannot be written as JSON; until it
 * is fixed the textarea keeps its last good value and the save is refused.
 */
import { iconSvg } from './icons';
import {
  RAMP_PRESETS, fractionToPercent, parseRampJson, percentToFraction, rampJsonFor,
  rampTotalPercent, validateRampSteps, type RampIssue, type RampStep,
} from './ladder-ramp-schedule';

const ID = {
  list: 'ladder-ramp-steps',
  textarea: 'ladder-cfg-ramp-schedule',
  jsonError: 'ladder-cfg-ramp-schedule-error',
  jsonPanel: 'ladder-ramp-json',
  toggle: 'ladder-ramp-json-toggle',
  summary: 'ladder-ramp-summary',
  bar: 'ladder-ramp-bar',
  add: 'ladder-ramp-add',
};

// Top-level keys of the schedule other than steps, kept across row edits.
let rampBase: Record<string, unknown> = {};

const byId = <T extends HTMLElement>(id: string): T | null => document.getElementById(id) as T | null;

function rowHtml(step: RampStep, i: number, count: number): string {
  const n = i + 1;
  const after = Number.isNaN(step.after_days) ? '' : String(step.after_days);
  const share = Number.isNaN(step.fraction) ? '' : fractionToPercent(step.fraction);
  const fraction = Number.isNaN(step.fraction) ? '' : String(step.fraction);
  const disabled = count === 1 ? ' disabled' : '';
  return `
<li class="ramp-step" data-fraction="${fraction}">
  <span class="ramp-step-num" aria-hidden="true">${n}</span>
  <label class="sr-only" for="ladder-ramp-after-${i}">Step ${n} after (days)</label>
  <input type="number" id="ladder-ramp-after-${i}" class="ramp-after" min="0" step="1" inputmode="numeric" value="${after}">
  <label class="sr-only" for="ladder-ramp-share-${i}">Step ${n} share (%)</label>
  <input type="number" id="ladder-ramp-share-${i}" class="ramp-share" min="0" max="100" step="any" inputmode="decimal" value="${share}">
  <button type="button" class="btn-small icon-btn ramp-remove" data-index="${i}"
          aria-label="Remove step ${n}" title="Remove step ${n}"${disabled}>${iconSvg('trash-2')}</button>
  <p class="field-error ramp-step-error" id="ladder-ramp-error-${i}" hidden></p>
</li>`;
}

function renderRows(steps: readonly RampStep[]): void {
  const list = byId(ID.list);
  if (list) list.innerHTML = steps.map((s, i) => rowHtml(s, i, steps.length)).join('');
}

function rows(): HTMLLIElement[] {
  return Array.from(byId(ID.list)?.querySelectorAll<HTMLLIElement>('li.ramp-step') ?? []);
}

/** The steps as the rows describe them; NaN marks a value that cannot be written yet. */
function readRows(): RampStep[] {
  return rows().map(li => {
    const raw = li.querySelector<HTMLInputElement>('.ramp-after')?.value.trim() ?? '';
    const days = raw === '' ? NaN : Number(raw);
    const fraction = li.dataset['fraction'] ?? '';
    return {
      after_days: Number.isSafeInteger(days) ? days : NaN,
      fraction: fraction === '' ? NaN : Number(fraction),
    };
  });
}

const hasGap = (steps: readonly RampStep[]): boolean =>
  steps.some(s => Number.isNaN(s.after_days) || Number.isNaN(s.fraction));

function showRowIssues(issues: readonly RampIssue[]): void {
  rows().forEach((li, i) => {
    const mine = issues.filter(issue => issue.step === i);
    const note = li.querySelector<HTMLElement>('.ramp-step-error');
    for (const field of ['after', 'share'] as const) {
      const input = li.querySelector<HTMLInputElement>(`.ramp-${field}`);
      if (!input) continue;
      const bad = mine.some(issue => issue.field === field);
      if (bad) {
        input.setAttribute('aria-invalid', 'true');
        input.setAttribute('aria-describedby', `ladder-ramp-error-${i}`);
      } else {
        input.removeAttribute('aria-invalid');
        input.removeAttribute('aria-describedby');
      }
    }
    if (note) {
      note.textContent = mine.map(issue => issue.message).join(' ');
      note.hidden = mine.length === 0;
    }
  });
}

function summaryText(steps: readonly RampStep[], issues: readonly RampIssue[]): string {
  if (steps.length === 0) return 'No steps yet. Add at least one step.';
  const counted = steps.filter(s => !Number.isNaN(s.fraction));
  const total = `Total ${rampTotalPercent(counted)}%`;
  const prefix = steps.length === 1 ? `${total} in 1 step` : `${total} across ${steps.length} steps`;
  if (issues.some(issue => issue.step === null)) return `${prefix} · shares must add up to 100%`;
  const broken = new Set(issues.map(issue => issue.step)).size;
  if (broken > 0) return `${prefix} · ${broken === 1 ? '1 step needs' : `${broken} steps need`} fixing`;
  const last = steps[steps.length - 1]!.after_days;
  return `${prefix} · ${steps.length === 1 ? 'purchase' : 'last purchase'} on day ${last}`;
}

function renderBar(steps: readonly RampStep[], over: boolean): void {
  const bar = byId(ID.bar);
  if (!bar) return;
  const shown = steps.filter(s => s.fraction > 0 && s.fraction <= 1);
  const total = shown.reduce((sum, s) => sum + s.fraction, 0);
  const scale = Math.max(1, total);
  bar.classList.toggle('is-invalid', over);
  // Segments are built with the DOM and sized through CSSOM: the CSP has no
  // 'unsafe-inline' for styles, so a style="" attribute would be dropped.
  bar.replaceChildren(...shown.map(s => {
    const label = `${Number.isNaN(s.after_days) ? 'Day ?' : `Day ${s.after_days}`} · ${fractionToPercent(s.fraction)}%`;
    const seg = document.createElement('span');
    seg.className = 'ramp-bar-seg';
    seg.title = label;
    seg.style.flexGrow = String(s.fraction / scale);
    const text = document.createElement('span');
    text.textContent = label;
    seg.append(text);
    return seg;
  }));
  // The unallocated share stays as empty track.
  if (total < 1) {
    const rest = document.createElement('span');
    rest.className = 'ramp-bar-rest';
    rest.style.flexGrow = String(1 - total);
    bar.append(rest);
  }
}

/** Redraw the summary, bar and per-row messages for `steps`. */
function showState(steps: readonly RampStep[]): RampIssue[] {
  const issues = validateRampSteps(steps);
  showRowIssues(issues);
  const summary = byId(ID.summary);
  if (summary) {
    summary.textContent = summaryText(steps, issues);
    summary.classList.toggle('is-invalid', issues.length > 0);
  }
  renderBar(steps, issues.some(issue => issue.step === null));
  return issues;
}

function setJsonError(message: string): void {
  const note = byId(ID.jsonError);
  const textarea = byId<HTMLTextAreaElement>(ID.textarea);
  if (note) {
    note.textContent = message;
    note.hidden = message === '';
  }
  if (message) textarea?.setAttribute('aria-invalid', 'true');
  else textarea?.removeAttribute('aria-invalid');
}

function setJsonOpen(open: boolean): void {
  const panel = byId(ID.jsonPanel);
  const toggle = byId(ID.toggle);
  if (panel) panel.hidden = !open;
  if (toggle) {
    toggle.setAttribute('aria-expanded', String(open));
    toggle.textContent = open ? 'Hide JSON' : 'Edit as JSON';
  }
}

/** Rows changed: rewrite the JSON when every row can be written. */
function syncFromRows(): void {
  const steps = readRows();
  showState(steps);
  if (hasGap(steps)) return;
  const textarea = byId<HTMLTextAreaElement>(ID.textarea);
  if (textarea) textarea.value = rampJsonFor(rampBase, steps);
  setJsonError('');
}

/** JSON changed: redraw the rows if it decodes, otherwise say why and keep them. */
function syncFromJson(): boolean {
  const parsed = parseRampJson(byId<HTMLTextAreaElement>(ID.textarea)?.value ?? '');
  if (!parsed.ok) {
    setJsonError(parsed.message);
    return false;
  }
  setJsonError('');
  rampBase = parsed.base;
  renderRows(parsed.steps);
  showState(parsed.steps);
  return true;
}

function focusRow(index: number, prefer: 'after' | 'remove'): void {
  const li = rows()[index];
  if (!li) {
    byId(ID.add)?.focus();
    return;
  }
  const remove = li.querySelector<HTMLButtonElement>('.ramp-remove');
  const target = prefer === 'remove' && remove && !remove.disabled ? remove : li.querySelector<HTMLInputElement>('.ramp-after');
  target?.focus();
}

/**
 * Append a step a week after the last one. It takes whatever share is left;
 * when the shares already total 100% it splits the last step's share in two
 * (halving is exact in binary, so the total stays exactly the same).
 */
function addStep(): void {
  const steps = readRows();
  const last = steps[steps.length - 1];
  const used = steps.reduce((sum, s) => sum + (Number.isNaN(s.fraction) ? 0 : s.fraction), 0);
  const left = Number((1 - used).toPrecision(12));
  let fraction = left > 0 ? left : 0;
  if (fraction === 0 && last && last.fraction > 0) {
    last.fraction /= 2;
    fraction = last.fraction;
  }
  steps.push({ after_days: last && !Number.isNaN(last.after_days) ? last.after_days + 7 : 0, fraction });
  renderRows(steps);
  syncFromRows();
  const input = rows()[steps.length - 1]?.querySelector<HTMLInputElement>('.ramp-after');
  input?.focus();
  input?.select();
}

function removeStep(index: number): void {
  const steps = readRows();
  steps.splice(index, 1);
  renderRows(steps);
  syncFromRows();
  focusRow(Math.min(index, steps.length - 1), 'remove');
}

function onInput(e: Event): void {
  const target = e.target as HTMLElement;
  if (target.id === ID.textarea) {
    syncFromJson();
    return;
  }
  if (target.classList.contains('ramp-share')) {
    const li = target.closest<HTMLLIElement>('li.ramp-step');
    const fraction = percentToFraction((target as HTMLInputElement).value);
    if (li) li.dataset['fraction'] = Number.isNaN(fraction) ? '' : String(fraction);
  }
  if (target.classList.contains('ramp-share') || target.classList.contains('ramp-after')) syncFromRows();
}

function onClick(e: Event): void {
  const button = (e.target as HTMLElement).closest<HTMLButtonElement>('button');
  if (!button) return;
  if (button.id === ID.toggle) {
    setJsonOpen(button.getAttribute('aria-expanded') !== 'true');
  } else if (button.id === ID.add) {
    addStep();
  } else if (button.classList.contains('ramp-remove')) {
    removeStep(Number(button.dataset['index']));
  } else if (button.dataset['preset']) {
    const preset = RAMP_PRESETS.find(p => p.id === button.dataset['preset']);
    if (preset) {
      renderRows(preset.steps);
      syncFromRows();
    }
  }
}

/**
 * Wire the editor inside `root` (the freshly cloned config form) and draw the
 * rows from the JSON the modal has already put in the textarea.
 */
export function initRampEditor(root: HTMLElement): void {
  const editor = root.querySelector<HTMLElement>('.ramp-editor');
  if (!editor) return;
  editor.addEventListener('input', onInput);
  editor.addEventListener('click', onClick);
  rampBase = {};
  setJsonOpen(false);
  if (!syncFromJson()) {
    // A stored value the rows cannot show: start in the JSON view.
    renderRows([]);
    renderBar([], false);
    const summary = byId(ID.summary);
    if (summary) summary.textContent = 'Fix the JSON below to edit the steps here.';
    setJsonOpen(true);
  }
}

/** Show a JSON error from the save path under the textarea, revealing it. */
export function showRampJsonError(message: string): void {
  setJsonOpen(true);
  setJsonError(message);
  byId(ID.textarea)?.focus();
}

/**
 * Why the ramp schedule cannot be saved, with the control to focus, or null
 * when the backend would accept it. Checks the rows first (a value they
 * cannot write yet), then the JSON the save sends.
 */
export function rampScheduleProblem(): { message: string; focus: HTMLElement | null } | null {
  // Rows that are all writable mirror the JSON, so redraw them from it (the
  // JSON is what the save sends); a JSON shape error stops here.
  if (!hasGap(readRows()) && !syncFromJson()) {
    setJsonOpen(true);
    return { message: byId(ID.jsonError)?.textContent ?? 'Not valid JSON.', focus: byId(ID.textarea) };
  }
  const steps = readRows();
  const issue = showState(steps)[0];
  if (!issue) return null;
  if (issue.step === null) {
    const firstShare = rows()[0]?.querySelector<HTMLElement>('.ramp-share') ?? null;
    return { message: issue.message, focus: firstShare ?? byId(ID.add) };
  }
  const message = `Step ${issue.step + 1}: ${issue.message.charAt(0).toLowerCase()}${issue.message.slice(1)}`;
  const input = rows()[issue.step]?.querySelector<HTMLElement>(issue.field === 'share' ? '.ramp-share' : '.ramp-after');
  return { message, focus: input ?? null };
}
