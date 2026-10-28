/**
 * Issue #727 (UX-33), group form: the refusals that follow the blank-field
 * checks (a bad Max Amount, an unrepresentable constraint, no permissions at
 * all) used to be toast-only. Each now also shows under the field to fix,
 * through lib/field-errors: aria-invalid, aria-describedby, focus on the
 * first problem, and the next edit clears it. The toast stays as a summary.
 */
import './setup';

jest.mock('../api', () => ({
  getGroup: jest.fn(),
  updateGroup: jest.fn(),
  createGroup: jest.fn(),
}));

jest.mock('../users/userActions', () => ({
  loadUsers: jest.fn().mockResolvedValue(undefined),
}));

jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn().mockResolvedValue(true),
}));

jest.mock('../users/utils', () => ({
  ...jest.requireActual('../users/utils'),
  showError: jest.fn(),
  showSuccess: jest.fn(),
}));

import * as api from '../api';
import * as groupState from '../groups/state';
import * as groupModals from '../groups/groupModals';
import { wireGroupForm } from '../groups/groupFormValidation';
import { showError } from '../users/utils';

function setUpModalDom(): void {
  // Mirrors index.html's #group-modal, including the Add Permission button.
  document.body.innerHTML = `
    <div id="group-modal" class="hidden">
      <span id="group-modal-title"></span>
      <form id="group-form" novalidate>
        <input type="hidden" id="group-id" />
        <div class="form-group"><input type="text" id="group-name" required /></div>
        <textarea id="group-description"></textarea>
        <div class="form-group">
          <div id="permissions-list"></div>
          <button type="button" id="add-permission-btn">Add Permission</button>
        </div>
        <button type="submit">Save Group</button>
      </form>
    </div>
  `;
  wireGroupForm(e => { void groupModals.saveGroup(e); });
}

async function openWith(permissions: api.Permission[]): Promise<void> {
  (api.getGroup as jest.Mock).mockResolvedValue({
    id: 'g1', name: 'Finance', description: '', permissions, created_at: '2024-01-01T00:00:00Z',
  });
  await groupModals.openEditGroupModal('g1');
}

async function clickSave(): Promise<void> {
  (document.getElementById('group-form') as HTMLFormElement).requestSubmit();
  await new Promise(r => setTimeout(r, 0));
}

function input(cls: string, row = 0): HTMLInputElement {
  return document.querySelectorAll('#permissions-list .permission-item')[row]!.querySelector<HTMLInputElement>(`.${cls}`)!;
}

/** The inline message linked to `control`, or null when it has none. */
function inlineError(control: HTMLElement): string | null {
  if (control.getAttribute('aria-invalid') !== 'true') return null;
  const ids = (control.getAttribute('aria-describedby') ?? '').split(/\s+/);
  const note = ids.map(id => document.getElementById(id)).find(el => el?.classList.contains('field-error'));
  return note?.textContent ?? null;
}

function type(control: HTMLInputElement, value: string): void {
  control.value = value;
  control.dispatchEvent(new Event('input', { bubbles: true }));
}

