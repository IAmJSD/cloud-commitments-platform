/**
 * Icon-only row actions on the admin page (issue #751 review feedback).
 *
 * API key Revoke/Delete, the group editor's permission Remove, and the
 * users/groups Edit, Duplicate and Delete actions used to be
 * chunky text buttons. They now render an SVG from the shared icon set, so
 * the accessible name has to come from aria-label. These tests pin the icon,
 * the name a screen reader (and getByRole) sees, the tooltip, and that the
 * click handlers still fire.
 */
import { getByRole, queryByRole } from '@testing-library/dom';
import { loadApiKeys } from '../apikeys';
import { addPermission } from '../groups/groupModals';
import { renderGroups } from '../groups/groupList';
import { renderUsers } from '../users/userList';
import * as userState from '../users/state';

jest.mock('../api', () => ({
  getApiKeys: jest.fn(),
  getApiKeysUsageStats: jest.fn(),
  revokeApiKey: jest.fn(),
  deleteApiKey: jest.fn(),
}));

const mockConfirmDialog = jest.fn<Promise<boolean>, [unknown]>(() => Promise.resolve(true));
jest.mock('../confirmDialog', () => ({
  confirmDialog: (opts: unknown) => mockConfirmDialog(opts),
}));

import * as api from '../api';

function expectIconButton(btn: HTMLElement, icon: string, name: string): void {
  expect(btn.classList.contains('icon-btn')).toBe(true);
  expect(btn.querySelector(`svg.icon.icon-${icon}`)).not.toBeNull();
  // No visible text: the name comes only from aria-label, the tooltip from title.
  expect(btn.textContent?.trim()).toBe('');
  expect(btn.getAttribute('aria-label')).toBe(name);
  expect(btn.getAttribute('title')).toBe(name);
}

describe('API key row actions', () => {
  beforeEach(() => {
    document.body.innerHTML = '<div id="apikeys-usage-summary"></div><div id="apikeys-list"></div>';
    (api.getApiKeysUsageStats as jest.Mock).mockResolvedValue({
      total_active: 1, total_requests_window: 0, total_requests_lifetime: 0, top_keys: [],
    });
    (api.getApiKeys as jest.Mock).mockResolvedValue({
      api_keys: [
        { id: 'k-active', name: 'CI', key_prefix: 'abc', is_active: true, created_at: '2026-01-01T00:00:00Z' },
        { id: 'k-revoked', name: 'Old', key_prefix: 'def', is_active: false, created_at: '2026-01-01T00:00:00Z' },
      ],
    });
    mockConfirmDialog.mockClear();
  });

  it('render as slash and trash icon buttons with accessible names', async () => {
    await loadApiKeys();
    const rows = document.querySelectorAll<HTMLElement>('#apikeys-list tbody tr');
    expect(rows).toHaveLength(2);

    const revoke = getByRole(rows[0]!, 'button', { name: 'Revoke API key' });
    const del = getByRole(rows[0]!, 'button', { name: 'Delete API key' });
    expectIconButton(revoke, 'slash', 'Revoke API key');
    expectIconButton(del, 'trash-2', 'Delete API key');
    // Warning/danger colour variants are kept.
    expect(revoke.classList.contains('btn-warning')).toBe(true);
    expect(del.classList.contains('btn-danger')).toBe(true);
    // Side by side in one non-wrapping group.
    expect(revoke.parentElement).toBe(del.parentElement);
    expect(revoke.parentElement?.classList.contains('row-actions')).toBe(true);

    // A revoked key offers only Delete.
    expect(queryByRole(rows[1]!, 'button', { name: /Revoke/ })).toBeNull();
    expect(getByRole(rows[1]!, 'button', { name: 'Delete API key' })).toBeTruthy();
  });

  it('keep their data attributes and confirm-then-act handlers', async () => {
    (api.revokeApiKey as jest.Mock).mockResolvedValue({});
    (api.deleteApiKey as jest.Mock).mockResolvedValue({});
    await loadApiKeys();
    const row = document.querySelector<HTMLElement>('#apikeys-list tbody tr')!;
    const revoke = getByRole(row, 'button', { name: /Revoke/ });
    expect(revoke.dataset['keyId']).toBe('k-active');

    revoke.click();
    await new Promise(resolve => setTimeout(resolve, 0));
    expect(mockConfirmDialog).toHaveBeenCalledTimes(1);
    expect(api.revokeApiKey).toHaveBeenCalledWith('k-active');

    getByRole(row, 'button', { name: /Delete/ }).click();
    await new Promise(resolve => setTimeout(resolve, 0));
    expect(mockConfirmDialog).toHaveBeenCalledTimes(2);
    expect(api.deleteApiKey).toHaveBeenCalledWith('k-active');
  });
});

