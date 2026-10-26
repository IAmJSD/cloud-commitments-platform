/**
 * Service override export/import panel in Admin -> Accounts (issue #9).
 *
 * Export downloads every per-account service override as JSON or CSV. Import
 * reads a JSON or CSV export, previews it with a dry run, then applies it.
 * Rows are matched to accounts on the server (by account ID, else by provider
 * and account number), so a file from another CUDly install imports too.
 */

import * as api from '../api';
import type {
  OverrideImportMode,
  OverrideImportReport,
  OverrideImportRequest,
  OverrideTransferRow,
} from '../api/accounts';
import type { ApiError } from '../api/types';
import { friendlyError } from '../api/errors';
import { badgeClass, type BadgeTone } from '../lib/status-badge';
import { showToast } from '../toast';
import { confirmDialog } from '../confirmDialog';

const ACTION_TONES: Record<string, BadgeTone> = {
  insert: 'success',
  update: 'warning',
  unchanged: 'muted',
  rejected: 'danger',
};

const ACTION_LABELS: Record<string, string> = {
  insert: 'New',
  update: 'Changed',
  unchanged: 'Unchanged',
  rejected: 'Rejected',
};

/** The selected file's rows, ready to send. Reset when the file changes. */
let pendingImport: Pick<OverrideImportRequest, 'overrides' | 'csv'> | null = null;

function byId<T extends HTMLElement>(id: string): T | null {
  return document.getElementById(id) as T | null;
}

function downloadText(filename: string, content: string, contentType: string): void {
  const blob = new Blob([content], { type: contentType });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(a.href);
}

function exportFilename(ext: string): string {
  return `cudly-service-overrides-${new Date().toISOString().slice(0, 10)}.${ext}`;
}

export async function exportOverrides(format: 'json' | 'csv'): Promise<void> {
  try {
    if (format === 'csv') {
      downloadText(exportFilename('csv'), await api.exportAccountServiceOverridesCSV(), 'text/csv');
    } else {
      const doc = await api.exportAccountServiceOverrides();
      downloadText(exportFilename('json'), JSON.stringify(doc, null, 2), 'application/json');
    }
  } catch (err) {
    showToast({ message: `Failed to export overrides: ${friendlyError(err)}`, kind: 'error' });
  }
}

/**
 * Turns a file's text into an import payload: CSV text is sent as is (the
 * server parses it), JSON may be an export document or a bare row array.
 */
export function parseImportFile(name: string, text: string): Pick<OverrideImportRequest, 'overrides' | 'csv'> {
  if (name.toLowerCase().endsWith('.csv')) {
    return { csv: text };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new Error('The file is not valid JSON or CSV.');
  }
  const rows = Array.isArray(parsed) ? parsed : (parsed as { overrides?: unknown })?.overrides;
  if (!Array.isArray(rows)) {
    throw new Error('The JSON file has no "overrides" list.');
  }
  return { overrides: rows as OverrideTransferRow[] };
}

function setStatus(text: string): void {
  const el = byId<HTMLElement>('override-import-status');
  if (el) el.textContent = text;
}

function setImportButtonsEnabled(enabled: boolean): void {
  for (const id of ['override-import-preview-btn', 'override-import-apply-btn']) {
    const btn = byId<HTMLButtonElement>(id);
    if (btn) btn.disabled = !enabled;
  }
}

async function onFileChosen(input: HTMLInputElement): Promise<void> {
  pendingImport = null;
  setImportButtonsEnabled(false);
  renderReport(null);
  const file = input.files?.[0];
  if (!file) {
    setStatus('');
    return;
  }
  try {
    pendingImport = parseImportFile(file.name, await file.text());
    setImportButtonsEnabled(true);
    setStatus(`${file.name} is ready. Preview it to see what would change.`);
  } catch (err) {
    setStatus((err as Error).message);
  }
}

function selectedMode(): OverrideImportMode {
  return byId<HTMLSelectElement>('override-import-mode')?.value === 'best_effort' ? 'best_effort' : 'strict';
}

