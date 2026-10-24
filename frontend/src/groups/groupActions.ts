/**
 * Group action functionality
 */

import * as api from '../api';
import { availableGroups } from '../users/state';
import { showError, showSuccess } from '../users/utils';
import { loadUsers } from '../users/userActions';
import { confirmDialog } from '../confirmDialog';
import { friendlyError } from '../api/errors';
import { isSystemManagedGroup, SYSTEM_GROUP_NOTE } from './systemGroups';

/**
 * Delete group
 */
export async function deleteGroup(groupId: string): Promise<void> {
  const group = availableGroups.find(g => g.id === groupId);
  if (!group) return;
  // The card hides Delete for these (#919); refuse here too rather than
  // ask for confirmation of a delete the API will reject.
  if (isSystemManagedGroup(group)) {
    showError(SYSTEM_GROUP_NOTE);
    return;
  }

  const ok = await confirmDialog({
    title: `Delete group "${group.name}"?`,
    body: 'Members of this group will lose any permissions granted by it. Users themselves are not deleted. This action cannot be undone.',
    confirmLabel: 'Delete group',
    destructive: true,
  });
  if (!ok) return;

  try {
    await api.deleteGroup(groupId);
    await loadUsers();
    showSuccess('Group deleted successfully');
  } catch (error) {
    console.error('Failed to delete group:', error);
    // Surface the server's reason (e.g. the system-managed refusal), as the
    // Edit path already does (#919 review comment). friendlyError keeps a
    // server message and turns transport failures into plain text (#748).
    showError(`Failed to delete group: ${friendlyError(error)}`);
  }
}

// Re-export openEditGroupModal from groupModals
export { openEditGroupModal } from './groupModals';
