/**
 * Shared Jest harness for the purchase-modal regression suites
 * (purchase-modal-*.test.ts): the module mocks, the fixtures and the DOM /
 * mock reset every suite runs before each test.
 *
 * Import this module FIRST in each suite. Its jest.mock() calls are hoisted
 * above its own imports and register before the suite imports ../app or
 * ../recommendations, so mock and module initialization order is the same as
 * when all suites lived in one file. This file is not a test (jest only
 * collects *.test.ts).
 */

// Module mocks

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

// NOTE: ../recommendations is intentionally NOT mocked — these suites exercise
// the real openPurchaseModal / getFanOutBuckets / loadRecommendations.

jest.mock('../plans', () => ({
  savePlan: jest.fn(),
  setupPlanHandlers: jest.fn(),
  closePlanModal: jest.fn(),
  openNewPlanModal: jest.fn(),
  closePurchaseModal: jest.fn(),
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

jest.mock('../modal', () => ({
  openModal: jest.fn(),
  closeModal: jest.fn(),
}));

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
  clearPurchaseModalRecommendations,
  clearFanOutBuckets,
  seedGlobalDefaults,
} from '../recommendations';
import { ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID } from '../permissions';
import type { LocalRecommendation } from '../types';

// Fixtures

// One AWS EC2 cell fanned out into its four (term, payment) variants — the
// same shape providers/aws/recommendations/client.go produces for a single
// physical resource. Every #1903 test reads from a fresh copy of this list
// via buildRows() so no test can leak a mutation into another.
export function buildRows(): LocalRecommendation[] {
  return [
    {
      id: 'v-3-all', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
      region: 'us-east-1', resource_type: 'm5.large', count: 2, term: 3,
      payment: 'all-upfront', upfront_cost: 36000, monthly_cost: 0, savings: 900,
    },
    {
      id: 'v-3-partial', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
      region: 'us-east-1', resource_type: 'm5.large', count: 2, term: 3,
      payment: 'partial-upfront', upfront_cost: 18000, monthly_cost: 300, savings: 850,
    },
    {
      id: 'v-1-all', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
      region: 'us-east-1', resource_type: 'm5.large', count: 2, term: 1,
      payment: 'all-upfront', upfront_cost: 12000, monthly_cost: 0, savings: 700,
    },
    {
      id: 'v-1-no', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
      region: 'us-east-1', resource_type: 'm5.large', count: 1, term: 1,
      payment: 'no-upfront', upfront_cost: 0, monthly_cost: 800, savings: 500,
    },
  ];
}

/** Drains the microtask queue enough for the async handlers under test to settle. */
export async function flush(): Promise<void> {
  for (let i = 0; i < 6; i++) await Promise.resolve();
}

export function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
} {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

// Two buckets for the #1904 fan-out suites: a priced EC2 row and an RDS row
// with no payment, which the fan-out modal must skip.
export function buildFanOutRows(): LocalRecommendation[] {
  return [
    {
      id: 'ec2-1', provider: 'aws', cloud_account_id: 'a1', service: 'ec2',
      region: 'us-east-1', resource_type: 'm5.large', term: 1, payment: 'no-upfront',
      count: 1, upfront_cost: 0, monthly_cost: 100, savings: 50,
    },
    {
      id: 'rds-3', provider: 'aws', cloud_account_id: 'a1', service: 'rds',
      region: 'us-east-1', resource_type: 'db.r5.large', term: 3, payment: undefined,
      count: 1, upfront_cost: 1000, savings: 200,
    },
  ];
}

// DOM / mock scaffolding

/** Rebuilds the purchase-modal DOM and resets every mock; run before each test. */
export function setupPurchaseModalTest(): void {
  document.body.replaceChildren();

  const opportunitiesTab = document.createElement('div');
  opportunitiesTab.id = 'opportunities-tab';
  opportunitiesTab.className = 'tab-content active';
  const summaryEl = document.createElement('div');
  summaryEl.id = 'recommendations-summary';
  const listEl = document.createElement('div');
  listEl.id = 'recommendations-list';
  opportunitiesTab.appendChild(summaryEl);
  opportunitiesTab.appendChild(listEl);
  document.body.appendChild(opportunitiesTab);

  const purchaseModal = document.createElement('div');
  purchaseModal.id = 'purchase-modal';
  purchaseModal.className = 'hidden';
  const purchaseDetails = document.createElement('div');
  purchaseDetails.id = 'purchase-details';
  purchaseModal.appendChild(purchaseDetails);
  document.body.appendChild(purchaseModal);

  const executeBtn = document.createElement('button');
  executeBtn.id = 'execute-purchase-btn';
  document.body.appendChild(executeBtn);

  const closeBtn = document.createElement('button');
  closeBtn.id = 'close-purchase-modal-btn';
  purchaseModal.appendChild(closeBtn);

  setupEventListeners();

  jest.clearAllMocks();
  clearPurchaseModalRecommendations();
  clearFanOutBuckets();
  seedGlobalDefaults(3, 'all-upfront');

  // loadBulkPurchaseState() (setup.ts's localStorage mock defaults getItem to
  // null) only reads cachedGlobalDefaultPayment when a raw value is present —
  // otherwise it falls back to the hardcoded 'all-upfront' default and never
  // consults GlobalConfig. Seed a truthy (capacity-only) value so the
  // bulk-purchase toolbar picks up the mocked getConfig() default_payment;
  // tests that need a specific capacity override this per-test.
  (localStorage.getItem as jest.Mock).mockReturnValue('{}');
  (api.listAccountServiceOverrides as jest.Mock).mockResolvedValue([]);
  (api.getConfig as jest.Mock).mockResolvedValue({ global: {} });
  (api.executePurchase as jest.Mock).mockResolvedValue({
    execution_id: 'exec-aaaaaaaa',
    email_sent: true,
    approval_recipient: 'approver@example.com',
  });
  (state.getCurrentUser as jest.Mock).mockReturnValue({
    id: 'u-admin', email: 'admin@example.com', groups: [ADMINISTRATORS_GROUP_ID, PURCHASER_GROUP_ID],
  });
  // Every #1903 test needs the full loaded cell so pricedCellVariant /
  // cellTermOptions / cellPaymentOptions can find the sibling rows.
  (state.getRecommendations as jest.Mock).mockReturnValue(buildRows());
}
