/**
 * Issue #20: after a group-membership edit that includes the signed-in user,
 * their groups and effective permissions are re-read so permission-gated UI
 * (the Opportunities Purchase button, the no-Purchaser banner) updates on the
 * next render instead of after a reload.
 */
import './setup';

jest.mock('../api', () => ({
  listUsers: jest.fn(),
  listGroups: jest.fn(),
  updateUser: jest.fn(),
  getCurrentUser: jest.fn(),
  getUserPermissions: jest.fn(),
}));

jest.mock('../groups/groupList', () => ({ renderGroups: jest.fn() }));

jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn(() => Promise.resolve(true)),
}));

import * as api from '../api';
import * as state from '../state';
import * as userState from '../users/state';
import { refreshCurrentUserAccess, bulkAddToGroup } from '../users/userActions';
import { canAccess, ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID } from '../permissions';

const SELF = { id: 'me', email: 'me@example.com', groups: [ADMINISTRATORS_GROUP_ID] };
const ADMIN_PERMS = [{ action: 'admin', resource: '*' }];
const PURCHASER_PERMS = [...ADMIN_PERMS, { action: 'execute', resource: 'purchases' }];

describe('refreshCurrentUserAccess (#20)', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    state.setCurrentUser({ ...SELF, effectivePermissions: ADMIN_PERMS } as never);
    (api.getCurrentUser as jest.Mock).mockResolvedValue({
      ...SELF,
      groups: [ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID],
    });
    (api.getUserPermissions as jest.Mock).mockResolvedValue({ permissions: PURCHASER_PERMS, is_admin: true });
  });

  test('re-reads groups and effective permissions when the current user changed', async () => {
    expect(canAccess('execute', 'purchases')).toBe(false);

    await refreshCurrentUserAccess(['someone-else', 'me']);

    expect(api.getCurrentUser).toHaveBeenCalledTimes(1);
    expect(api.getUserPermissions).toHaveBeenCalledTimes(1);
    expect(state.getCurrentUser()?.groups).toEqual([ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID]);
    expect(state.getCurrentUser()?.effectivePermissions).toEqual(PURCHASER_PERMS);
    expect(canAccess('execute', 'purchases')).toBe(true);
  });

  test('does nothing when the change did not include the current user', async () => {
    await refreshCurrentUserAccess(['someone-else']);

    expect(api.getCurrentUser).not.toHaveBeenCalled();
    expect(api.getUserPermissions).not.toHaveBeenCalled();
    expect(state.getCurrentUser()?.effectivePermissions).toEqual(ADMIN_PERMS);
  });

  test('keeps the previous state and warns when a fetch fails', async () => {
    (api.getUserPermissions as jest.Mock).mockRejectedValue(new Error('HTTP 503'));
    const warn = jest.spyOn(console, 'warn').mockImplementation(() => {});

    await refreshCurrentUserAccess(['me']);

    expect(state.getCurrentUser()?.groups).toEqual([ADMINISTRATORS_GROUP_ID]);
    expect(state.getCurrentUser()?.effectivePermissions).toEqual(ADMIN_PERMS);
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
  });

  test('a bulk add that includes the current user refreshes their access', async () => {
    document.body.innerHTML = '<div id="users-list"></div><div id="user-stats"></div>';
    const groups = [
      { id: ADMINISTRATORS_GROUP_ID, name: 'Administrators', permissions: ADMIN_PERMS, allowed_accounts: [] },
      { id: PURCHASER_GROUP_ID, name: 'Purchaser', permissions: [], allowed_accounts: [] },
    ];
    (api.updateUser as jest.Mock).mockResolvedValue({});
    (api.listUsers as jest.Mock).mockResolvedValue({ users: [SELF] });
    (api.listGroups as jest.Mock).mockResolvedValue({ groups });
    userState.setAllUsers([SELF] as never);
    userState.setAvailableGroups(groups as never);
    userState.clearSelectedUserIds();
    userState.addSelectedUserId('me');

    await bulkAddToGroup(PURCHASER_GROUP_ID);

    expect(api.updateUser).toHaveBeenCalledWith('me', { groups: [ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID] });
    expect(canAccess('execute', 'purchases')).toBe(true);
  });
});
