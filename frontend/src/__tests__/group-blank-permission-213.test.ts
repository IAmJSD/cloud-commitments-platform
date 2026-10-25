/**
 * Issue #213: a group holding a permission with a blank action or resource
 * could not be saved, with no explanation. These tests drive the real form
 * through a real `submit` event (requestSubmit) so constraint validation and
 * the runtime wiring are exercised, not just a direct saveGroup(event) call.
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
import { showSuccess } from '../users/utils';

const updateGroup = api.updateGroup as jest.Mock;

function setUpModalDom(): void {
  // Mirrors index.html, including `required` on the name and the
  // novalidate attribute the fix adds.
  document.body.innerHTML = `
    <div id="group-modal" class="hidden">
      <span id="group-modal-title"></span>
      <form id="group-form" novalidate>
        <input type="hidden" id="group-id" />
        <div class="form-group"><input type="text" id="group-name" required /></div>
        <textarea id="group-description"></textarea>
        <div id="permissions-list"></div>
        <button type="submit">Save Group</button>
      </form>
    </div>
  `;
}

function group(permissions: api.Permission[]): api.APIGroup {
  return { id: 'g1', name: 'Finance', description: '', permissions, created_at: '2024-01-01T00:00:00Z' };
}

function rows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('#permissions-list .permission-item'));
}

function select(row: HTMLElement, cls: 'perm-action' | 'perm-resource'): HTMLSelectElement {
  return row.querySelector<HTMLSelectElement>(`.${cls}`)!;
}

function choose(sel: HTMLSelectElement, value: string): void {
  sel.value = value;
  sel.dispatchEvent(new Event('change', { bubbles: true }));
}

async function clickSave(): Promise<void> {
  (document.getElementById('group-form') as HTMLFormElement).requestSubmit();
  await new Promise(r => setTimeout(r, 0));
}

async function openWith(permissions: api.Permission[]): Promise<void> {
  (api.getGroup as jest.Mock).mockResolvedValue(group(permissions));
  await groupModals.openEditGroupModal('g1');
}

beforeEach(() => {
  setUpModalDom();
  groupState.setCurrentEditingGroup(null);
  jest.clearAllMocks();
  updateGroup.mockResolvedValue({});
  wireGroupForm((e) => void groupModals.saveGroup(e));
});

describe('group form with a blank permission resource (#213)', () => {
  it('renders the stored blank resource as a selected placeholder, never as All (*)', async () => {
    await openWith([{ action: 'view', resource: '' }]);
    const resource = select(rows()[0]!, 'perm-resource');
    expect(resource.value).toBe('');
    expect(resource.options[0]!.textContent).toBe('Select Resource');
    expect(resource.options[0]!.selected).toBe(true);
    expect(resource.options[0]!.disabled).toBe(false);
  });

  it('Save refuses the row with a note under it, marks the select and focuses it', async () => {
    await openWith([{ action: 'view', resource: 'plans' }, { action: 'view', resource: '', constraints: { accounts: ['acct-A'] } }]);
    await clickSave();

    expect(updateGroup).not.toHaveBeenCalled();
    const row = rows()[1]!;
    const resource = select(row, 'perm-resource');
    const note = row.querySelector('.field-error') as HTMLElement;
    expect(note.textContent).toBe('This permission has no resource. Choose one, or remove the permission.');
    // After the whole flex row, not inside it between two controls.
    expect(note.previousElementSibling?.classList.contains('form-row')).toBe(true);
    expect(note.parentElement).toBe(row);
    expect(resource.getAttribute('aria-invalid')).toBe('true');
    expect(resource.getAttribute('aria-describedby')).toBe(note.id);
    expect(select(row, 'perm-action').hasAttribute('aria-invalid')).toBe(false);
    expect(document.activeElement).toBe(resource);
    expect(rows()[0]!.querySelector('.field-error')).toBeNull();
  });

  it('the note goes away when a resource is picked and comes back on the placeholder', async () => {
    await openWith([{ action: 'view', resource: '' }]);
    await clickSave();
    const row = rows()[0]!;
    const resource = select(row, 'perm-resource');

    choose(resource, 'plans');
    expect(row.querySelector('.field-error')).toBeNull();
    expect(resource.hasAttribute('aria-invalid')).toBe(false);

    choose(resource, '');
    expect(row.querySelector('.field-error')).not.toBeNull();
    expect(resource.getAttribute('aria-invalid')).toBe('true');
  });

  it('after picking a resource the save sends every row, constraints intact', async () => {
    await openWith([{ action: 'view', resource: 'plans' }, { action: 'view', resource: '', constraints: { accounts: ['acct-A'] } }]);
    (document.getElementById('group-name') as HTMLInputElement).value = 'Finance (renamed)';
    await clickSave();
    choose(select(rows()[1]!, 'perm-resource'), 'purchases');
    await clickSave();

    expect(updateGroup).toHaveBeenCalledTimes(1);
    expect(updateGroup).toHaveBeenCalledWith('g1', {
      name: 'Finance (renamed)',
      description: '',
      permissions: [
        { action: 'view', resource: 'plans' },
        { action: 'view', resource: 'purchases', constraints: { accounts: ['acct-A'] } },
      ],
    });
    expect(showSuccess).toHaveBeenCalled();
  });

  it('removing the blank row lets a pure rename save the rest unchanged', async () => {
    await openWith([{ action: 'view', resource: 'plans' }, { action: 'view', resource: '' }]);
    (rows()[1]!.querySelector('.remove-permission-btn') as HTMLButtonElement).click();
    await clickSave();
    expect(updateGroup).toHaveBeenCalledWith('g1', expect.objectContaining({
      permissions: [{ action: 'view', resource: 'plans' }],
    }));
  });

  it('a whitespace-only resource takes the same path as an empty one', async () => {
    await openWith([{ action: 'view', resource: '   ' }]);
    const resource = select(rows()[0]!, 'perm-resource');
    expect(resource.value).toBe('');
    expect(Array.from(resource.options).some(o => o.textContent?.includes('not recognized'))).toBe(false);
    await clickSave();
    expect(updateGroup).not.toHaveBeenCalled();
    expect(rows()[0]!.querySelector('.field-error')).not.toBeNull();
  });

  it('a blank action is refused the same way', async () => {
    await openWith([{ action: '', resource: 'plans' }]);
    const action = select(rows()[0]!, 'perm-action');
    expect(action.value).toBe('');
    await clickSave();
    expect(updateGroup).not.toHaveBeenCalled();
    expect(rows()[0]!.querySelector('.field-error')?.textContent)
      .toBe('This permission has no action. Choose one, or remove the permission.');
    expect(action.getAttribute('aria-invalid')).toBe('true');
    expect(document.activeElement).toBe(action);
  });

  it('a new row still defaults to All (*)', () => {
    groupModals.openCreateGroupModal();
    groupModals.addPermission();
    expect(select(rows()[0]!, 'perm-resource').value).toBe('*');
  });

  it('a blank name is refused with a note instead of the native bubble', async () => {
    await openWith([{ action: 'view', resource: 'plans' }]);
    const name = document.getElementById('group-name') as HTMLInputElement;
    name.value = '  ';
    await clickSave();
    expect(updateGroup).not.toHaveBeenCalled();
    expect(document.getElementById('group-name-error')?.textContent).toBe('Enter a group name.');
    expect(name.getAttribute('aria-invalid')).toBe('true');
    expect(document.activeElement).toBe(name);
  });

  it('reopening the modal clears earlier notes', async () => {
    await openWith([{ action: 'view', resource: '' }]);
    await clickSave();
    expect(document.querySelector('.field-error')).not.toBeNull();
    await openWith([{ action: 'view', resource: 'plans' }]);
    expect(document.querySelector('.field-error')).toBeNull();
    expect(document.querySelector('[aria-invalid="true"]')).toBeNull();
  });
});

describe('group form wiring (#213, audit A11-001)', () => {
  it('binds submit once even when wired twice, and turns off native validation', async () => {
    wireGroupForm((e) => void groupModals.saveGroup(e));
    await openWith([{ action: 'view', resource: 'plans' }]);
    const form = document.getElementById('group-form') as HTMLFormElement;
    expect(form.noValidate).toBe(true);
    await clickSave();
    expect(updateGroup).toHaveBeenCalledTimes(1);
  });
});
