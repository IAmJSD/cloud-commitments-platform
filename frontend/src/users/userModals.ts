/**
 * User modal functionality
 *
 * PR #912: role removed from the user API contract. Group membership
 * is now required (>= 1 group) on create and update. The role selector
 * has been replaced by a required group multi-select that mirrors the
 * backend's cardinality >= 1 constraint.
 */

import * as api from '../api';
import { describePasswordValidationError } from '../auth';
import {
  currentEditingUser,
  setCurrentEditingUser,
  availableGroups
} from './state';
import { escapeHtml, showError, showSuccess, validateGroupCombination } from './utils';
import { loadUsers, refreshCurrentUserAccess } from './userActions';
import { openModal, closeModal } from '../modal';
import { clearFieldErrors, showFieldError } from '../lib/field-errors';

/**
 * Open create user modal
 */
export function openCreateUserModal(): void {
  setCurrentEditingUser(null);
  const modal = document.getElementById('user-modal');
  const title = document.getElementById('user-modal-title');
  const form = document.getElementById('user-form') as HTMLFormElement;

  if (!modal || !title || !form) return;

  title.textContent = 'Create User';
  form.reset();
  clearFieldErrors(form);
  (document.getElementById('user-id') as HTMLInputElement).value = '';

  // Show password field for new user. Password is optional: leaving it
  // blank invites the user via email to set their own password on first
  // login, so don't mark the field required.
  const passwordFields = document.getElementById('password-fields');
  if (passwordFields) {
    passwordFields.classList.remove('hidden');
    (document.getElementById('user-password') as HTMLInputElement).required = false;
  }

  // Populate groups dropdown (no pre-selection for new user)
  populateGroupsDropdown();

  openModal(modal);
}

/**
 * Open edit user modal
 */
export async function openEditUserModal(userId: string): Promise<void> {
  try {
    const user = await api.getUser(userId);
    setCurrentEditingUser(user);

    const modal = document.getElementById('user-modal');
    const title = document.getElementById('user-modal-title');
    const form = document.getElementById('user-form') as HTMLFormElement;

    if (!modal || !title || !form) return;

    title.textContent = 'Edit User';
    clearFieldErrors(form);
    (document.getElementById('user-id') as HTMLInputElement).value = user.id;
    (document.getElementById('user-email') as HTMLInputElement).value = user.email;
    // role field no longer exists in the API response (PR #912).

    // Hide password field for editing
    const passwordFields = document.getElementById('password-fields');
    if (passwordFields) {
      passwordFields.classList.add('hidden');
      (document.getElementById('user-password') as HTMLInputElement).required = false;
    }

    // Populate and pre-select groups
    populateGroupsDropdown(user.groups);

    openModal(modal);
  } catch (error) {
    console.error('Failed to load user:', error);
    showError('Failed to load user details');
  }
}

/**
 * Close user modal
 */
export function closeUserModal(): void {
  const modal = document.getElementById('user-modal');
  if (modal) {
    closeModal(modal);
  }
  setCurrentEditingUser(null);
}

/**
 * Save user (create or update).
 *
 * Groups are required (>= 1). The backend enforces this with a DB
 * CHECK constraint (migration 000057); the frontend mirrors it with
 * a pre-submit validation so the user gets a clear message rather
 * than a generic 400 from the server.
 */
export async function saveUser(e: Event): Promise<void> {
  e.preventDefault();
  const submitBtn = document.querySelector<HTMLButtonElement>('#user-form button[type="submit"]');
  if (submitBtn) submitBtn.disabled = true;
  try {
    await submitUserForm();
  } finally {
    if (submitBtn) submitBtn.disabled = false;
  }
}

interface FieldProblem {
  control: HTMLElement;
  message: string;
  /** Also toast it (the pre-#727 behavior these messages already had). */
  toast: boolean;
}

/**
 * Every reason the form cannot be saved, in field order (issue #727,
 * UX-33). #user-form is novalidate, so this is the only gate: the browser's
 * own bubbles vanish on the next click and never reached the group checks.
 */
