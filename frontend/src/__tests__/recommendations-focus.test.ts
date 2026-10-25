/**
 * Issue #191: keyboard focus survives the Opportunities table rebuild.
 *
 * Toggling a row checkbox (Space) or the select-all checkbox re-renders the
 * table through innerHTML. Before the fix the focused checkbox was destroyed
 * with the old markup, focus fell back to <body>, and the next Tab restarted
 * at the top of the page. These tests use the real state module so the
 * selection round-trips the way it does in the app.
 */

import { loadRecommendations, resetExpandedCells } from '../recommendations';
import * as state from '../state';
import type { LocalRecommendation } from '../types';

jest.mock('../api', () => ({
  getRecommendations: jest.fn(),
  refreshRecommendations: jest.fn(),
  listAccounts: jest.fn().mockResolvedValue([]),
  listAccountsMinimal: jest.fn().mockResolvedValue([]),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  listAccountServiceOverrides: jest.fn().mockResolvedValue([]),
}));

jest.mock('../api/recommendations', () => ({
  getRecommendationsFreshness: jest.fn().mockResolvedValue({
    last_collected_at: new Date().toISOString(),
    last_collection_error: null,
  }),
  refreshRecommendations: jest.fn().mockResolvedValue({}),
}));

jest.mock('../toast', () => ({
  showToast: jest.fn().mockReturnValue({ dismiss: jest.fn() }),
}));

import * as api from '../api';

const ADMIN_GROUP = '00000000-0000-5000-8000-000000000001';

function rec(id: string, region: string, savings: number): LocalRecommendation {
  return {
    id,
    provider: 'aws',
    cloud_account_id: 'acct-1',
    service: 'ec2',
    resource_type: 'm5.large',
    region,
    count: 1,
    term: 1,
    payment: 'all-upfront',
    savings,
    upfront_cost: 500,
  } as LocalRecommendation;
}

const RECS = [rec('r1', 'us-east-1', 300), rec('r2', 'us-west-2', 200), rec('r3', 'eu-west-1', 100)];

function seedDOM(): void {
  const tab = document.createElement('div');
  tab.id = 'opportunities-tab';
  tab.className = 'tab-content active';
  for (const id of ['recommendations-summary', 'recommendations-toolbar', 'recommendations-list']) {
    const el = document.createElement('div');
    el.id = id;
    tab.appendChild(el);
  }
  document.body.replaceChildren(tab);
}

const rowCheckbox = (id: string): HTMLInputElement =>
  document.querySelector<HTMLInputElement>(`tbody input[data-rec-id="${id}"]`)!;

beforeEach(async () => {
  jest.clearAllMocks();
  resetExpandedCells();
  state.clearSelectedRecommendations();
  state.setCurrentUser({ id: 'u', email: 'u@example.com', groups: [ADMIN_GROUP] } as never);
  (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: RECS, regions: [] });
  seedDOM();
  await loadRecommendations();
});

describe('Opportunities table keeps keyboard focus across re-renders (issue #191)', () => {
  test('checking a row checkbox leaves focus on that row checkbox', () => {
    const before = rowCheckbox('r2');
    before.focus();
    before.checked = true;
    before.dispatchEvent(new Event('change', { bubbles: true }));

    const after = rowCheckbox('r2');
    expect(after).not.toBe(before); // the table really was rebuilt
    expect(after.checked).toBe(true);
    expect(document.activeElement).toBe(after);
  });

  test('unchecking a row checkbox leaves focus on that row checkbox', () => {
    rowCheckbox('r3').focus();
    rowCheckbox('r3').click();
    rowCheckbox('r3').click();

    expect(rowCheckbox('r3').checked).toBe(false);
    expect(document.activeElement).toBe(rowCheckbox('r3'));
  });

  test('toggling select-all leaves focus on select-all', () => {
    const all = document.getElementById('select-all-recs') as HTMLInputElement;
    all.focus();
    all.click();

    const rebuilt = document.getElementById('select-all-recs') as HTMLInputElement;
    expect(rebuilt).not.toBe(all);
    expect(document.activeElement).toBe(rebuilt);
  });

  test('a re-render with focus outside the table does not steal focus', () => {
    const outside = document.createElement('button');
    document.body.appendChild(outside);
    outside.focus();

    const cb = rowCheckbox('r1');
    cb.checked = true;
    cb.dispatchEvent(new Event('change', { bubbles: true }));

    expect(document.activeElement).toBe(outside);
  });
});
