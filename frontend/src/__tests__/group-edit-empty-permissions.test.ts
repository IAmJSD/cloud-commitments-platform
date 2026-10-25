/**
 * Regression tests for issue #237: removing every permission row from the
 * group edit form and saving reported "Group updated successfully" while
 * silently leaving the group's stored permissions untouched.
 *
 * collectPermissions() (groupModals.ts) skips any row whose action or
 * resource is still the empty "Select Action"/"Select Resource" placeholder,
 * so clicking Remove on every real row -- or leaving only a placeholder row
 * -- collects as permissions: []. The update API's documented contract
 * (applyUpdateGroupRequest, internal/auth/service_api.go) treats an empty
 * permissions list as "not sent" and leaves the group's permissions
 * unchanged -- there is no way for this endpoint to express "clear all
 * permissions". So the form would show success and reload a group that
 * still carried its old privileges (possibly admin:*).
 *
 * The fix refuses to submit when collectPermissions() returns [], with an
 * explicit error telling the operator to delete the group instead.
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
import { showError, showSuccess } from '../users/utils';

function setUpModalDom(): void {
  document.body.innerHTML = `
    <div id="group-modal" class="hidden">
      <span id="group-modal-title"></span>
      <form id="group-form">
        <input id="group-id" />
        <input id="group-name" />
        <textarea id="group-description"></textarea>
        <div id="permissions-list"></div>
      </form>
    </div>
  `;
}

async function submitForm(): Promise<void> {
  const event = { preventDefault: jest.fn() } as unknown as Event;
  await groupModals.saveGroup(event);
}

const ADMIN_GROUP: api.APIGroup = {
  id: 'admin-group-id',
  name: 'Admins',
  description: 'Old description',
  permissions: [{ action: 'admin', resource: '*' }],
  created_at: '2024-01-01T00:00:00Z',
};

describe('regression: saving a group with every permission row removed (#237)', () => {
  beforeEach(() => {
    setUpModalDom();
    groupState.setCurrentEditingGroup(null);
    jest.clearAllMocks();
  });

  test('removing every permission row and saving does not call updateGroup and shows an error, not success', async () => {
    (api.getGroup as jest.Mock).mockResolvedValue(ADMIN_GROUP);
    await groupModals.openEditGroupModal(ADMIN_GROUP.id);

    // Remove the one loaded permission row via its own Remove button --
    // the exact operator action the issue describes.
    const removeBtn = document.querySelector<HTMLButtonElement>('.remove-permission-btn');
    expect(removeBtn).not.toBeNull();
    removeBtn!.click();
    expect(document.querySelectorAll('.permission-item')).toHaveLength(0);

    await submitForm();

    // The group's real admin:* permissions must never be silently kept by
    // submitting an empty list the backend interprets as "unchanged" --
    // submission must not happen at all.
    expect(api.updateGroup).not.toHaveBeenCalled();
    expect(showSuccess).not.toHaveBeenCalled();
    expect(showError).toHaveBeenCalledWith(expect.stringContaining('at least one permission'));
  });

  test('a group whose only remaining row is left on the placeholder also refuses to save', async () => {
    (api.getGroup as jest.Mock).mockResolvedValue(ADMIN_GROUP);
    await groupModals.openEditGroupModal(ADMIN_GROUP.id);

    // Reset the loaded row back to the "Select Action" / "Select Resource"
    // placeholder instead of removing it outright -- the issue's second
    // reproduction shape.
    const actionSelect = document.querySelector<HTMLSelectElement>('.perm-action')!;
    const resourceSelect = document.querySelector<HTMLSelectElement>('.perm-resource')!;
    actionSelect.value = '';
    resourceSelect.value = '';

    await submitForm();

    expect(api.updateGroup).not.toHaveBeenCalled();
    expect(showSuccess).not.toHaveBeenCalled();
    // Issue #213: the placeholder row is refused in place (note under the
    // row, controls aria-invalid) instead of being dropped and then hitting
    // the empty-list refusal.
    expect(actionSelect.getAttribute('aria-invalid')).toBe('true');
    expect(resourceSelect.getAttribute('aria-invalid')).toBe('true');
    expect(document.querySelector('.permission-item .field-error')?.textContent)
      .toContain('no action and no resource');
  });

  test('creating a new group with no permission rows also refuses to save', async () => {
    groupModals.openCreateGroupModal();
    (document.getElementById('group-name') as HTMLInputElement).value = 'New Group';

    await submitForm();

    expect(api.createGroup).not.toHaveBeenCalled();
    expect(showSuccess).not.toHaveBeenCalled();
    expect(showError).toHaveBeenCalledWith(expect.stringContaining('at least one permission'));
  });

  test('a group that still has at least one real permission row saves normally', async () => {
    (api.getGroup as jest.Mock).mockResolvedValue(ADMIN_GROUP);
    await groupModals.openEditGroupModal(ADMIN_GROUP.id);

    await submitForm();

    expect(api.updateGroup).toHaveBeenCalledWith('admin-group-id', {
      name: 'Admins',
      description: 'Old description',
      permissions: [{ action: 'admin', resource: '*' }],
    });
    expect(showError).not.toHaveBeenCalled();
  });
});
