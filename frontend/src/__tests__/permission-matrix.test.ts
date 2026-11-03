/**
 * Permission overview matrix (Admin > Users). Rows used to be a hardcoded
 * view/create/update/delete/execute/approve/admin list matched exactly,
 * so Administrators (admin:*) showed "—" everywhere but admin, and groups
 * holding approve-any / retry-any / cancel-own looked like they held nothing.
 */
import type { APIGroup } from '../api';
import { ALL_ACTIONS } from '../permissions';
import {
  permissionMatrixActions,
  permissionMatrixCell,
  renderPermissionMatrix,
} from '../users/permissionMatrix';

const group = (name: string, perms: Array<[string, string]>): APIGroup => ({
  id: name, name, description: '',
  permissions: perms.map(([action, resource]) => ({ action, resource })) as APIGroup['permissions'],
});

const ADMINS = group('Administrators', [['admin', '*']]);
const STANDARD = group('Standard Users', [['view', 'plans'], ['cancel-own', 'purchases']]);
const PURCHASER = group('Purchaser', [['execute', 'purchases'], ['approve-any', 'purchases'], ['retry-any', 'purchases']]);

function cellFor(container: HTMLElement, action: string, column: number): HTMLTableCellElement {
  const row = Array.from(container.querySelectorAll('tbody tr')).find(r => r.querySelector('td')?.textContent === action);
  if (!row) throw new Error(`no row for ${action}`);
  return row.querySelectorAll('td')[column + 1] as HTMLTableCellElement;
}

describe('permission matrix', () => {
  afterEach(() => document.body.replaceChildren());

  test('rows cover the whole action vocabulary, variants under their base verb, admin last', () => {
    const rows = permissionMatrixActions([ADMINS]);
    expect([...rows].sort()).toEqual([...ALL_ACTIONS].sort());
    expect(rows.indexOf('approve-own')).toBe(rows.indexOf('approve') + 1);
    expect(rows.indexOf('approve-any')).toBe(rows.indexOf('approve') + 2);
    expect(rows.indexOf('update-any')).toBe(rows.indexOf('update') + 1);
    expect(rows[rows.length - 1]).toBe('admin');
  });

  test('an action a group stores that the vocabulary lacks still gets a row', () => {
    expect(permissionMatrixActions([group('Odd', [['export', 'history']])])).toContain('export');
  });

  test('verb variants map to their own rows instead of reading as not granted', () => {
    const c = document.createElement('div');
    renderPermissionMatrix([STANDARD, PURCHASER], c);
    expect(cellFor(c, 'cancel-own', 0).className).toBe('has-perm');
    expect(cellFor(c, 'cancel-own', 0).title).toBe('purchases');
    expect(cellFor(c, 'approve-any', 1).className).toBe('has-perm');
    expect(cellFor(c, 'retry-any', 1).className).toBe('has-perm');
    expect(cellFor(c, 'approve', 1).className).toBe('no-perm');
  });

  test('admin:* grants every action via admin, except the carved-out verbs', () => {
    expect(permissionMatrixCell(ADMINS, 'delete')).toEqual({ kind: 'admin', except: [] });
    expect(permissionMatrixCell(ADMINS, 'admin')).toEqual({ kind: 'direct', resources: ['*'] });
    expect(permissionMatrixCell(ADMINS, 'execute')).toEqual({ kind: 'admin', except: ['execute:purchases', 'execute:ri-exchange'] });
    expect(permissionMatrixCell(ADMINS, 'approve-any')).toEqual({ kind: 'admin', except: ['approve-any:purchases'] });

    const c = document.createElement('div');
    renderPermissionMatrix([ADMINS], c);
    const del = cellFor(c, 'delete', 0);
    expect(del.className).toBe('has-perm via-admin');
    expect(del.title).toBe('Via admin:*');
    expect(del.getAttribute('aria-label')).toBe('Granted via admin');
    const exec = cellFor(c, 'execute', 0);
    expect(exec.classList.contains('partial')).toBe(true);
    expect(exec.title).toContain('except execute:purchases, execute:ri-exchange');
    expect(c.querySelectorAll('td.no-perm')).toHaveLength(0);
    expect(c.querySelector('.permission-matrix-note')?.textContent).toContain('admin:*');
  });

  test('admin on a single resource is not the wildcard', () => {
    expect(permissionMatrixCell(group('User admins', [['admin', 'users']]), 'delete')).toEqual({ kind: 'none' });
  });

  test('an explicit grant beats the admin wildcard', () => {
    const both = group('Admin purchasers', [['admin', '*'], ['execute', 'purchases']]);
    expect(permissionMatrixCell(both, 'execute')).toEqual({ kind: 'direct', resources: ['purchases'] });
  });
});
