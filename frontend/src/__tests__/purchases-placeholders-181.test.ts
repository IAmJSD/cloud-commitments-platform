/**
 * Issue #181: Purchases (Approval queue / Purchase History) must never
 * render impossible values. An execution with no recommendations comes back
 * as count 0, term 0, "0 commitment(s)", provider/region "multiple" and $0
 * costs; a mixed-term basket comes back as term 0. Before the fix these
 * rendered as "0 Years", "0", "0 commitment(s)", "Multiple" and "$0.00".
 *
 * Drives loadHistory() with the real utils formatters.
 */

import { loadHistory } from '../history';
import { hasNoCommitments, isRealTerm, isPastScheduledDate } from '../lib/purchase-row-guards';

jest.mock('../api', () => ({
  getHistory: jest.fn(),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  approvePurchase: jest.fn(),
  cancelPurchase: jest.fn(),
  getPurchaseDetails: jest.fn().mockResolvedValue({
    total_upfront_cost: 100,
    estimated_savings: 10,
    recommendations: [{ provider: 'aws', service: 'ec2', region: 'us-east-1', count: 1, term: 1, upfront_cost: 100, savings: 10 }],
  }),
  listAccounts: jest.fn().mockResolvedValue([]),
  getDeploymentInfo: jest.fn().mockResolvedValue({}),
}));

jest.mock('../navigation', () => ({
  switchTab: jest.fn(),
}));

jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn(),
}));

jest.mock('../toast', () => ({
  showToast: jest.fn(),
}));

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

jest.mock('../recommendations', () => ({
  getAccountName: jest.fn((id: string) => id),
}));

import * as api from '../api';
import { getCurrentUser } from '../state';
import { ADMINISTRATORS_GROUP_ID } from '../permissions';

const ADMIN_USER = { id: 'admin-uuid', email: 'admin@example.com', groups: [ADMINISTRATORS_GROUP_ID] };

function setupDOM(): void {
  document.body.innerHTML = `
    <input type="date" id="history-start"><input type="date" id="history-end">
    <div id="history-summary"></div><div id="history-list"></div><div id="purchases-approval-queue"></div>
  `;
}

// The shape internal/api/handler_history.go produces for a pending
// execution that carries no recommendations.
const EMPTY_PENDING = {
  purchase_id: 'exec-empty',
  timestamp: '2026-09-01T00:00:00Z',
  provider: 'multiple',
  service: 'multiple',
  resource_type: '0 commitment(s)',
  region: 'multiple',
  count: 0,
  term: 0,
  payment: '',
  upfront_cost: 0,
  monthly_cost: 0,
  estimated_savings: 0,
  plan_name: 'Full Coverage Purchase',
  status: 'pending',
  created_by_user_id: ADMIN_USER.id,
};

// A real two-commitment basket whose terms differ: term collapses to 0.
const MIXED_TERM_COMPLETED = {
  ...EMPTY_PENDING,
  purchase_id: 'exec-mixed',
  provider: 'aws',
  service: 'ec2',
  resource_type: '2 commitment(s)',
  count: 2,
  upfront_cost: 1200,
  monthly_cost: 40,
  estimated_savings: 90,
  status: 'completed',
};

const cells = (root: string, id: string): string[] =>
  Array.from(document.querySelectorAll(`${root} tr[data-execution-id="${id}"] td`)).map(td => (td.textContent || '').trim());

describe('purchase-row-guards helpers (#181)', () => {
  test('hasNoCommitments is true only for a zero count', () => {
    expect(hasNoCommitments({ count: 0 })).toBe(true);
    expect(hasNoCommitments({ count: 1 })).toBe(false);
  });

  test('isRealTerm rejects 0, negatives, NaN and nullish', () => {
    expect(isRealTerm(1)).toBe(true);
    expect(isRealTerm(3)).toBe(true);
    for (const t of [0, -1, NaN, Infinity, null, undefined]) expect(isRealTerm(t)).toBe(false);
  });

  test('isPastScheduledDate compares calendar days, not instants', () => {
    const now = new Date(2026, 9, 23, 0, 30); // local 2026-10-23 00:30
    expect(isPastScheduledDate('2026-10-22', now)).toBe(true);
    expect(isPastScheduledDate('2026-10-22T23:59:59Z', now)).toBe(true);
    expect(isPastScheduledDate('2026-10-23', now)).toBe(false);
    expect(isPastScheduledDate('2026-11-01', now)).toBe(false);
    expect(isPastScheduledDate('', now)).toBe(false);
    expect(isPastScheduledDate(undefined, now)).toBe(false);
    expect(isPastScheduledDate('not-a-date', now)).toBe(false);
  });
});

describe('Purchases tables never render impossible values (#181)', () => {
  beforeEach(() => {
    setupDOM();
    jest.clearAllMocks();
    (getCurrentUser as jest.Mock).mockReturnValue(ADMIN_USER);
    (api.getHistory as jest.Mock).mockResolvedValue({ summary: {}, purchases: [EMPTY_PENDING, MIXED_TERM_COMPLETED] });
  });

  test('history row with no commitments shows dashes and says why', async () => {
    await loadHistory();
    const row = cells('#history-list', 'exec-empty');
    // Status, Date, Provider, Service, Type, Region, Count, Term, Upfront, Monthly, Savings, Plan
    expect(row.slice(2, 11)).toEqual(['—', '—', 'No recommendations attached', '—', '—', '—', '—', '—', '—']);
    const text = row.join(' ');
    expect(text).not.toMatch(/0 Years|0 commitment\(s\)|Multiple|\$0/);
  });

  test('approval queue row with no commitments shows dashes and says why', async () => {
    await loadHistory();
    const row = cells('#purchases-approval-queue', 'exec-empty');
    // Date, Account, Provider, Service, Count, Term, Payment, Monthly, Upfront, Savings, Created by, Actions
    expect(row.slice(2, 10)).toEqual(['—', 'No recommendations attached', '—', '—', '—', '—', '—', '—']);
  });

  test('a real mixed-term basket keeps its values but shows a dash, not "0 Years", for the term', async () => {
    await loadHistory();
    const row = cells('#history-list', 'exec-mixed');
    expect(row[6]).toBe('2');
    expect(row[7]).toBe('—');
    expect(row[8]).toBe('$1,200');
    expect(row.join(' ')).not.toContain('0 Years');
  });
});
