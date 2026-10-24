/**
 * User action functionality (CRUD operations, bulk operations)
 */

import * as api from '../api';
import { getCurrentUser, setCurrentUser } from '../state';
import { isAdmin, isPurchaser, PURCHASER_GROUP_ID } from '../permissions';
import {
  allUsers,
  filteredUsers,
  selectedUserIds,
  setAllUsers,
  setAvailableGroups,
  removeSelectedUserId
} from './state';
import { showError, showSuccess, validateGroupCombination } from './utils';
import { confirmDialog } from '../confirmDialog';
import { applyFilters } from './filters';
import { renderUsers, renderUserStats, populateBulkGroupSelect } from './userList';
import { renderGroups } from '../groups/groupList';
import { renderPermissionMatrix } from './permissionMatrix';
import { clearUserSectionsStatus, renderUserSectionsError, showUserSectionsLoading } from './loadStatus';

/**
 * Defence-in-depth normaliser for the API contract. The backend's TS-typed
 * APIUser declares `groups: string[]` as required, but historical bugs
 * (and unforeseen future ones) can omit the field or send `null`. Renderers
 * iterate `user.groups`, so a missing field crashes the page with a
 * generic "Failed to load users and groups" toast that obscures the real
 * cause. See issue #350 — the backend fix lives there too; this guard is
 * the second line of defence so a future contract regression degrades
 * gracefully instead of throwing a TypeError in an Array.map.
 */
function normalizeUser(user: api.APIUser): api.APIUser {
  return {
    ...user,
    groups: Array.isArray(user.groups) ? user.groups : [],
  };
}

function normalizeGroup(group: api.APIGroup): api.APIGroup {
  return {
    ...group,
    permissions: Array.isArray(group.permissions) ? group.permissions : [],
    allowed_accounts: Array.isArray(group.allowed_accounts) ? group.allowed_accounts : [],
  };
}

/**
 * Ensure the currently-logged-in user is visible in the list.
 * If the API didn't include them (e.g. self-management not wired up yet),
 * prepend a synthetic entry flagged with id='current' so the userList "You"
 * badge still renders.
 */
function withCurrentUser(users: api.APIUser[]): api.APIUser[] {
  const current = getCurrentUser();
  if (!current) return users;
  if (users.some(u => u.email === current.email)) return users;
  const synthetic: api.APIUser = {
    id: 'current',
    email: current.email,
    groups: Array.isArray(current.groups) ? current.groups : [],
    mfa_enabled: current.mfa_enabled === true,
  };
  return [synthetic, ...users];
}

/**
 * Re-read the signed-in user's groups and effective permissions after a
 * group-membership change that includes them, so permission-gated UI (e.g.
 * the Opportunities Purchase button and no-Purchaser banner) reflects the
 * change on its next render instead of after a reload (#20). Best-effort:
 * a failed fetch keeps the old state, which is no worse than before.
 */
export async function refreshCurrentUserAccess(changedUserIds: readonly string[]): Promise<void> {
  const current = getCurrentUser();
  if (!current || !changedUserIds.includes(current.id)) return;
  try {
    const [me, perms] = await Promise.all([api.getCurrentUser(), api.getUserPermissions()]);
    setCurrentUser({ ...me, effectivePermissions: perms.permissions });
  } catch (err) {
    console.warn('Failed to refresh current user permissions:', err);
  }
}

/**
 * Load and display users and groups
 */
export async function loadUsers(): Promise<void> {
  // Issue #182: skeletons while the lists load, so an empty box is never
  // mistaken for "no users" (no-op where a list is already on screen).
  showUserSectionsLoading();
  try {
    const [usersResponse, groupsResponse] = await Promise.all([
      api.listUsers(),
      api.listGroups()
    ]);

    // Normalise at the boundary so downstream renderers can trust
    // user.groups / group.permissions / group.allowed_accounts to be
    // real arrays (see normalizeUser/normalizeGroup above for the
    // rationale — issue #350 backend + frontend defence-in-depth).
    const users = (usersResponse.users ?? []).map(normalizeUser);
    const groups = (groupsResponse.groups ?? []).map(normalizeGroup);

    setAvailableGroups(groups);
    setAllUsers(withCurrentUser(users));
    clearUserSectionsStatus();

    // Apply current filters
    applyFilters();

    renderUsers(filteredUsers);
    renderGroups(groups);
    renderUserStats();
    populateBulkGroupSelect();

    const matrixContainer = document.getElementById('permission-matrix');
    if (matrixContainer) {
      renderPermissionMatrix(groups, matrixContainer);
    }

    // Issue #923: first-run prompt for admins not in the Purchaser group.
    // Show once per browser (stored in localStorage). The Purchaser group
    // must exist (migration 000058) before the prompt is relevant, so we
    // check that availableGroups contains it before surfacing the dialog.
    const PROMPT_KEY = 'cudly:purchaser-prompt-dismissed';
    const purchaserGroupExists = groups.some(g => g.id === PURCHASER_GROUP_ID);
    if (
      purchaserGroupExists &&
      isAdmin() &&
      !isPurchaser() &&
      !localStorage.getItem(PROMPT_KEY)
    ) {
      localStorage.setItem(PROMPT_KEY, '1');
      // Use the existing confirmDialog as a non-destructive notification.
      void confirmDialog({
        title: 'Purchaser group: separation of duties',
        body:
          'Recommended: add yourself to the Purchaser group only if no separate ' +
          'finance team will execute purchases. Otherwise leave it to dedicated ' +
          'Purchaser user(s). You can manage membership in the Groups panel below.',
        confirmLabel: 'Got it',
        destructive: false,
      });
    }
  } catch (error) {
    console.error('Failed to load users/groups:', error);
    showError('Failed to load users and groups');
    // Issue #182: the toast fades; leave an inline error with a Retry in
    // each section that has nothing else to show.
    const detail = error instanceof Error ? error.message : '';
    renderUserSectionsError(detail, () => { void loadUsers(); });
  }
}

