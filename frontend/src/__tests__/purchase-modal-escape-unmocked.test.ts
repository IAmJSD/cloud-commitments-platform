/**
 * Issue #414 (follow-up to #331 / #411): end-to-end Escape-close regression
 * with modal.ts UNMOCKED.
 *
 * purchase-modal-submit.test.ts mocks modal.ts, so its #331 tests invoke the
 * captured onClose directly, and modal.test.ts proves Escape -> closeModal ->
 * onClose separately. Here the whole chain runs for real: a keydown Escape is
 * dispatched on #purchase-modal, the modal is reopened in the other mode
 * (single-row <-> fan-out), Execute is clicked, and the executePurchase
 * payloads must contain only what the newly opened modal shows -- no stale
 * recommendations or fan-out buckets.
 *
 * It also pins that the explicit close button needs no manual state clearing
 * (app.ts relies on the same onClose).
 */

// Mocks must precede imports.


jest.mock('../api', () => ({
  initAuth: jest.fn(),
  isAuthenticated: jest.fn(),
  getCurrentUser: jest.fn(),
  executePurchase: jest.fn(),
  getRecommendations: jest.fn(),
  getConfig: jest.fn().mockResolvedValue({ global: {} }),
  listAccountsMinimal: jest.fn().mockResolvedValue([]),
  listAccountServiceOverrides: jest.fn().mockResolvedValue([]),
}));

jest.mock('../api/recommendations', () => ({
  getRecommendationsFreshness: jest.fn().mockResolvedValue({
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
  // setupRecommendationsHandlers (real — ../recommendations is NOT mocked in
  // this file) subscribes to both on module init via app.ts's
  // setupEventListeners().
  subscribeProvider: jest.fn().mockReturnValue(() => {}),
  subscribeAccount: jest.fn().mockReturnValue(() => {}),
}));

jest.mock('../auth', () => ({
  showLoginModal: jest.fn(),
  updateUserUI: jest.fn(),
}));

jest.mock('../dashboard', () => ({
  loadDashboard: jest.fn().mockResolvedValue(undefined),
  setupDashboardHandlers: jest.fn(),
}));

jest.mock('../navigation', () => ({
  switchTab: jest.fn(),
  applyTabFromPath: jest.fn().mockReturnValue('dashboard'),
  initRouter: jest.fn(),
  switchSettingsSubTab: jest.fn(),
  getSettingsSubTabFromPath: jest.fn().mockReturnValue('general'),
}));

// NOTE: ../recommendations is intentionally NOT mocked — this file exercises
// the real openPurchaseModal / getFanOutBuckets / loadRecommendations.

// closePurchaseModal is the real one (it only routes #purchase-modal through
// modal.ts closeModal); the rest of plans.ts is not needed here.
jest.mock('../plans', () => ({
  savePlan: jest.fn(),
  setupPlanHandlers: jest.fn(),
  closePlanModal: jest.fn(),
  openNewPlanModal: jest.fn(),
  closePurchaseModal: (): void => {
    const modal = document.getElementById('purchase-modal');
    if (modal) (require('../modal') as typeof import('../modal')).closeModal(modal);
  },
}));

jest.mock('../settings', () => ({
  saveGlobalSettings: jest.fn(),
  setupSettingsHandlers: jest.fn(),
  resetSettings: jest.fn(),
}));

jest.mock('../riexchange', () => ({
  setupRIExchangeHandlers: jest.fn(),
  saveAutomationSettings: jest.fn(),
}));

jest.mock('../users', () => ({
  setupUserHandlers: jest.fn(),
}));

jest.mock('../apikeys', () => ({
  initApiKeys: jest.fn(),
}));

jest.mock('../history', () => ({
  loadHistory: jest.fn(),
  setupHistoryHandlers: jest.fn(),
}));

jest.mock('../modules/savings-history', () => ({
  initSavingsHistory: jest.fn(),
}));

jest.mock('../purchases-deeplink', () => ({
  handlePurchaseDeeplink: jest.fn(),
}));

// NOTE: ../modal is intentionally NOT mocked -- the point of this file.

jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn().mockResolvedValue(true),
}));

jest.mock('../archera', () => ({
  handleArcheraDeeplink: jest.fn(),
  openArcheraOfferModal: jest.fn(),
}));

jest.mock('../toast', () => ({
  showToast: jest.fn(),
}));

// Imports

import { setupEventListeners } from '../app';
import * as api from '../api';
import * as state from '../state';
import {
  openPurchaseModal,
  getPurchaseModalRecommendations,
  clearPurchaseModalRecommendations,
  getFanOutBuckets,
  clearFanOutBuckets,
  loadRecommendations,
  seedGlobalDefaults,
} from '../recommendations';
import { ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID } from '../permissions';
import type { LocalRecommendation } from '../types';

// Two AWS rows in different services, so a bulk selection fans out into two
// buckets (ec2 1yr, rds 3yr).
const FAN_OUT_ROWS: LocalRecommendation[] = [
  {
    id: 'fo-ec2', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
    region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'no-upfront',
    count: 1, upfront_cost: 0, monthly_cost: 100, savings: 50,
  },
  {
    id: 'fo-rds', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
    region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: 'all-upfront',
    count: 1, upfront_cost: 1000, monthly_cost: 0, savings: 200,
  },
];

