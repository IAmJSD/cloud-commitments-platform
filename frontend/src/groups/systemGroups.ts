/**
 * System-managed groups (issue #919).
 */

import type { APIGroup } from '../api';

// Issue #919: the API refuses edits and deletes of system-managed groups
// (seeded by migrations: Administrators, Purchaser, Standard Users, ...)
// with ErrSystemManagedGroup / 403, so offering Edit and Delete there only
// leads to an error. Duplicate stays: it creates a separate, editable copy.
export const SYSTEM_GROUP_NOTE =
  'Built-in group maintained by CUDly. It cannot be edited or deleted; change its members from the Users list.';

export function isSystemManagedGroup(group: Pick<APIGroup, 'system_managed'> | undefined): boolean {
  return group?.system_managed === true;
}
