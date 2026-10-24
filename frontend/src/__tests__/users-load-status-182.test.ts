/**
 * Issue #182: Admin > Users sections must never sit as empty boxes.
 * While listUsers/listGroups are in flight each section shows a skeleton;
 * when they fail each section shows an inline error with a Retry; a failed
 * refresh keeps the last good lists; genuinely empty data gets a real
 * empty state instead of "matching your filters".
 */
import './setup';

jest.mock('../api', () => ({
  listUsers: jest.fn(),
  listGroups: jest.fn(),
}));

jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn(() => Promise.resolve(true)),
}));

import * as api from '../api';
import * as userState from '../users/state';
import { loadUsers } from '../users/userActions';

const listUsers = api.listUsers as jest.Mock;
const listGroups = api.listGroups as jest.Mock;

const USERS = [{ id: 'u1', email: 'alice@example.com', groups: ['g1'], mfa_enabled: true }];
const GROUPS = [{ id: 'g1', name: 'Finance', permissions: [{ action: 'view', resource: 'plans' }], allowed_accounts: [] }];

function section(id: string): HTMLElement {
  return document.getElementById(id) as HTMLElement;
}

function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>(r => { resolve = r; });
  return { promise, resolve };
}

beforeEach(() => {
  document.body.innerHTML = `
    <div id="user-stats"></div>
    <div id="bulk-actions-bar" class="hidden"><span id="selected-count">0</span></div>
    <select id="bulk-group-select"><option value="">Add to group...</option></select>
    <div id="users-list"></div>
    <div id="groups-list"></div>
    <div id="permission-matrix"></div>
  `;
  userState.setAllUsers([]);
  userState.setFilteredUsers([]);
  userState.setAvailableGroups([]);
  listUsers.mockReset();
  listGroups.mockReset();
  jest.spyOn(console, 'error').mockImplementation(() => undefined);
});

afterEach(() => {
  jest.restoreAllMocks();
});

describe('loadUsers loading and failure states (#182)', () => {
  it('shows a skeleton in every empty section while the request is in flight', async () => {
    const users = deferred<{ users: unknown[] }>();
    listUsers.mockReturnValue(users.promise);
    listGroups.mockResolvedValue({ groups: GROUPS });

    const pending = loadUsers();
    for (const id of ['users-list', 'groups-list', 'permission-matrix']) {
      expect(section(id).querySelector('.skeleton, .skeleton-table')).not.toBeNull();
    }

    users.resolve({ users: USERS });
    await pending;
    expect(section('users-list').textContent).toContain('alice@example.com');
    expect(section('groups-list').textContent).toContain('Finance');
    expect(section('permission-matrix').querySelector('table')).not.toBeNull();
    for (const id of ['users-list', 'groups-list', 'permission-matrix']) {
      expect(section(id).querySelector('.skeleton, .skeleton-table')).toBeNull();
      expect(section(id).dataset['skeletonActive']).toBeUndefined();
    }
  });

  it('renders an inline error with Retry in each section when loading fails', async () => {
    listUsers.mockRejectedValue(new Error('database unavailable'));
    listGroups.mockResolvedValue({ groups: GROUPS });

    await loadUsers();

    expect(section('users-list').querySelector('.error')?.textContent)
      .toBe('Could not load users: database unavailable');
    expect(section('groups-list').querySelector('.error')?.textContent)
      .toBe('Could not load groups: database unavailable');
    expect(section('permission-matrix').querySelector('.error')?.textContent)
      .toBe('Could not load the permission overview: database unavailable');
    // The toast is the one live region, so the failure is announced once.
    expect(document.querySelectorAll('[role="alert"]')).toHaveLength(1);
    expect(document.querySelector('.toast-error[role="alert"]')).not.toBeNull();
    expect(document.querySelectorAll('button[data-action="retry-load-users"]')).toHaveLength(3);
    expect(document.querySelector('.skeleton, .skeleton-table')).toBeNull();
  });

  it('Retry reloads and replaces the errors with the lists', async () => {
    listUsers.mockRejectedValueOnce(new Error('HTTP 500')).mockResolvedValue({ users: USERS });
    listGroups.mockResolvedValue({ groups: GROUPS });
    await loadUsers();

    const retry = section('groups-list').querySelector('button[data-action="retry-load-users"]') as HTMLButtonElement;
    retry.click();
    // Focus stays in the section while the button is swapped out.
    expect(document.activeElement).toBe(section('groups-list'));
    await new Promise(r => setTimeout(r, 0));

    expect(listUsers).toHaveBeenCalledTimes(2);
    expect(document.querySelector('.error')).toBeNull();
    expect(section('users-list').textContent).toContain('alice@example.com');
    expect(section('groups-list').textContent).toContain('Finance');
    expect(section('permission-matrix').querySelector('table')).not.toBeNull();
  });

  it('keeps the last good lists when a later refresh fails', async () => {
    listUsers.mockResolvedValueOnce({ users: USERS }).mockRejectedValue(new Error('HTTP 503'));
    listGroups.mockResolvedValue({ groups: GROUPS });
    await loadUsers();
    await loadUsers();

    expect(section('users-list').textContent).toContain('alice@example.com');
    expect(section('groups-list').textContent).toContain('Finance');
    expect(section('permission-matrix').querySelector('table')).not.toBeNull();
    expect(document.querySelector('.error')).toBeNull();
    expect(document.querySelector('.toast-error')).not.toBeNull();
  });
});

describe('empty states (#182)', () => {
  it('says there are no users yet rather than blaming filters', async () => {
    listUsers.mockResolvedValue({ users: [] });
    listGroups.mockResolvedValue({ groups: [] });
    await loadUsers();

    expect(section('users-list').textContent).toContain('No users yet');
    expect(section('users-list').textContent).not.toContain('matching your filters');
    expect(section('groups-list').textContent).toContain('No groups yet');
    expect(section('permission-matrix').textContent).toContain('No groups defined yet');
  });
});