describe('group form refusals show under the field (issue #727)', () => {
  beforeEach(() => {
    setUpModalDom();
    groupState.setCurrentEditingGroup(null);
    jest.clearAllMocks();
  });

  test('a Max Amount of 0 is marked on that input, focused, and cleared by the next edit', async () => {
    await openWith([
      { action: 'view', resource: 'plans' },
      { action: 'update-any', resource: 'purchases', constraints: { max_amount: 5000 } },
    ]);
    const amount = input('perm-max-amount', 1);
    amount.value = '0';
    await clickSave();

    expect(api.updateGroup).not.toHaveBeenCalled();
    expect(inlineError(amount)).toBe('Enter an amount greater than 0, or leave it blank for no limit.');
    expect(inlineError(input('perm-max-amount', 0))).toBeNull();
    expect(document.activeElement).toBe(amount);
    // The note sits below the flex .form-row, not beside the input.
    const note = document.getElementById(amount.getAttribute('aria-describedby')!)!;
    expect(note.previousElementSibling).toBe(amount.closest('.form-row'));
    // The toast summary still names the row.
    expect(showError).toHaveBeenCalledWith(expect.stringContaining('(permission 1)'));

    type(amount, '250');
    expect(inlineError(amount)).toBeNull();
    expect(document.querySelectorAll('.field-error')).toHaveLength(0);
  });

  test('a typed constraint with no usable entries is marked on that input', async () => {
    await openWith([{ action: 'view', resource: 'history', constraints: { regions: ['us-east-1'] } }]);
    const regions = input('perm-regions');
    regions.value = ' , ';
    await clickSave();

    expect(api.updateGroup).not.toHaveBeenCalled();
    expect(inlineError(regions)).toBe('This list has no usable entries. Enter at least one value, or clear the box for no restriction.');
    expect(inlineError(input('perm-accounts'))).toBeNull();
    expect(document.activeElement).toBe(regions);
    expect(showError).toHaveBeenCalledWith(expect.stringContaining('"regions" value with no usable entries'));
  });

  test('a stored constraint value the form cannot represent is marked on each affected input', async () => {
    await openWith([{ action: 'view', resource: 'history', constraints: { accounts: [' acct A '], services: [''] } }]);
    await clickSave();

    expect(api.updateGroup).not.toHaveBeenCalled();
    for (const cls of ['perm-accounts', 'perm-services']) {
      expect(inlineError(input(cls))).toMatch(/^This stored value cannot be shown in this form/);
    }
    expect(inlineError(input('perm-regions'))).toBeNull();
    // One toast entry for the row, not one per input.
    const toast = (showError as jest.Mock).mock.calls[0][0] as string;
    expect(toast.match(/permission 0/g)).toHaveLength(1);
    expect(toast).toContain('"accounts, services"');
  });

  test('no permissions at all is marked at Add Permission, and adding a row clears it', async () => {
    await openWith([{ action: 'admin', resource: '*' }]);
    document.querySelector<HTMLButtonElement>('.remove-permission-btn')!.click();
    await clickSave();

    const add = document.getElementById('add-permission-btn')!;
    expect(api.updateGroup).not.toHaveBeenCalled();
    expect(inlineError(add)).toBe('Add at least one permission. To remove all privileges, delete the group instead.');
    expect(document.activeElement).toBe(add);
    expect(showError).toHaveBeenCalledWith(expect.stringContaining('at least one permission'));

    add.addEventListener('click', () => groupModals.addPermission());
    add.click();
    expect(inlineError(add)).toBeNull();
    expect(document.querySelectorAll('.field-error')).toHaveLength(0);
  });

  test('a fixed form saves and leaves no stale field errors behind', async () => {
    await openWith([{ action: 'update-any', resource: 'purchases', constraints: { max_amount: 5000 } }]);
    const amount = input('perm-max-amount');
    amount.value = '-1';
    await clickSave();
    expect(inlineError(amount)).not.toBeNull();

    amount.value = '100';
    await clickSave();
    expect(api.updateGroup).toHaveBeenCalledTimes(1);
    expect(document.querySelectorAll('.field-error')).toHaveLength(0);
    expect(amount.hasAttribute('aria-invalid')).toBe(false);
  });

  test('reopening the modal clears field errors from a refused save', async () => {
    await openWith([{ action: 'update-any', resource: 'purchases', constraints: { max_amount: 5000 } }]);
    input('perm-max-amount').value = '0';
    await clickSave();
    expect(document.querySelectorAll('.field-error')).toHaveLength(1);

    groupModals.openCreateGroupModal();
    expect(document.querySelectorAll('.field-error')).toHaveLength(0);
    expect(document.getElementById('add-permission-btn')!.hasAttribute('aria-invalid')).toBe(false);
  });
});
