/**
 * Issue #727.
 * UX-33: user form validation must be field-level (message under the
 * field, aria-invalid + aria-describedby, focus on the first problem), not
 * toast-only.
 * UX-28: the categorical filter popover's footer button selects nothing,
 * so it is labelled "Select none"; numeric filters keep "Clear".
 */
import './setup';

jest.mock('../api', () => ({
  createUser: jest.fn(),
  updateUser: jest.fn(),
  getUser: jest.fn(),
  listUsers: jest.fn(),
  listGroups: jest.fn(),
}));

jest.mock('../users/userActions', () => ({
  loadUsers: jest.fn().mockResolvedValue(undefined),
}));

import * as api from '../api';
import * as userState from '../users/state';
import { openCreateUserModal, openEditUserModal, saveUser } from '../users/userModals';
import { clearFieldError, clearFieldErrors, showFieldError } from '../lib/field-errors';
import { openHistoryColumnPopover, closeOpenHistoryPopover } from '../lib/history-filter-popover';

const GROUPS = [
  { id: 'w1', name: 'Writers', description: '', permissions: [{ action: 'update', resource: 'plans' }] },
  { id: 'v1', name: 'Viewers', description: '', permissions: [{ action: 'view', resource: '*' }] },
];

function setUpUserForm(): void {
  // Mirrors index.html's #user-modal, including the novalidate the fix adds.
  document.body.innerHTML = `
    <div id="user-modal" class="modal hidden">
      <h2 id="user-modal-title"></h2>
      <form id="user-form" novalidate>
        <input type="hidden" id="user-id">
        <div class="form-group"><input type="email" id="user-email" required></div>
        <div id="password-fields" class="form-group">
          <input type="password" id="user-password" minlength="12">
          <small class="form-help">help</small>
        </div>
        <div class="form-group">
          <select id="user-groups" multiple required></select>
          <small class="form-help">At least one group is required.</small>
        </div>
        <button type="submit">Save User</button>
      </form>
    </div>`;
}

const $ = <T extends HTMLElement>(id: string): T => document.getElementById(id) as T;

function errorFor(control: HTMLElement): string | null | undefined {
  const id = control.dataset['fieldErrorId'];
  return id ? document.getElementById(id)?.textContent : null;
}

async function submit(): Promise<void> {
  const e = new Event('submit');
  e.preventDefault = jest.fn();
  await saveUser(e);
}

function selectGroups(...ids: string[]): void {
  Array.from($<HTMLSelectElement>('user-groups').options).forEach(o => { o.selected = ids.includes(o.value); });
}

describe('lib/field-errors', () => {
  beforeEach(() => {
    document.body.innerHTML = '<div class="form-row"><label><input id="amt" aria-describedby="amt-help"></label></div><small id="amt-help">h</small>';
  });

  it('places the message after the anchor and links it without dropping existing help', () => {
    const input = $<HTMLInputElement>('amt');
    showFieldError(input, 'Too small.', input.closest('.form-row')!);
    const id = input.dataset['fieldErrorId']!;
    const note = document.getElementById(id)!;
    expect(note.previousElementSibling?.classList.contains('form-row')).toBe(true);
    expect(note.className).toBe('field-error');
    expect(input.getAttribute('aria-invalid')).toBe('true');
    expect(input.getAttribute('aria-describedby')).toBe(`amt-help ${id}`);
  });

  it('updates in place, and clears on the next edit, restoring aria-describedby', () => {
    const input = $<HTMLInputElement>('amt');
    showFieldError(input, 'First.');
    showFieldError(input, 'Second.');
    expect(document.querySelectorAll('.field-error')).toHaveLength(1);
    expect(errorFor(input)).toBe('Second.');
    input.dispatchEvent(new Event('input'));
    expect(document.querySelector('.field-error')).toBeNull();
    expect(input.hasAttribute('aria-invalid')).toBe(false);
    expect(input.getAttribute('aria-describedby')).toBe('amt-help');
  });

  it('clearFieldError and clearFieldErrors are safe on untouched fields', () => {
    const input = $<HTMLInputElement>('amt');
    expect(() => clearFieldError(input)).not.toThrow();
    showFieldError(input, 'x');
    clearFieldErrors(document);
    expect(document.querySelector('.field-error')).toBeNull();
  });
});