describe('group editor permission Remove', () => {
  beforeEach(() => {
    document.body.innerHTML = '<div id="permissions-list"></div>';
  });

  it('is a trash icon button named "Remove permission" that still removes its row', () => {
    addPermission({ action: 'view', resource: 'plans' });
    addPermission({ action: 'update', resource: 'purchases' });
    const list = document.getElementById('permissions-list')!;
    expect(list.querySelectorAll('.permission-item')).toHaveLength(2);

    const first = list.querySelector<HTMLElement>('.permission-item')!;
    const remove = getByRole(first, 'button', { name: 'Remove permission' });
    expectIconButton(remove, 'trash-2', 'Remove permission');
    expect(remove.classList.contains('btn-danger')).toBe(true);
    expect(remove.classList.contains('remove-permission-btn')).toBe(true);

    remove.click();
    expect(list.querySelectorAll('.permission-item')).toHaveLength(1);
    expect(list.querySelector<HTMLSelectElement>('.perm-action')?.value).toBe('update');
  });
});

describe('users table and group card actions', () => {
  afterEach(() => {
    document.body.replaceChildren();
  });

  it('users Edit/Delete are pencil and trash icon buttons with per-user names', () => {
    document.body.innerHTML = '<div id="users-list"></div>';
    renderUsers([
      { id: 'u1', email: 'alice@example.com', groups: [], mfa_enabled: false },
    ] as unknown as Parameters<typeof renderUsers>[0]);
    const row = document.querySelector<HTMLElement>('#users-list tbody tr')!;
    const edit = getByRole(row, 'button', { name: 'Edit user alice@example.com' });
    const del = getByRole(row, 'button', { name: 'Delete user alice@example.com' });
    expect(edit.querySelector('svg.icon-edit')).not.toBeNull();
    expect(del.querySelector('svg.icon-trash-2')).not.toBeNull();
    expect(edit.textContent?.trim()).toBe('');
    expect(del.textContent?.trim()).toBe('');
    expect(edit.getAttribute('title')).toBe('Edit user');
    expect(del.getAttribute('title')).toBe('Delete user');
    expect(del.classList.contains('btn-danger')).toBe(true);
    expect(edit.parentElement?.classList.contains('row-actions')).toBe(true);
  });

  it('group Edit/Duplicate/Delete are icon buttons named after the group', () => {
    document.body.innerHTML = '<div id="groups-list"></div>';
    userState.setAllUsers([]);
    renderGroups([
      { id: 'g1', name: 'Ops', description: '', permissions: [], allowed_accounts: [] },
    ] as unknown as Parameters<typeof renderGroups>[0]);
    const card = document.querySelector<HTMLElement>('#groups-list .group-card')!;
    const cases: Array<[string, string, string, string]> = [
      ['Edit group Ops', 'edit', 'Edit group', 'edit-group-btn'],
      ['Duplicate group Ops', 'copy', 'Duplicate group', 'duplicate-group-btn'],
      ['Delete group Ops', 'trash-2', 'Delete group', 'delete-group-btn'],
    ];
    for (const [name, icon, title, cls] of cases) {
      const btn = getByRole(card, 'button', { name });
      expect(btn.querySelector(`svg.icon-${icon}`)).not.toBeNull();
      expect(btn.textContent?.trim()).toBe('');
      expect(btn.getAttribute('title')).toBe(title);
      expect(btn.classList.contains(cls)).toBe(true);
      expect(btn.dataset['groupId']).toBe('g1');
    }
  });
});
