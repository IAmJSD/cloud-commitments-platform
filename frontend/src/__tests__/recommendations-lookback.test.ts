/**
 * Issue #909: Opportunities lookback selector tests.
 *
 * Verifies:
 *   - the 7/30/60-day selector prefills from /api/config
 *   - admin sees an editable control with the AWS-scope tooltip
 *   - non-admin (user / readonly) sees the control disabled with the
 *     no-permission tooltip (backend stays authoritative)
 *   - changing the value persists only the lookback (#530), triggers a
 *     recommendations refresh, and reloads the list
 *   - a persist failure toasts the error and reverts the selector
 *   - a tampered/out-of-range value is rejected client-side
 */
import {
  loadRecommendations,
  resetCachedGlobalConfig,
  resetAutoRefreshInFlight,
  LOOKBACK_SCOPE_HINT,
} from '../recommendations';
import * as api from '../api';
import { refreshRecommendations as refreshRecsAPI } from '../api/recommendations';
import { showToast } from '../toast';
import { ADMINISTRATORS_GROUP_ID } from '../permissions';

jest.mock('../api', () => ({
  getRecommendations: jest.fn().mockResolvedValue({ summary: {}, recommendations: [], regions: [] }),
  refreshRecommendations: jest.fn(),
  getConfig: jest.fn(),
  updateConfig: jest.fn().mockResolvedValue({}),
  listAccounts: jest.fn().mockResolvedValue([]),
  listAccountsMinimal: jest.fn().mockResolvedValue([]),
  listAccountServiceOverrides: jest.fn().mockResolvedValue([]),
}));

jest.mock('../api/recommendations', () => ({
  getRecommendationsFreshness: jest.fn().mockResolvedValue({
    // Fresh (within the 24h budget) so the auto-refresh-on-open path does
    // not fire and interfere with the explicit change-driven refresh.
    last_collected_at: new Date().toISOString(),
    last_collection_error: null,
  }),
  refreshRecommendations: jest.fn().mockResolvedValue({}),
}));

jest.mock('../state', () => ({
  getCurrentProvider: jest.fn().mockReturnValue('all'),
  setCurrentProvider: jest.fn(),
  getCurrentAccountIDs: jest.fn().mockReturnValue([]),
  setCurrentAccountIDs: jest.fn(),
  getRecommendations: jest.fn().mockReturnValue([]),
  getRecommendationByID: jest.fn().mockReturnValue(undefined),
  setRecommendations: jest.fn(),
  getSelectedRecommendationIDs: jest.fn().mockReturnValue(new Set()),
  clearSelectedRecommendations: jest.fn(),
  addSelectedRecommendation: jest.fn(),
  removeSelectedRecommendation: jest.fn(),
  getRecommendationsSort: jest.fn().mockReturnValue({ column: 'savings', direction: 'desc' }),
  setRecommendationsSort: jest.fn(),
  getRecommendationsColumnFilters: jest.fn().mockReturnValue({}),
  setRecommendationsColumnFilter: jest.fn(),
  clearAllRecommendationsColumnFilters: jest.fn(),
  getVisibleRecommendations: jest.fn().mockReturnValue([]),
  setVisibleRecommendations: jest.fn(),
  getCostPeriod: jest.fn().mockReturnValue('monthly'),
  setCostPeriod: jest.fn(),
  getHiddenColumns: jest.fn().mockReturnValue(new Set()),
  setHiddenColumns: jest.fn(),
  getCurrentUser: jest.fn(),
}));

jest.mock('../toast', () => ({
  showToast: jest.fn().mockReturnValue({ dismiss: jest.fn() }),
}));

import * as state from '../state';

const mockUser = (role: string | null) => {
  (state.getCurrentUser as jest.Mock).mockReturnValue(
    role === null ? null : { id: 'u', email: 'u@example.com', groups: role === 'admin' ? [ADMINISTRATORS_GROUP_ID] : [] },
  );
};

