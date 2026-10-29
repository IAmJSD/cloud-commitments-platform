/**
 * Read-only permission matrix rendering
 *
 * Displays a table with actions as rows and groups as columns,
 * showing which actions each group has for any resource.
 *
 * Rows are the full action vocabulary from permissions.ts (ALL_ACTIONS),
 * not a hand-picked subset, so verb variants such as approve-any,
 * cancel-own or retry-any get their own row instead of reading as "not
 * granted". Variants sit under their base verb. A group holding admin:*
 * shows every action as granted "via admin", except the verbs carved out
 * of the admin wildcard (ADMIN_CARVED_OUTS), which it only holds when it
 * grants them explicitly.
 */

import type { APIGroup } from '../api';
import { ADMIN_CARVED_OUTS, ALL_ACTIONS } from '../permissions';
import { escapeHtml } from './utils';

export type PermissionMatrixCell =
  | { kind: 'direct'; resources: string[] }
  | { kind: 'admin'; except: string[] }
  | { kind: 'none' };

const VARIANT_RANK: Record<string, number> = { '': 0, own: 1, any: 2 };

function splitVerb(action: string): { base: string; variant: string } {
  const m = /^(.*)-(own|any)$/.exec(action);
  return m ? { base: m[1]!, variant: m[2]! } : { base: action, variant: '' };
}

function holdsAdminWildcard(group: APIGroup): boolean {
  return (group.permissions ?? []).some(p => p.action === 'admin' && p.resource === '*');
}

/**
 * Matrix rows: every known action plus any unknown action a group stores
 * (so nothing granted is hidden), grouped by base verb in vocabulary order
 * (view, create, update, update-any, delete, execute, execute-own, ...),
 * with admin last.
 */
export function permissionMatrixActions(groups: readonly APIGroup[]): string[] {
  const known = new Set<string>(ALL_ACTIONS);
  const actions: string[] = [...ALL_ACTIONS];
  for (const g of groups) {
    for (const p of g.permissions ?? []) {
      if (!known.has(p.action)) {
        known.add(p.action);
        actions.push(p.action);
      }
    }
  }
  const baseOrder: string[] = [];
  for (const a of actions) {
    const { base } = splitVerb(a);
    if (!baseOrder.includes(base)) baseOrder.push(base);
  }
  const rank = (a: string): [number, number] => {
    const { base, variant } = splitVerb(a);
    return [base === 'admin' ? Number.MAX_SAFE_INTEGER : baseOrder.indexOf(base), VARIANT_RANK[variant] ?? 3];
  };
  return actions.sort((a, b) => {
    const [ba, va] = rank(a);
    const [bb, vb] = rank(b);
    return ba - bb || va - vb;
  });
}

/** What one group grants for one action, directly or through admin:*. */
export function permissionMatrixCell(group: APIGroup, action: string): PermissionMatrixCell {
  const resources = (group.permissions ?? []).filter(p => p.action === action).map(p => p.resource);
  if (resources.length > 0) return { kind: 'direct', resources };
  if (action !== 'admin' && holdsAdminWildcard(group)) {
    const except = [...ADMIN_CARVED_OUTS].filter(key => key.startsWith(`${action}:`));
    return { kind: 'admin', except };
  }
  return { kind: 'none' };
}

function renderCell(cell: PermissionMatrixCell): string {
  // WCAG 1.4.1: pair the glyph with an sr-only label so screen readers
  // announce the state instead of reading nothing / a bare symbol.
  if (cell.kind === 'direct') {
    return `<td class="has-perm" title="${escapeHtml(cell.resources.join(', '))}" aria-label="Granted">✓<span class="sr-only"> Granted</span></td>`;
  }
  if (cell.kind === 'admin') {
    if (cell.except.length === 0) {
      return '<td class="has-perm via-admin" title="Via admin:*" aria-label="Granted via admin">✓<span class="sr-only"> Granted via admin</span></td>';
    }
    const except = escapeHtml(cell.except.join(', '));
    return `<td class="has-perm via-admin partial" title="Via admin:*, except ${except} (needs an explicit grant)" aria-label="Granted via admin, except ${except}">✓<span class="perm-partial-mark" aria-hidden="true">*</span><span class="sr-only"> Granted via admin, except ${except}</span></td>`;
  }
  return '<td class="no-perm" aria-label="Not granted">—<span class="sr-only"> Not granted</span></td>';
}

/**
 * Render a permission matrix table into the given container.
 * Rows = actions, Columns = group names, Cells = checkmark or dash.
 *
 * All dynamic values are escaped via escapeHtml before insertion.
 */
function renderEmptyState(container: HTMLElement, message: string): void {
  container.textContent = '';
  container.removeAttribute('tabindex');
  container.removeAttribute('role');
  container.removeAttribute('aria-label');
  const p = document.createElement('p');
  p.className = 'empty-state';
  p.textContent = message;
  container.appendChild(p);
}

export function renderPermissionMatrix(groups: APIGroup[], container: HTMLElement): void {
  if (groups.length === 0) {
    renderEmptyState(container, 'No groups defined yet. Create one to assign permissions.');
    return;
  }

  // Groups exist but none carry permissions — matrix would be all dashes.
  // Show an explicit empty-state instead so the section doesn't read as
  // broken or unfinished.
  const anyPermission = groups.some(g => g.permissions && g.permissions.length > 0);
  if (!anyPermission) {
    renderEmptyState(container, 'No custom permissions configured yet. Edit a group to assign actions to resources.');
    return;
  }

  const headerCells = groups
    .map(g => `<th>${escapeHtml(g.name)}</th>`)
    .join('');

  const rows = permissionMatrixActions(groups).map(action => {
    const cells = groups.map(group => renderCell(permissionMatrixCell(group, action))).join('');
    return `<tr><td>${escapeHtml(action)}</td>${cells}</tr>`;
  }).join('');

  const viaAdmin = groups.some(holdsAdminWildcard);
  const note = viaAdmin
    ? `<p class="permission-matrix-note">Grey ticks are granted through <code>admin:*</code>. It covers every action on every resource except ${escapeHtml([...ADMIN_CARVED_OUTS].join(', '))}, which need an explicit grant (ticks marked *). Hover a tick for the resources.</p>`
    : '<p class="permission-matrix-note">Hover a tick for the resources it covers.</p>';

  // All dynamic content (group names, action names, resources) is escaped
  // via escapeHtml before insertion — safe against XSS.
  container.innerHTML = `
    <table class="permission-matrix">
      <thead>
        <tr><th>Action</th>${headerCells}</tr>
      </thead>
      <tbody>
        ${rows}
      </tbody>
    </table>
    ${note}
  `;
  // The matrix scrolls sideways on narrow screens and has no focusable
  // content of its own, so the scroll region must be keyboard-reachable.
  container.setAttribute('tabindex', '0');
  container.setAttribute('role', 'region');
  container.setAttribute('aria-label', 'Permission matrix');
}