function userFormProblems(email: HTMLInputElement, password: HTMLInputElement,
  groupsSelect: HTMLSelectElement, selectedGroups: string[]): FieldProblem[] {
  const problems: FieldProblem[] = [];
  if (email.value.trim() === '') {
    problems.push({ control: email, message: 'Enter an email address.', toast: false });
  } else if (email.validity?.typeMismatch) {
    problems.push({ control: email, message: 'Enter a valid email address, like name@example.com.', toast: false });
  }
  // Password is optional on create (blank emails an invite) and hidden on edit.
  if (!currentEditingUser && password.value) {
    const requirementError = describePasswordValidationError(password.value);
    if (requirementError) problems.push({ control: password, message: requirementError, toast: true });
  }
  // Enforce >= 1 group on the client side so the user gets a clear
  // validation message rather than a 400 from the backend. Then reject
  // contradictory combinations (view-only + write-capable).
  const groupProblem = selectedGroups.length === 0
    ? 'At least one group is required. Select one or more groups for this user.'
    : validateGroupCombination(selectedGroups, availableGroups);
  if (groupProblem) problems.push({ control: groupsSelect, message: groupProblem, toast: true });
  return problems;
}

async function submitUserForm(): Promise<void> {
  const emailInput = document.getElementById('user-email') as HTMLInputElement;
  const passwordInput = document.getElementById('user-password') as HTMLInputElement;
  const email = emailInput.value;
  const password = passwordInput.value;
  // role selector removed: PR #912 drops the role column.
  const groupsSelect = document.getElementById('user-groups') as HTMLSelectElement;
  const selectedGroups = Array.from(groupsSelect.selectedOptions).map(opt => opt.value);

  const problems = userFormProblems(emailInput, passwordInput, groupsSelect, selectedGroups);
  if (problems.length > 0) {
    for (const p of problems) showFieldError(p.control, p.message);
    const toast = problems.find(p => p.toast);
    if (toast) showError(toast.message);
    problems[0]!.control.focus();
    return;
  }

  try {
    if (currentEditingUser) {
      // Update existing user
      await api.updateUser(currentEditingUser.id, {
        email,
        groups: selectedGroups
      });
      await refreshCurrentUserAccess([currentEditingUser.id]);
      showSuccess('User updated successfully');
    } else {
      // Create new user. Password is optional: if blank, the backend
      // emails an invite that lets the user pick their own password on
      // first login. Strength checks ran in userFormProblems.
      const result = await api.createUser({
        email,
        password,
        groups: selectedGroups
      });
      // Three outcomes: password-up-front (no invite),
      // password-omitted + invite delivered, password-omitted + invite
      // send failed (user row exists but the recipient is unreachable
      // -- surface a warning so the admin knows to re-mail the link via
      // Forgot Password).
      if (password) {
        showSuccess('User created successfully');
      } else if (result.invite_email_sent === false) {
        showError(
          `User ${email} created but the invitation email could not be sent` +
            (result.invite_email_error ? `: ${result.invite_email_error}` : '') +
            '. Use the Forgot Password link from the sign-in page to re-mail the setup link.'
        );
      } else {
        showSuccess(
          `Invitation email sent to ${email} -- they will set their password on first login`
        );
      }
    }

    closeUserModal();
    await loadUsers();
  } catch (error) {
    console.error('Failed to save user:', error);
    const err = error as Error;
    showError(`Failed to save user: ${err.message}`);
  }
}

/**
 * Populate groups multi-select. Options are generated from the
 * availableGroups list loaded in loadUsers(); preSelectedGroups holds
 * the user's current group UUIDs (empty for new users).
 */
function populateGroupsDropdown(preSelectedGroups: string[] = []): void {
  const groupsSelect = document.getElementById('user-groups') as HTMLSelectElement;
  if (!groupsSelect) return;

  groupsSelect.innerHTML = availableGroups
    .map(group => `
      <option value="${escapeHtml(group.id)}" ${preSelectedGroups.includes(group.id) ? 'selected' : ''}>
        ${escapeHtml(group.name)}
      </option>
    `)
    .join('');
}
