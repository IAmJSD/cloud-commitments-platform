/**
 * Service override export/import panel (issue #9).
 */
import fs from 'fs';
import path from 'path';
import {
  exportOverrides,
  initOverrideTransfer,
  parseImportFile,
  reportSummary,
  resetOverrideTransferForTest,
  runImport,
} from '../modules/override-transfer';
import type { OverrideImportReport } from '../api/accounts';

jest.mock('../api', () => ({
  exportAccountServiceOverrides: jest.fn(),
  exportAccountServiceOverridesCSV: jest.fn(),
  importAccountServiceOverrides: jest.fn(),
}));
jest.mock('../toast', () => ({ showToast: jest.fn() }));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn() }));

import * as api from '../api';
import { showToast } from '../toast';
import { confirmDialog } from '../confirmDialog';

const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');

function mountPanel(): void {
  const doc = new DOMParser().parseFromString(html, 'text/html');
  const panel = doc.getElementById('accounts-override-transfer');
  if (!panel) throw new Error('panel missing from index.html');
  document.body.replaceChildren(document.importNode(panel, true));
}

function report(overrides: Partial<OverrideImportReport> = {}): OverrideImportReport {
  return {
    mode: 'strict',
    dry_run: true,
    applied: false,
    summary: { insert: 1, update: 1, unchanged: 0, rejected: 0 },
    rows: [
      { row: 1, account_id: 'a-1', provider: 'aws', service: 'ec2', action: 'update' },
      { row: 2, account_id: 'a-1', provider: 'aws', service: 'rds', action: 'insert' },
    ],
    ...overrides,
  };
}

async function chooseFile(name: string, text: string): Promise<void> {
  const input = document.getElementById('override-import-file') as HTMLInputElement;
  const file = new File([text], name);
  Object.defineProperty(file, 'text', { value: () => Promise.resolve(text) });
  Object.defineProperty(input, 'files', { value: [file], configurable: true });
  input.dispatchEvent(new Event('change'));
  await new Promise((r) => setTimeout(r, 0));
}

const button = (id: string) => document.getElementById(id) as HTMLButtonElement;

beforeEach(() => {
  jest.clearAllMocks();
  resetOverrideTransferForTest();
  mountPanel();
  initOverrideTransfer();
});

describe('parseImportFile', () => {
  test('sends CSV text as is', () => {
    expect(parseImportFile('x.CSV', 'provider,service\naws,ec2')).toEqual({ csv: 'provider,service\naws,ec2' });
  });

  test('accepts an export document or a bare row array', () => {
    const rows = [{ provider: 'aws', service: 'ec2' }];
    expect(parseImportFile('x.json', JSON.stringify({ version: 1, overrides: rows }))).toEqual({ overrides: rows });
    expect(parseImportFile('x.json', JSON.stringify(rows))).toEqual({ overrides: rows });
  });

  test('rejects invalid JSON and JSON without rows', () => {
    expect(() => parseImportFile('x.json', '{')).toThrow('not valid JSON or CSV');
    expect(() => parseImportFile('x.json', '{"rows":[]}')).toThrow('no "overrides" list');
  });
});

describe('reportSummary', () => {
  test('says whether anything was saved', () => {
    expect(reportSummary(report())).toBe('Preview (nothing saved): 1 new, 1 changed, 0 unchanged, 0 rejected.');
    expect(reportSummary(report({ dry_run: false, applied: true }))).toMatch(/^Imported: /);
    expect(reportSummary(report({ dry_run: false }))).toMatch(/^Not imported: /);
  });
});

