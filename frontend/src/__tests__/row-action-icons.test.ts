/**
 * Icon-only row actions on the admin page (issue #751 review feedback).
 *
 * API key Revoke/Delete and the group editor's permission Remove used to be
 * chunky text buttons. They now render an SVG from the shared icon set, so
 * the accessible name has to come from aria-label. These tests pin the icon,
 * the name a screen reader (and getByRole) sees, the tooltip, and that the
 * click handlers still fire.
 */
import { getByRole, queryByRole } from '@testing-library/dom';
import { loadApiKeys } from '../apikeys';
import { addPermission } from '../groups/groupModals';

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
