/**
 * Save wiring and blank-field validation for the group form (issue #213).
 *
 * Two defects made a group holding a blank permission row impossible to
 * save, with nothing on screen to say why:
 *
 *  - #group-form's submit was never bound at runtime (the only binder,
 *    setupGroupHandlers, is reached from tests alone), so Save fell
 *    through to a native form submission that reloaded the page and lost
 *    the edit (audit finding A11-001).
 *  - A stored blank action could only trip the browser's constraint
 *    validation, and a stored blank resource rendered as "All (*)", so a
 *    working Save would have widened it to every resource.
 *
 * The form is now `novalidate` and saveGroup runs every time. It refuses a
 * blank name or a permission row with no action or no resource, marks the
 * offending controls aria-invalid, puts a note under the row (after the
 * whole .form-row, not inside the flex row) and focuses the first problem.
 * Nothing is dropped or widened: the admin picks a value or removes the row.
 *
 * After a refused save the notes are recomputed on every change, both ways,
 * so re-selecting the placeholder brings the note back.
 */

import { clearFieldError, clearFieldErrors, showFieldError } from '../lib/field-errors';

const NOTE_CLASS = 'group-form-note';
const NAME_NOTE_ID = 'group-name-error';

let noteSeq = 0;

/** Blank after trimming. `undefined`/`null` (a brand-new row) is not blank. */
export function isBlankPermissionValue(value: string | null | undefined): boolean {
  return value !== undefined && value !== null && value.trim() === '';
}

function form(): HTMLFormElement | null {
  return document.getElementById('group-form') as HTMLFormElement | null;
}

function setInvalid(control: HTMLElement, noteId: string | null): void {
  if (noteId) {
    control.setAttribute('aria-invalid', 'true');
    control.setAttribute('aria-describedby', noteId);
  } else {
    control.removeAttribute('aria-invalid');
    control.removeAttribute('aria-describedby');
  }
}

function makeNote(text: string): HTMLElement {
  const note = document.createElement('p');
  note.className = `field-error ${NOTE_CLASS}`;
  note.id = `group-form-note-${++noteSeq}`;
  note.textContent = text;
  return note;
}

function rowMessage(actionBlank: boolean, resourceBlank: boolean): string {
  if (actionBlank && resourceBlank) {
    return 'This permission has no action and no resource. Choose both, or remove the permission.';
  }
  return actionBlank
    ? 'This permission has no action. Choose one, or remove the permission.'
    : 'This permission has no resource. Choose one, or remove the permission.';
}

/** Remove every note and invalid marker, and forget a refused save. */
export function clearGroupFormValidation(): void {
  const f = form();
  if (!f) return;
  clearFieldErrors(f);
  f.querySelectorAll(`.${NOTE_CLASS}`).forEach(n => n.remove());
  f.querySelectorAll('[aria-invalid="true"]').forEach(el => setInvalid(el as HTMLElement, null));
  delete f.dataset['validated'];
}

/**
 * A reason saveGroup refuses after the blank-field checks (issue #727): an
 * unrepresentable constraint, a bad Max Amount, or no permissions at all,
 * tied to the control that has to change.
 */
export interface GroupFieldProblem {
  control: HTMLElement;
  /** Shown under the field. */
  message: string;
  /** The same reason for the toast summary, naming the permission row. */
  summary: string;
}

/**
 * Show each problem under its field through lib/field-errors (aria-invalid,
 * aria-describedby, cleared by the next edit) and focus the first. Inside a
 * permission row the note goes after the flex .form-row, not beside the
 * input. The caller keeps its toast as the summary.
 */
export function showGroupFieldProblems(problems: readonly GroupFieldProblem[]): void {
  for (const p of problems) {
    showFieldError(p.control, p.message, p.control.closest('.form-row') ?? undefined);
  }
  problems[0]?.control.focus();
}

/** Drop the field problems of an earlier refused save before re-checking. */
export function clearGroupFieldProblems(): void {
  const f = form();
  if (f) clearFieldErrors(f);
}

/** A newly added permission row answers the "no permissions" refusal. */
export function clearNoPermissionsProblem(): void {
  const add = document.getElementById('add-permission-btn');
  if (add) clearFieldError(add);
}

/**
 * Check the name and every permission row, render the notes, and return the
 * first invalid control (null when the form can be saved). `focus` moves
 * focus there, which also scrolls it into view inside the modal.
 */
export function validateGroupForm(focus = true): HTMLElement | null {
  const f = form();
  if (!f) return null;
  f.querySelectorAll(`.${NOTE_CLASS}`).forEach(n => n.remove());
  f.dataset['validated'] = '1';

  let first: HTMLElement | null = null;

  const name = document.getElementById('group-name') as HTMLInputElement | null;
  if (name) {
    if (name.value.trim() === '') {
      const note = makeNote('Enter a group name.');
      note.id = NAME_NOTE_ID;
      name.insertAdjacentElement('afterend', note);
      setInvalid(name, note.id);
      first = name;
    } else {
      setInvalid(name, null);
    }
  }

  f.querySelectorAll<HTMLElement>('#permissions-list .permission-item').forEach(item => {
    const action = item.querySelector<HTMLSelectElement>('.perm-action');
    const resource = item.querySelector<HTMLSelectElement>('.perm-resource');
    const actionBlank = !!action && action.value.trim() === '';
    const resourceBlank = !!resource && resource.value.trim() === '';
    let noteId: string | null = null;
    if (actionBlank || resourceBlank) {
      const note = makeNote(rowMessage(actionBlank, resourceBlank));
      noteId = note.id;
      const row = item.querySelector('.form-row');
      if (row) row.insertAdjacentElement('afterend', note);
      else item.prepend(note);
    }
    if (action) setInvalid(action, actionBlank ? noteId : null);
    if (resource) setInvalid(resource, resourceBlank ? noteId : null);
    if (!first) first = actionBlank ? action : resourceBlank ? resource : null;
  });

  if (first && focus) (first as HTMLElement).focus();
  return first;
}

/**
 * Bind #group-form's submit to `onSubmit` once (idempotent, so the legacy
 * setupGroupHandlers path cannot double-save), turn off native constraint
 * validation, and keep the notes in step with the fields after a refused
 * save.
 */
export function wireGroupForm(onSubmit: (e: Event) => void): void {
  const f = form();
  if (!f || f.dataset['wired'] === '1') return;
  f.dataset['wired'] = '1';
  f.noValidate = true;
  f.addEventListener('submit', onSubmit);
  const recheck = (): void => {
    if (f.dataset['validated'] === '1') validateGroupForm(false);
  };
  f.addEventListener('change', recheck);
  f.addEventListener('input', recheck);
}
