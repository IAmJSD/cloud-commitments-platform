/**
 * Issue #919: system-managed groups must not offer Edit or Delete (the API
 * refuses both with 403), and a refused Delete must show the server's
 * reason rather than a generic "Failed to delete group".
 */
import './setup';

jest.mock('../api', () => ({
  deleteGroup: jest.fn(),
  getGroup: jest.fn(),
}));

jest.mock('../users/userActions', () => ({
  loadUsers: jest.fn().mockResolvedValue(undefined),
}));

const mockConfirmDialog = jest.fn<Promise<boolean>, [unknown]>(() => Promise.resolve(true));
jest.mock('../confirmDialog', () => ({
  confirmDialog: (opts: unknown) => mockConfirmDialog(opts),
}));

import * as api from '../api';
import type { APIGroup } from '../api';
import * as userState from '../users/state';
import { renderGroups } from '../groups/groupList';
import { deleteGroup } from '../groups/groupActions';
import { SYSTEM_GROUP_NOTE } from '../groups/systemGroups';

const SYSTEM: APIGroup = {
  id: '00000000-0000-5000-8000-000000000001', name: 'Administrators', description: '',
  permissions: [{ action: 'admin', resource: '*' }], system_managed: true,
};
const CUSTOM: APIGroup = {
  id: 'g-custom', name: 'Finance', description: '',
  permissions: [{ action: 'view', resource: 'plans' }],
};

function card(name: string): HTMLElement {
  return Array.from(document.querySelectorAll<HTMLElement>('.group-card'))
    .find(c => c.querySelector('h4')?.textContent === name)!;
}

beforeEach(() => {
  document.body.innerHTML = '<div id="groups-list"></div>';
  userState.setAllUsers([]);
  userState.setAvailableGroups([SYSTEM, CUSTOM]);
  jest.clearAllMocks();
  mockConfirmDialog.mockResolvedValue(true);
  jest.spyOn(console, 'error').mockImplementation(() => undefined);
});

afterEach(() => {
  jest.restoreAllMocks();
});

describe('group cards (#919)', () => {
  it('hides Edit and Delete on a system-managed group and says why', () => {
    renderGroups([SYSTEM, CUSTOM]);
    const sys = card('Administrators');
    expect(sys.querySelector('.edit-group-btn')).toBeNull();
    expect(sys.querySelector('.delete-group-btn')).toBeNull();
    // Duplicate still works: it makes an editable copy.
    expect(sys.querySelector('.duplicate-group-btn')).not.toBeNull();
    const badge = sys.querySelector('.system-group-badge') as HTMLElement;
    expect(badge.getAttribute('title')).toBe(SYSTEM_GROUP_NOTE);
    expect(badge.querySelector('.sr-only')?.textContent).toContain(SYSTEM_GROUP_NOTE);
  });

  it('keeps Edit and Delete on custom groups', () => {
    renderGroups([SYSTEM, CUSTOM]);
    const custom = card('Finance');
    expect(custom.querySelector('.edit-group-btn')).not.toBeNull();
    expect(custom.querySelector('.delete-group-btn')).not.toBeNull();
    expect(custom.querySelector('.system-group-badge')).toBeNull();
  });

  it('treats a missing or false system_managed as a custom group', () => {
    renderGroups([{ ...CUSTOM, system_managed: false }]);
    expect(document.querySelector('.edit-group-btn')).not.toBeNull();
    expect(document.querySelector('.delete-group-btn')).not.toBeNull();
  });
});

describe('deleteGroup (#919)', () => {
  it('refuses a system-managed group without asking or calling the API', async () => {
    await deleteGroup(SYSTEM.id);
    expect(mockConfirmDialog).not.toHaveBeenCalled();
    expect(api.deleteGroup).not.toHaveBeenCalled();
    expect(document.querySelector('.toast-error')?.textContent).toContain(SYSTEM_GROUP_NOTE);
  });

  it('shows the server reason when the API refuses a delete', async () => {
    (api.deleteGroup as jest.Mock).mockRejectedValue(
      new Error('system-managed group cannot be modified: "Finance" is seeded and maintained by migrations'),
    );
    await deleteGroup(CUSTOM.id);
    expect(document.querySelector('.toast-error')?.textContent).toContain(
      'Failed to delete group: system-managed group cannot be modified: "Finance" is seeded and maintained by migrations',
    );
  });

  it('falls back to the generic message when the error has no text', async () => {
    (api.deleteGroup as jest.Mock).mockRejectedValue(new Error(''));
    await deleteGroup(CUSTOM.id);
    expect(document.querySelector('.toast-error')?.textContent).toContain('Failed to delete group');
  });
});