const setupDom = () => {
  const recsTab = document.createElement('div');
  recsTab.id = 'opportunities-tab';
  recsTab.className = 'tab-content active';
  const summary = document.createElement('div');
  summary.id = 'recommendations-summary';
  const toolbar = document.createElement('div');
  toolbar.id = 'recommendations-toolbar';
  toolbar.className = 'recommendations-toolbar';
  const list = document.createElement('div');
  list.id = 'recommendations-list';
  recsTab.append(summary, toolbar, list);
  document.body.replaceChildren(recsTab);
};

const getSelect = () => document.getElementById('recs-lookback-days') as HTMLSelectElement | null;

// A realistic full GlobalConfig, so the change handler has sibling fields
// in its cache that it must not re-send on save.
const baseConfig = {
  enabled_providers: ['aws', 'azure', 'gcp'],
  default_term: 3,
  default_payment: 'all-upfront',
  default_coverage: 80,
  auto_collect: true,
  collection_schedule: 'daily',
  notification_days_before: 7,
  recommendations_cache_stale_hours: 24,
  recommendations_lookback_days: 30,
};

beforeEach(() => {
  jest.clearAllMocks();
  resetCachedGlobalConfig();
  resetAutoRefreshInFlight();
  setupDom();
  (api.getConfig as jest.Mock).mockResolvedValue({ global: baseConfig });
});

