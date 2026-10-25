/**
 * History truncation banner (issue #248): the API flags `truncated` when a
 * fetch hit its row cap; the UI must say so above the queue and the table.
 */

import { loadHistory } from '../history';

jest.mock('../api', () => ({
  getHistory: jest.fn(),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  listAccounts: jest.fn().mockResolvedValue([]),
  getDeploymentInfo: jest.fn().mockResolvedValue({}),
}));
jest.mock('../navigation', () => ({ switchTab: jest.fn() }));
jest.mock('../utils', () => ({
  NO_VALUE: '\u2014',
  formatCurrency: jest.fn((val) => `$${val || 0}`),
  formatDate: jest.fn((val) => (val ? new Date(val).toLocaleDateString() : '')),
  formatTerm: jest.fn((years) => (years == null ? '' : `${years} Year${years === 1 ? '' : 's'}`)),
  escapeHtml: jest.fn((str) => str || ''),
  escapeHtmlAttr: jest.fn((str: string | null | undefined) => str || ''),
  populateAccountFilter: jest.fn(() => Promise.resolve()),
}));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn() }));
jest.mock('../toast', () => ({ showToast: jest.fn() }));
jest.mock('../state', () => ({
  getCurrentUser: jest.fn(),
  getCurrentProvider: jest.fn().mockReturnValue(''),
  setCurrentProvider: jest.fn(),
  getCurrentAccountIDs: jest.fn().mockReturnValue([]),
  setCurrentAccountIDs: jest.fn(),
  subscribeProvider: jest.fn().mockReturnValue(() => {}),
  subscribeAccount: jest.fn().mockReturnValue(() => {}),
  getAmortizeUpfront: jest.fn().mockReturnValue(false),
  setAmortizeUpfront: jest.fn(),
  subscribeAmortizeUpfront: jest.fn().mockReturnValue(() => {}),
  getPurchaseHistoryColumnFilters: jest.fn().mockReturnValue({}),
  setPurchaseHistoryColumnFilter: jest.fn(),
  clearAllPurchaseHistoryColumnFilters: jest.fn(),
  getApprovalQueueColumnFilters: jest.fn().mockReturnValue({}),
  setApprovalQueueColumnFilter: jest.fn(),
  clearAllApprovalQueueColumnFilters: jest.fn(),
}));
jest.mock('../recommendations', () => ({ getAccountName: jest.fn((id: string) => id) }));

import * as api from '../api';
import { showToast } from '../toast';

function setupDOM(): void {
  while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
  const ids = ['history-start', 'history-end'];
  for (const id of ids) {
    const el = document.createElement('input');
    el.type = 'date';
    el.id = id;
    document.body.appendChild(el);
  }
  for (const id of ['history-summary', 'history-list', 'purchases-approval-queue']) {
    const el = document.createElement('div');
    el.id = id;
    document.body.appendChild(el);
  }
  for (const id of ['purchases-truncation-notice', 'history-truncation-notice']) {
    const el = document.createElement('p');
    el.id = id;
    el.hidden = true;
    document.body.appendChild(el);
  }
}

describe('History truncation banner (issue #248)', () => {
  beforeEach(() => {
    setupDOM();
    jest.clearAllMocks();
  });

  test('shows the banner above both tables when the API reports truncation', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [], truncated: true, limit: 100, executions_limit: 100 });
    await loadHistory();
    for (const id of ['purchases-truncation-notice', 'history-truncation-notice']) {
      const el = document.getElementById(id)!;
      expect(el.hidden).toBe(false);
      expect(el.textContent).toContain('100');
    }
  });

  test('keeps the banner hidden when the response is not truncated', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [], truncated: false, limit: 100 });
    await loadHistory();
    for (const id of ['purchases-truncation-notice', 'history-truncation-notice']) {
      expect(document.getElementById(id)!.hidden).toBe(true);
    }
  });

  test('clears a previous banner on a later untruncated load', async () => {
    (api.getHistory as jest.Mock).mockResolvedValueOnce({ summary: {}, purchases: [], truncated: true, limit: 100, executions_limit: 100 });
    await loadHistory();
    (api.getHistory as jest.Mock).mockResolvedValueOnce({ summary: {}, purchases: [] });
    await loadHistory();
    expect(document.getElementById('history-truncation-notice')!.hidden).toBe(true);
    expect(document.getElementById('history-truncation-notice')!.textContent).toBe('');
  });

  test('hides a previous banner when the next load fails', async () => {
    (api.getHistory as jest.Mock).mockResolvedValueOnce({ summary: {}, purchases: [], truncated: true, limit: 100, executions_limit: 100 });
    await loadHistory();
    (api.getHistory as jest.Mock).mockRejectedValueOnce(new Error('boom'));
    jest.spyOn(console, 'error').mockImplementation(() => {});
    await loadHistory();
    for (const id of ['purchases-truncation-notice', 'history-truncation-notice']) {
      expect(document.getElementById(id)!.hidden).toBe(true);
    }
  });
});

describe('History empty window (issue #282)', () => {
  beforeEach(() => {
    setupDOM();
    const controls = document.createElement('div');
    controls.id = 'history-controls';
    document.body.appendChild(controls);
    jest.clearAllMocks();
  });
  afterEach(() => {
    window.history.replaceState({}, '', window.location.pathname);
  });

  test('mounts the amortize checkbox even when the list is empty', async () => {
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [] });
    await loadHistory();
    expect(document.getElementById('history-amortize-checkbox')).not.toBeNull();
  });

  test('toasts and clears the deep link when the execution is outside an empty window', async () => {
    window.history.replaceState({}, '', '/purchases#history?execution=exec-123');
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [] });
    await loadHistory();
    expect(showToast).toHaveBeenCalledWith(expect.objectContaining({ message: expect.stringContaining("isn't in the current view") }));
    expect(window.location.hash).not.toContain('execution=');
  });
});