describe('user form field-level validation (#727 UX-33)', () => {
  beforeEach(() => {
    setUpUserForm();
    jest.clearAllMocks();
    userState.setAvailableGroups(GROUPS as api.APIGroup[]);
    userState.setCurrentEditingUser(null);
    (api.createUser as jest.Mock).mockResolvedValue({});
    (api.updateUser as jest.Mock).mockResolvedValue({});
  });

  it('marks every problem at once and focuses the first', async () => {
    openCreateUserModal();
    $<HTMLInputElement>('user-email').value = 'not-an-email';
    $<HTMLInputElement>('user-password').value = 'short';
    await submit();

    expect(api.createUser).not.toHaveBeenCalled();
    const email = $<HTMLInputElement>('user-email');
    const password = $<HTMLInputElement>('user-password');
    const groups = $<HTMLSelectElement>('user-groups');
    expect(errorFor(email)).toBe('Enter a valid email address, like name@example.com.');
    expect(errorFor(password)).toBe('Password must be at least 12 characters long');
    expect(errorFor(groups)).toBe('At least one group is required. Select one or more groups for this user.');
    for (const el of [email, password, groups]) {
      expect(el.getAttribute('aria-invalid')).toBe('true');
      expect(el.getAttribute('aria-describedby')).toBe(el.dataset['fieldErrorId']);
    }
    expect(document.activeElement).toBe(email);
  });

  it('asks for an email when it is blank (no native bubble with novalidate)', async () => {
    openCreateUserModal();
    selectGroups('w1');
    await submit();
    expect(api.createUser).not.toHaveBeenCalled();
    expect(errorFor($('user-email'))).toBe('Enter an email address.');
    expect(document.activeElement).toBe($('user-email'));
  });

  it('puts the contradictory-groups message on the groups field and still toasts it', async () => {
    openCreateUserModal();
    $<HTMLInputElement>('user-email').value = 'a@example.com';
    selectGroups('w1', 'v1');
    await submit();
    expect(api.createUser).not.toHaveBeenCalled();
    expect(errorFor($('user-groups'))).toContain('Contradictory');
    expect(document.querySelector('.toast-error .toast-message')?.textContent).toContain('Contradictory');
    expect(document.activeElement).toBe($('user-groups'));
  });

  it('clears a field message when the field is edited, then saves', async () => {
    openCreateUserModal();
    $<HTMLInputElement>('user-email').value = 'a@example.com';
    await submit();
    const groups = $<HTMLSelectElement>('user-groups');
    expect(groups.getAttribute('aria-invalid')).toBe('true');
    selectGroups('w1');
    groups.dispatchEvent(new Event('change'));
    expect(groups.hasAttribute('aria-invalid')).toBe(false);
    expect(document.querySelector('.field-error')).toBeNull();
    await submit();
    expect(api.createUser).toHaveBeenCalledWith({ email: 'a@example.com', password: '', groups: ['w1'] });
  });

  it('does not check the hidden password field when editing', async () => {
    (api.getUser as jest.Mock).mockResolvedValue({ id: 'u1', email: 'a@example.com', groups: ['w1'], mfa_enabled: false });
    await openEditUserModal('u1');
    $<HTMLInputElement>('user-password').value = 'short';
    await submit();
    expect(api.updateUser).toHaveBeenCalledWith('u1', { email: 'a@example.com', groups: ['w1'] });
    expect(document.querySelector('.field-error')).toBeNull();
  });

  it('reopening the modal clears old messages', async () => {
    openCreateUserModal();
    await submit();
    expect(document.querySelectorAll('.field-error').length).toBeGreaterThan(0);
    openCreateUserModal();
    expect(document.querySelector('.field-error')).toBeNull();
    expect(document.querySelector('[aria-invalid="true"]')).toBeNull();
  });
});

describe('history filter popover footer label (#727 UX-28)', () => {
  afterEach(() => closeOpenHistoryPopover());

  function open(kind: 'numeric' | 'categorical', onCommit = jest.fn()): HTMLButtonElement {
    document.body.innerHTML = '<button id="anchor"></button>';
    openHistoryColumnPopover({
      scope: 'history', column: 'provider', anchor: $('anchor'), currentFilter: undefined, headerLabel: 'Provider', kind,
      distinctValues: kind === 'categorical' ? ['aws', 'gcp'] : undefined, onCommit,
    });
    return document.querySelector<HTMLButtonElement>('.column-filter-clear')!;
  }

  it('labels the categorical button "Select none", which selects nothing', () => {
    const onCommit = jest.fn();
    const btn = open('categorical', onCommit);
    expect(btn.textContent).toBe('Select none');
    btn.click();
    expect(onCommit).toHaveBeenLastCalledWith({ kind: 'set', values: [] });
  });

  it('keeps "Clear" for numeric filters, which removes the filter', () => {
    const onCommit = jest.fn();
    const btn = open('numeric', onCommit);
    expect(btn.textContent).toBe('Clear');
    btn.click();
    expect(onCommit).toHaveBeenLastCalledWith(null);
  });
});
