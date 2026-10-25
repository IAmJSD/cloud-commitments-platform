/**
 * Field-level validation messages for modal forms (issue #727, UX-33).
 *
 * The user and group forms used to report a refused save only through a
 * toast: it fades after a few seconds, sits away from the field, and gives
 * a screen-reader user no cue on the control itself. showFieldError puts
 * the message under the field (a `.field-error`, styled in base.css), sets
 * aria-invalid and links the message with aria-describedby (appended, so
 * existing help text stays announced). The next edit of the field clears it.
 */

let seq = 0;

function errorIdFor(control: HTMLElement): string {
  if (!control.dataset['fieldErrorId']) {
    control.dataset['fieldErrorId'] = `${control.id || 'field'}-error-${++seq}`;
  }
  return control.dataset['fieldErrorId'];
}

function removeToken(list: string | null, token: string): string {
  return (list ?? '').split(/\s+/).filter(t => t && t !== token).join(' ');
}

/**
 * Show `message` under `control`. `anchor` is the element the message is
 * placed after; it defaults to the control itself. Pass the enclosing row
 * when the control sits in a flex row, so the message lands below the row
 * instead of becoming a flex item beside the control.
 */
export function showFieldError(control: HTMLElement, message: string, anchor?: Element): void {
  const id = errorIdFor(control);
  let note = document.getElementById(id);
  if (!note) {
    note = document.createElement('p');
    note.id = id;
    note.className = 'field-error';
    (anchor ?? control).insertAdjacentElement('afterend', note);
  }
  note.textContent = message;
  control.setAttribute('aria-invalid', 'true');
  const describedBy = removeToken(control.getAttribute('aria-describedby'), id);
  control.setAttribute('aria-describedby', describedBy ? `${describedBy} ${id}` : id);

  if (control.dataset['fieldErrorWired'] !== '1') {
    control.dataset['fieldErrorWired'] = '1';
    const clear = (): void => clearFieldError(control);
    control.addEventListener('input', clear);
    control.addEventListener('change', clear);
  }
}

/** Remove the message and invalid marker from `control`, if any. */
export function clearFieldError(control: HTMLElement): void {
  const id = control.dataset['fieldErrorId'];
  if (!id) return;
  document.getElementById(id)?.remove();
  control.removeAttribute('aria-invalid');
  const describedBy = removeToken(control.getAttribute('aria-describedby'), id);
  if (describedBy) control.setAttribute('aria-describedby', describedBy);
  else control.removeAttribute('aria-describedby');
}

/** Clear every field error raised by showFieldError inside `root`. */
export function clearFieldErrors(root: ParentNode): void {
  root.querySelectorAll<HTMLElement>('[data-field-error-id]').forEach(clearFieldError);
}