/** Runs the import (dry run or apply) and renders its per-row report. */
export async function runImport(dryRun: boolean): Promise<void> {
  if (!pendingImport) return;
  setImportButtonsEnabled(false);
  try {
    const report = await api.importAccountServiceOverrides({ ...pendingImport, mode: selectedMode(), dry_run: dryRun });
    renderReport(report);
    setStatus(reportSummary(report));
    if (report.applied) {
      showToast({ message: 'Service overrides imported', kind: 'success' });
    }
  } catch (err) {
    const report = (err as ApiError).details?.['report'] as OverrideImportReport | undefined;
    renderReport(report ?? null);
    setStatus(report ? `${reportSummary(report)} ${friendlyError(err)}` : friendlyError(err));
  } finally {
    setImportButtonsEnabled(pendingImport !== null);
  }
}

async function confirmAndApply(): Promise<void> {
  const ok = await confirmDialog({
    title: 'Import service overrides?',
    body: 'Each row in the file replaces that account\'s override for its service, including clearing fields the row leaves empty. Overrides not in the file are not changed.',
    confirmLabel: 'Import',
  });
  if (ok) await runImport(false);
}

export function reportSummary(report: OverrideImportReport): string {
  const s = report.summary;
  const counts = `${s['insert'] ?? 0} new, ${s['update'] ?? 0} changed, ${s['unchanged'] ?? 0} unchanged, ${s['rejected'] ?? 0} rejected.`;
  if (report.applied) return `Imported: ${counts}`;
  if (report.dry_run) return `Preview (nothing saved): ${counts}`;
  return `Not imported: ${counts}`;
}

function cell(text: string): HTMLTableCellElement {
  const td = document.createElement('td');
  td.textContent = text;
  return td;
}

function renderReport(report: OverrideImportReport | null): void {
  const container = byId<HTMLElement>('override-import-report');
  if (!container) return;
  container.textContent = '';
  if (!report || report.rows.length === 0) return;

  const table = document.createElement('table');
  table.className = 'data-table';
  const head = table.createTHead().insertRow();
  for (const h of ['Row', 'Provider', 'Service', 'Account', 'Result', 'Reason']) {
    const th = document.createElement('th');
    th.textContent = h;
    head.appendChild(th);
  }
  const body = table.createTBody();
  for (const r of report.rows) {
    const tr = body.insertRow();
    tr.appendChild(cell(String(r.row)));
    tr.appendChild(cell(r.provider));
    tr.appendChild(cell(r.service));
    const acct = cell(r.account_id ?? '');
    acct.className = 'override-transfer-account';
    tr.appendChild(acct);
    const badgeTd = document.createElement('td');
    const badge = document.createElement('span');
    badge.className = badgeClass(ACTION_TONES[r.action] ?? 'muted');
    badge.textContent = ACTION_LABELS[r.action] ?? r.action;
    badgeTd.appendChild(badge);
    tr.appendChild(badgeTd);
    tr.appendChild(cell(r.error ?? ''));
  }
  container.appendChild(table);
}

/** Wires the panel once; safe to call on every Accounts tab visit. */
export function initOverrideTransfer(): void {
  const panel = byId<HTMLElement>('accounts-override-transfer');
  if (!panel || panel.dataset['wired'] === '1') return;
  panel.dataset['wired'] = '1';
  byId('override-export-json-btn')?.addEventListener('click', () => void exportOverrides('json'));
  byId('override-export-csv-btn')?.addEventListener('click', () => void exportOverrides('csv'));
  const file = byId<HTMLInputElement>('override-import-file');
  file?.addEventListener('change', () => void onFileChosen(file));
  byId('override-import-preview-btn')?.addEventListener('click', () => void runImport(true));
  byId('override-import-apply-btn')?.addEventListener('click', () => void confirmAndApply());
}

/** Test hook: reset module state between tests. */
export function resetOverrideTransferForTest(): void {
  pendingImport = null;
}