describe('Opportunities lookback selector (issue #909)', () => {
  test('prefills the selector from the current config value', async () => {
    mockUser('admin');
    await loadRecommendations();
    const select = getSelect();
    expect(select).not.toBeNull();
    expect(select!.value).toBe('30');
    // All three accepted enum options present.
    expect([...select!.options].map(o => o.value)).toEqual(['7', '30', '60']);
  });

  test('defaults to 7 when the config omits the lookback field', async () => {
    mockUser('admin');
    (api.getConfig as jest.Mock).mockResolvedValue({ global: { enabled_providers: ['aws'] } });
    await loadRecommendations();
    expect(getSelect()!.value).toBe('7');
  });

  test('admin sees an enabled control with the AWS-scope tooltip', async () => {
    mockUser('admin');
    await loadRecommendations();
    const select = getSelect()!;
    expect(select.disabled).toBe(false);
    const tip = document.querySelector('#recommendations-toolbar .tooltip-text')!;
    expect(tip.textContent).toContain('AWS recommendations only');
    expect(tip.textContent).toContain('Azure and GCP are unaffected');
  });

  // Issue #191: the scope is visible inline, not only in the hover tooltip.
  test.each(['admin', 'user'])('shows a visible AWS-only scope hint tied to the select (%s)', async (role) => {
    mockUser(role);
    await loadRecommendations();
    const select = getSelect()!;
    const hint = document.getElementById('recs-lookback-scope')!;
    expect(hint).not.toBeNull();
    expect(hint.textContent).toBe(LOOKBACK_SCOPE_HINT);
    expect(hint.textContent).toContain('AWS');
    expect(hint.closest('.tooltip-text')).toBeNull();
    expect(select.getAttribute('aria-describedby')).toBe('recs-lookback-scope');
  });

  test('re-rendering the toolbar keeps a single scope hint', async () => {
    mockUser('admin');
    await loadRecommendations();
    await loadRecommendations();
    expect(document.querySelectorAll('#recs-lookback-scope')).toHaveLength(1);
  });

  test.each(['user', 'readonly', null])('non-admin (%s) sees a disabled control with a permission tooltip', async (role) => {
    mockUser(role);
    await loadRecommendations();
    const select = getSelect()!;
    expect(select.disabled).toBe(true);
    const tip = document.querySelector('#recommendations-toolbar .tooltip-text')!;
    expect(tip.textContent).toContain('admin');
  });

  test('changing the value persists only the lookback and refreshes', async () => {
    mockUser('admin');
    await loadRecommendations();
    const select = getSelect()!;

    select.value = '60';
    select.dispatchEvent(new Event('change'));
    // Let the async change handler settle (persist + refresh + reload).
    await new Promise(r => setTimeout(r, 0));
    await new Promise(r => setTimeout(r, 0));

    // Issue #530: the cache is stale once another admin saves a newer
    // default_term, so re-sending it would revert that change. The backend
    // preserves omitted fields.
    expect(api.updateConfig).toHaveBeenCalledTimes(1);
    expect(api.updateConfig).toHaveBeenCalledWith({ recommendations_lookback_days: 60 });

    // Re-collect triggered for the new window.
    expect(refreshRecsAPI).toHaveBeenCalledTimes(1);
  });

  test('does not rebuild the cache from a pre-await copy when the cache changes during the PUT', async () => {
    mockUser('admin');
    await loadRecommendations();
    const select = getSelect()!;

    let resolvePut!: (v: { status: string }) => void;
    (api.updateConfig as jest.Mock).mockReturnValueOnce(
      new Promise(r => { resolvePut = r; }),
    );
    // Hold the post-save re-collect so the cache can be inspected before the
    // final reload refetches the config.
    (refreshRecsAPI as jest.Mock).mockReturnValueOnce(new Promise(() => {}));

    select.value = '60';
    select.dispatchEvent(new Event('change'));
    await new Promise(r => setTimeout(r, 0));

    // The cache is cleared while the PUT is pending. The handler must merge
    // onto the current cache after the await; the pre-fix code resurrected
    // the pre-await copy with the new lookback.
    resetCachedGlobalConfig();
    resolvePut({ status: 'updated' });
    await new Promise(r => setTimeout(r, 0));
    await new Promise(r => setTimeout(r, 0));

    // Re-render the toolbar from the cache only (config fetch fails).
    (api.getConfig as jest.Mock).mockRejectedValue(new Error('offline'));
    await loadRecommendations();
    expect(getSelect()!.value).toBe('7');
  });

  test('does nothing when the value is unchanged', async () => {
    mockUser('admin');
    await loadRecommendations();
    const select = getSelect()!;
    select.value = '30'; // same as baseConfig
    select.dispatchEvent(new Event('change'));
    await new Promise(r => setTimeout(r, 0));
    expect(api.updateConfig).not.toHaveBeenCalled();
    expect(refreshRecsAPI).not.toHaveBeenCalled();
  });

  test('persist failure toasts an error and reverts the selector', async () => {
    mockUser('admin');
    (api.updateConfig as jest.Mock).mockRejectedValueOnce(new Error('boom'));
    await loadRecommendations();
    const select = getSelect()!;

    select.value = '60';
    select.dispatchEvent(new Event('change'));
    await new Promise(r => setTimeout(r, 0));
    await new Promise(r => setTimeout(r, 0));

    expect(refreshRecsAPI).not.toHaveBeenCalled();
    // Reverted to the last-known value, re-enabled.
    expect(select.value).toBe('30');
    expect(select.disabled).toBe(false);
    const errToast = (showToast as jest.Mock).mock.calls.find(
      (c) => c[0].kind === 'error' && /lookback/i.test(c[0].message),
    );
    expect(errToast).toBeTruthy();
  });

  test.each(['999', '0', '-1', '0x3c', '7e0', '', ' 7', 'abc'])(
    'tampered/out-of-range value "%s" is rejected client-side without persisting',
    async (badValue) => {
      mockUser('admin');
      await loadRecommendations();
      const select = getSelect()!;

      // Directly invoke the change handler with an injected bad value to
      // simulate a tampered DOM (the real <select> only exposes the three
      // enum options, so dispatchEvent would fire the legitimate value).
      select.value = badValue;
      select.dispatchEvent(new Event('change'));
      await new Promise(r => setTimeout(r, 0));

      // No persist should have been attempted.
      expect(api.updateConfig).not.toHaveBeenCalled();
      // Selector reverted to last-known value.
      expect(select.value).toBe('30');
      // Error toast surfaced.
      const errToast = (showToast as jest.Mock).mock.calls.find(
        (c) => c[0].kind === 'error',
      );
      expect(errToast).toBeTruthy();
    },
  );
});