/**
 * Delete user
 */
export async function deleteUser(userId: string): Promise<void> {
  const user = allUsers.find(u => u.id === userId);
  if (!user) return;

  const ok = await confirmDialog({
    title: `Delete user "${user.email}"?`,
    body: 'This removes the user from the system along with all of their group memberships. This action cannot be undone.',
    confirmLabel: 'Delete user',
    destructive: true,
  });
  if (!ok) return;

  try {
    await api.deleteUser(userId);
    await loadUsers();
    showSuccess('User deleted successfully');
  } catch (error) {
    console.error('Failed to delete user:', error);
    showError('Failed to delete user');
  }
}

/**
 * Bulk delete users
 */
export async function bulkDeleteUsers(): Promise<void> {
  if (selectedUserIds.size === 0) return;

  const count = selectedUserIds.size;
  const ok = await confirmDialog({
    title: `Delete ${count} user${count === 1 ? '' : 's'}?`,
    body: 'This removes the selected users from the system along with all of their group memberships. This action cannot be undone.',
    confirmLabel: `Delete ${count} user${count === 1 ? '' : 's'}`,
    destructive: true,
  });
  if (!ok) return;

  const ids = Array.from(selectedUserIds);
  const labels = new Map(allUsers.map(u => [u.id, u.email]));
  const results = await Promise.allSettled(ids.map(userId => api.deleteUser(userId)));

  const failed: string[] = [];
  results.forEach((result, i) => {
    if (result.status === 'fulfilled') {
      removeSelectedUserId(ids[i]!);
    } else {
      console.error('Failed to delete user:', ids[i], result.reason);
      failed.push(labels.get(ids[i]!) ?? ids[i]!);
    }
  });

  await loadUsers();

  if (failed.length > 0) {
    showError(`Failed to delete ${failed.length} of ${ids.length} user(s): ${failed.join(', ')}`);
  } else {
    showSuccess(`Successfully deleted ${count} user(s)`);
  }
}

// bulkChangeRole removed in PR #912 frontend: role no longer exists.
// Use the group multi-select in the Edit User modal to manage access.

/**
 * Bulk add to group
 */
export async function bulkAddToGroup(groupId: string): Promise<void> {
  if (selectedUserIds.size === 0) return;

  const { availableGroups } = await import('./state');
  const count = selectedUserIds.size;
  const group = availableGroups.find(g => g.id === groupId);
  if (!group) return;

  const addOk = await confirmDialog({
    title: `Add ${count} user${count === 1 ? '' : 's'} to "${group.name}"?`,
    body: `The ${count === 1 ? 'selected user' : 'selected users'} will gain the permissions of the "${group.name}" group.`,
    confirmLabel: 'Add to group',
  });
  if (!addOk) return;

  // Partition the selected users by whether adding this group would produce a
  // contradictory combination (view-only group + write-capable group, issue
  // #1405). The bulk path must enforce the same rule as the inline expand
  // panel and the create/edit modal -- otherwise selecting a Viewer-group
  // user and bulk-adding a write group silently creates the exact
  // combination #1405 is meant to prevent. Contradictory users are skipped
  // (never silently written) and named in an error toast so the admin knows
  // which ones were left unchanged.
  const applicable: { userId: string; groups: string[] }[] = [];
  const skipped: string[] = [];
  for (const userId of selectedUserIds) {
    const user = allUsers.find(u => u.id === userId);
    if (!user) continue;

    const updatedGroups = [...new Set([...user.groups, groupId])];
    if (validateGroupCombination(updatedGroups, availableGroups)) {
      skipped.push(user.email);
    } else {
      applicable.push({ userId, groups: updatedGroups });
    }
  }

  if (skipped.length > 0) {
    showError(
      `Skipped ${skipped.length} user(s) whose group set would be contradictory: ` +
      `${skipped.join(', ')}. A view-only group cannot be combined with ` +
      `write-capable groups. Remove the conflicting membership first.`
    );
  }

  // Nothing safe to apply -- leave the selection intact so the admin can
  // adjust the offending memberships and retry.
  if (applicable.length === 0) return;

  try {
    await Promise.all(
      applicable.map(({ userId, groups }) => api.updateUser(userId, { groups })),
    );

    // Deselect only the successfully-applied users so any skipped
    // (contradictory) users stay selected for the admin to fix.
    applicable.forEach(({ userId }) => removeSelectedUserId(userId));
    await refreshCurrentUserAccess(applicable.map(({ userId }) => userId));
    await loadUsers();
    showSuccess(`Successfully added ${applicable.length} user(s) to group`);
  } catch (error) {
    console.error('Failed to add users to group:', error);
    showError('Failed to add some users to group');
  }
}

// Re-export for use in userModals
export { openEditUserModal } from './userModals';