const SINGLE_ROW: LocalRecommendation = {
  id: 'single-only', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
  region: 'us-west-2', resource_type: 'm6i.large', term: 3, payment: 'all-upfront',
  count: 1, upfront_cost: 4000, monthly_cost: 0, savings: 300,
};

/** Drains the microtask queue enough for the async handlers under test to settle. */
async function flush(): Promise<void> {
  for (let i = 0; i < 6; i++) await Promise.resolve();
}

function purchaseModal(): HTMLElement {
  return document.getElementById('purchase-modal')!;
}

function pressEscapeOnPurchaseModal(): void {
  purchaseModal().dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
}

/** Every rec id sent across all executePurchase calls. */
function submittedIds(): string[] {
  return (api.executePurchase as jest.Mock).mock.calls
    .flatMap(([payload]) => payload as Array<Record<string, unknown>>)
    .map((rec) => String(rec['id']));
}

async function openFanOutModal(): Promise<void> {
  (state.getVisibleRecommendations as jest.Mock).mockReturnValue(FAN_OUT_ROWS);
  (state.getSelectedRecommendationIDs as jest.Mock).mockReturnValue(new Set(FAN_OUT_ROWS.map((r) => r.id)));
  await loadRecommendations();
  (document.getElementById('bulk-purchase-btn') as HTMLButtonElement).click();
  await flush();
}

beforeEach(() => {
  document.body.replaceChildren();

  const opportunitiesTab = document.createElement('div');
  opportunitiesTab.id = 'opportunities-tab';
  opportunitiesTab.className = 'tab-content active';
  const summaryEl = document.createElement('div');
  summaryEl.id = 'recommendations-summary';
  const listEl = document.createElement('div');
  listEl.id = 'recommendations-list';
  opportunitiesTab.append(summaryEl, listEl);
  document.body.appendChild(opportunitiesTab);

  const modal = document.createElement('div');
  modal.id = 'purchase-modal';
  modal.className = 'hidden';
  const details = document.createElement('div');
  details.id = 'purchase-details';
  const closeBtn = document.createElement('button');
  closeBtn.id = 'close-purchase-modal-btn';
  modal.append(details, closeBtn);
  document.body.appendChild(modal);

  const executeBtn = document.createElement('button');
  executeBtn.id = 'execute-purchase-btn';
  document.body.appendChild(executeBtn);

  setupEventListeners();

  jest.clearAllMocks();
  clearPurchaseModalRecommendations();
  clearFanOutBuckets();
  seedGlobalDefaults(3, 'all-upfront');

  (localStorage.getItem as jest.Mock).mockReturnValue('{}');
  (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([]);
  (api.getConfig as jest.Mock).mockResolvedValue({ global: { default_payment: 'no-upfront' } });
  (api.getRecommendations as jest.Mock).mockResolvedValue({ summary: {}, recommendations: FAN_OUT_ROWS, regions: [] });
  (api.executePurchase as jest.Mock).mockResolvedValue({
    execution_id: 'exec-aaaaaaaa',
    email_sent: true,
    approval_recipient: 'approver@example.com',
  });
  (state.getCurrentUser as jest.Mock).mockReturnValue({
    id: 'u-admin', email: 'admin@example.com', groups: [ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID],
  });
  (state.getRecommendations as jest.Mock).mockReturnValue([...FAN_OUT_ROWS, SINGLE_ROW]);
});

describe('Issue #414: Escape closes the purchase modal through the real modal.ts', () => {
  test('fan-out -> Escape -> single-row: only the newly displayed recommendation is submitted', async () => {
    await openFanOutModal();
    expect(purchaseModal().classList.contains('hidden')).toBe(false);
    expect(getFanOutBuckets()).toHaveLength(2);

    pressEscapeOnPurchaseModal();

    expect(purchaseModal().classList.contains('hidden')).toBe(true);
    expect(getFanOutBuckets()).toBeNull();

    await openPurchaseModal([SINGLE_ROW]);
    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    expect(api.executePurchase).toHaveBeenCalledTimes(1);
    expect(submittedIds()).toEqual(['single-only']);
  });

  test('single-row -> Escape -> fan-out: the stale single-row selection is never submitted', async () => {
    await openPurchaseModal([SINGLE_ROW]);
    expect(getPurchaseModalRecommendations()).toHaveLength(1);

    pressEscapeOnPurchaseModal();

    expect(purchaseModal().classList.contains('hidden')).toBe(true);
    expect(getPurchaseModalRecommendations()).toEqual([]);

    await openFanOutModal();
    expect(getFanOutBuckets()).toHaveLength(2);
    (document.getElementById('execute-purchase-btn') as HTMLButtonElement).click();
    await flush();

    // One POST per bucket, and only the fan-out rows across them.
    expect(api.executePurchase).toHaveBeenCalledTimes(2);
    expect(submittedIds().sort()).toEqual(['fo-ec2', 'fo-rds']);
  });

  test('the explicit close button discards fan-out state via onClose alone', async () => {
    await openFanOutModal();
    expect(getFanOutBuckets()).toHaveLength(2);

    (document.getElementById('close-purchase-modal-btn') as HTMLButtonElement).click();

    expect(purchaseModal().classList.contains('hidden')).toBe(true);
    expect(getFanOutBuckets()).toBeNull();
    expect(getPurchaseModalRecommendations()).toEqual([]);
  });
});
