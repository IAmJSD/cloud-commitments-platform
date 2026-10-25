/**
 * <option> lists for the group form's permission rows, moved out of
 * groupModals.ts (which is well past the 500-line rule).
 */

import { ALL_ACTIONS, ALL_RESOURCES } from '../permissions';
import type { Action, Resource } from '../permissions';
import { escapeHtml } from '../users/utils';
import { isBlankPermissionValue } from './groupFormValidation';

// Human-readable <option> labels for the action/resource vocabulary.
// Falls back to a generic title-case of the raw value ("cancel-own" ->
// "Cancel Own") for every entry that doesn't need a special-cased
// override; only acronyms and the symbolic "*" need one.
const ACTION_LABEL_OVERRIDES: Partial<Record<Action, string>> = {
  admin: 'Admin (full)',
};

const RESOURCE_LABEL_OVERRIDES: Partial<Record<Resource, string>> = {
  '*': 'All (*)',
  'api-keys': 'API Keys',
  'ri-exchange': 'RI Exchange',
};

function titleCase(value: string): string {
  return value
    .split('-')
    .map(word => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ');
}

function actionOptionLabel(action: string): string {
  return ACTION_LABEL_OVERRIDES[action as Action] ?? titleCase(action);
}

function resourceOptionLabel(resource: string): string {
  return RESOURCE_LABEL_OVERRIDES[resource as Resource] ?? titleCase(resource);
}

// Builds the <option> list for the perm-action / perm-resource <select>
// elements (issue #1629). This list used to be hardcoded to 7 of the 20
// actions and 9 of the 11 resources the backend can store. A stored
// permission carrying one of the missing values had no matching <option>,
// so the browser fell back to the first entry in the list -- silently
// DROPPING the permission on the action select (index 0 was the empty
// "Select Action" placeholder; collectPermissions() skips a falsy action)
// and silently WIDENING it to the `*` wildcard on the resource select
// (index 0 there was "All (*)"). Editing any group that held one of those
// permissions and clicking Save re-submitted a materially different
// permission list than the one shown, with no error.
//
// Building the lists from permissions.ts's ALL_ACTIONS / ALL_RESOURCES
// (which mirror the backend's full vocabulary, and are exhaustiveness
// -checked against it at compile time) closes that gap for every value
// currently known to the frontend. `currentValue` is still handled
// defensively beyond the known list: if a stored permission's value isn't
// one of them (a future backend verb this form hasn't been taught yet, or
// legacy/foreign data), an extra option is appended for that *exact*
// value, selected and visibly flagged, instead of silently falling back
// to a different one. The select then still represents -- and
// round-trips unchanged through collectPermissions() -- the real stored
// permission rather than corrupting it.
//
// A stored value that is blank after trimming (issue #213) is not an
// unknown value: it renders as the placeholder, explicitly `selected` (a
// disabled or unselected placeholder lets the browser fall through to the
// next option, which on the resource list is the `*` wildcard), and
// saveGroup refuses the row until the admin picks a value or removes it.
export function buildActionOptions(currentValue: string | undefined): string {
  if (isBlankPermissionValue(currentValue)) currentValue = undefined;
  const options = [`<option value=""${currentValue ? '' : ' selected'}>Select Action</option>`];
  for (const action of ALL_ACTIONS) {
    const selected = currentValue === action ? ' selected' : '';
    options.push(`<option value="${escapeHtml(action)}"${selected}>${escapeHtml(actionOptionLabel(action))}</option>`);
  }
  if (currentValue && !(ALL_ACTIONS as readonly string[]).includes(currentValue)) {
    options.push(`<option value="${escapeHtml(currentValue)}" selected>⚠ ${escapeHtml(currentValue)} (not recognized by this form)</option>`);
  }
  return options.join('');
}

export function buildResourceOptions(currentValue: string | undefined): string {
  const options: string[] = [];
  const storedBlank = isBlankPermissionValue(currentValue);
  if (storedBlank) {
    currentValue = undefined;
    options.push('<option value="" selected>Select Resource</option>');
  }
  for (const resource of ALL_RESOURCES) {
    // Only a brand-new row defaults to All (*); a stored blank must not.
    const isDefault = !currentValue && !storedBlank && resource === '*';
    const selected = currentValue === resource || isDefault ? ' selected' : '';
    options.push(`<option value="${escapeHtml(resource)}"${selected}>${escapeHtml(resourceOptionLabel(resource))}</option>`);
  }
  if (currentValue && !(ALL_RESOURCES as readonly string[]).includes(currentValue)) {
    options.push(`<option value="${escapeHtml(currentValue)}" selected>⚠ ${escapeHtml(currentValue)} (not recognized by this form)</option>`);
  }
  return options.join('');
}