describe('import flow', () => {
  test('buttons stay disabled until a file is chosen', async () => {
    expect(button('override-import-preview-btn').disabled).toBe(true);
    await chooseFile('o.json', '[{"provider":"aws","service":"ec2"}]');
    expect(button('override-import-preview-btn').disabled).toBe(false);
    expect(button('override-import-apply-btn').disabled).toBe(false);
  });

  test('a bad file explains itself and keeps the buttons disabled', async () => {
    await chooseFile('o.json', 'nope');
    expect(document.getElementById('override-import-status')?.textContent).toContain('not valid JSON or CSV');
    expect(button('override-import-preview-btn').disabled).toBe(true);
  });

  test('preview sends a dry run in the chosen mode and renders the rows', async () => {
    (api.importAccountServiceOverrides as jest.Mock).mockResolvedValue(report());
    await chooseFile('o.csv', 'provider,service\naws,ec2');
    (document.getElementById('override-import-mode') as HTMLSelectElement).value = 'best_effort';

    await runImport(true);

    expect(api.importAccountServiceOverrides).toHaveBeenCalledWith({ csv: 'provider,service\naws,ec2', mode: 'best_effort', dry_run: true });
    const cells = [...document.querySelectorAll('#override-import-report tbody tr')].map((tr) => tr.textContent);
    expect(cells).toEqual(['1awsec2a-1Changed', '2awsrdsa-1New']);
    expect(showToast).not.toHaveBeenCalled();
  });

  test('a strict refusal renders the report from the error details', async () => {
    const refused = report({ summary: { insert: 0, update: 0, unchanged: 0, rejected: 1 }, rows: [
      { row: 1, provider: 'aws', service: 'Bad', action: 'rejected', error: 'invalid service name "Bad"' },
    ] });
    (api.importAccountServiceOverrides as jest.Mock).mockRejectedValue(
      Object.assign(new Error('import rejected: fix the rejected rows or use best_effort mode'), { status: 400, details: { report: refused } }),
    );
    await chooseFile('o.json', '[{"provider":"aws","service":"Bad"}]');

    await runImport(true);

    expect(document.querySelector('#override-import-report tbody')?.textContent).toContain('invalid service name');
    expect(document.getElementById('override-import-status')?.textContent).toContain('1 rejected');
  });

  test('import asks for confirmation before applying', async () => {
    (api.importAccountServiceOverrides as jest.Mock).mockResolvedValue(report({ dry_run: false, applied: true }));
    (confirmDialog as jest.Mock).mockResolvedValueOnce(false).mockResolvedValueOnce(true);
    await chooseFile('o.json', '[{"provider":"aws","service":"ec2"}]');

    button('override-import-apply-btn').click();
    await new Promise((r) => setTimeout(r, 0));
    expect(api.importAccountServiceOverrides).not.toHaveBeenCalled();

    button('override-import-apply-btn').click();
    await new Promise((r) => setTimeout(r, 0));
    expect(api.importAccountServiceOverrides).toHaveBeenCalledWith(expect.objectContaining({ dry_run: false, mode: 'strict' }));
    expect(showToast).toHaveBeenCalledWith({ message: 'Service overrides imported', kind: 'success' });
  });
});

describe('export', () => {
  beforeEach(() => {
    URL.createObjectURL = jest.fn(() => 'blob:x');
    URL.revokeObjectURL = jest.fn();
  });

  test('downloads JSON and CSV exports', async () => {
    (api.exportAccountServiceOverrides as jest.Mock).mockResolvedValue({ version: 1, exported_at: 't', overrides: [] });
    (api.exportAccountServiceOverridesCSV as jest.Mock).mockResolvedValue('account_id\n');
    const clicks: string[] = [];
    jest.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clicks.push(this.download);
    });

    await exportOverrides('json');
    await exportOverrides('csv');

    expect(clicks).toHaveLength(2);
    expect(clicks[0]).toMatch(/^cudly-service-overrides-\d{4}-\d{2}-\d{2}\.json$/);
    expect(clicks[1]).toMatch(/\.csv$/);
  });

  test('an export failure is a toast', async () => {
    (api.exportAccountServiceOverrides as jest.Mock).mockRejectedValue(new Error('boom'));
    await exportOverrides('json');
    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'error' }));
  });
});
