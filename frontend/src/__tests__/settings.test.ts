/**
 * Settings module tests
 */
import {
  loadGlobalSettings,
  saveGlobalSettings,
  resetSettings,
  setupSettingsHandlers,
  copyToClipboard,
  isUnsavedChanges,
  openOverrideModal,
} from '../settings';

// Mock the api module
jest.mock('../api', () => ({
  getConfig: jest.fn(),
  updateConfig: jest.fn(),
  updateServiceConfig: jest.fn().mockResolvedValue(undefined),
  listAccounts: jest.fn(),
  createAccount: jest.fn(),
  updateAccount: jest.fn(),
  deleteAccount: jest.fn(),
  testAccountCredentials: jest.fn(),
  saveAccountCredentials: jest.fn(),
  getLadderConfigs: jest.fn().mockResolvedValue([]),
}));

// Mock federation module — loadGlobalSettings fires initFederationPanel void-style,
// and we don't want real federation code touching the test DOM.
jest.mock('../federation', () => ({
  initFederationPanel: jest.fn().mockResolvedValue(undefined),
}));

// Mock confirmDialog so tests can control confirm/cancel without driving
// the real modal UI. mockConfirmDialog default is "confirmed" (true);
// individual tests can mockResolvedValueOnce(false) to simulate cancel.
const mockConfirmDialog = jest.fn<Promise<boolean>, [unknown]>(() => Promise.resolve(true));
jest.mock('../confirmDialog', () => ({
  confirmDialog: (opts: unknown) => mockConfirmDialog(opts),
}));

// Q7: saveGlobalSettings + error paths now use showToast.
const mockShowToast = jest.fn<{ dismiss: () => void }, [unknown]>(() => ({ dismiss: jest.fn() }));
jest.mock('../toast', () => ({
  showToast: (opts: unknown) => mockShowToast(opts),
}));

import * as api from '../api';

describe('Settings Module', () => {
  beforeEach(() => {
    // Reset DOM
    document.body.innerHTML = `
      <div id="settings-loading" class="hidden"></div>
      <form id="global-settings-form" class="hidden">
        <input type="checkbox" id="provider-aws">
        <input type="checkbox" id="provider-azure">
        <input type="checkbox" id="provider-gcp">
        <div id="aws-settings" class="hidden"></div>
        <div id="azure-settings" class="hidden"></div>
        <div id="gcp-settings" class="hidden"></div>
        <input type="email" id="setting-notification-email">
        <input type="checkbox" id="setting-auto-collect">
        <div id="collection-schedule-row" class="hidden">
          <select id="setting-collection-schedule">
            <option value="daily">Daily</option>
            <option value="weekly">Weekly</option>
          </select>
        </div>
        <select id="setting-default-term">
          <option value="1">1</option>
          <option value="3">3</option>
        </select>
        <select id="setting-default-payment">
          <option value="no-upfront">No Upfront</option>
          <option value="partial-upfront">Partial Upfront</option>
          <option value="all-upfront">All Upfront</option>
        </select>
        <input type="number" id="setting-default-coverage" min="0" max="100">
        <input type="number" id="setting-notification-days" min="1" max="30">
        <input type="number" id="setting-recs-stale-hours" value="24" min="0" max="8760">
        <select id="setting-recs-lookback-days">
          <option value="7" selected>7 days</option>
          <option value="30">30 days</option>
          <option value="60">60 days</option>
        </select>
        <!-- AWS term/payment selects -->
        <select id="aws-ec2-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="aws-rds-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="aws-elasticache-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="aws-opensearch-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="aws-redshift-term"><option value="1">1</option><option value="3">3</option></select>
        <!-- Issue #22 follow-up: per-plan-type Savings Plans cards
             (Compute / EC2 Instance / SageMaker / Database). The earlier
             umbrella savingsplans and PR #71 sagemaker cards have
             been replaced. -->
        <select id="aws-savings-plans-compute-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="aws-savings-plans-ec2instance-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="aws-savings-plans-sagemaker-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="aws-savings-plans-database-term"><option value="1">1</option><option value="3">3</option></select>
        <!-- Issue #136: per-product SP coverage and enabled controls -->
        <input type="number" id="aws-savings-plans-compute-coverage" min="0" max="100" value="80">
        <input type="checkbox" id="aws-savings-plans-compute-enabled" checked>
        <input type="number" id="aws-savings-plans-ec2instance-coverage" min="0" max="100" value="80">
        <input type="checkbox" id="aws-savings-plans-ec2instance-enabled" checked>
        <input type="number" id="aws-savings-plans-sagemaker-coverage" min="0" max="100" value="80">
        <input type="checkbox" id="aws-savings-plans-sagemaker-enabled" checked>
        <input type="number" id="aws-savings-plans-database-coverage" min="0" max="100" value="80">
        <input type="checkbox" id="aws-savings-plans-database-enabled" checked>
        <select id="aws-ec2-payment"><option value="no-upfront">No</option><option value="partial-upfront">Partial</option><option value="all-upfront">All</option></select>
        <select id="aws-rds-payment"><option value="no-upfront">No</option><option value="partial-upfront">Partial</option><option value="all-upfront">All</option></select>
        <select id="aws-elasticache-payment"><option value="no-upfront">No</option><option value="partial-upfront">Partial</option><option value="all-upfront">All</option></select>
        <select id="aws-opensearch-payment"><option value="no-upfront">No</option><option value="partial-upfront">Partial</option><option value="all-upfront">All</option></select>
        <select id="aws-redshift-payment"><option value="no-upfront">No</option><option value="partial-upfront">Partial</option><option value="all-upfront">All</option></select>
        <select id="aws-savings-plans-compute-payment"><option value="no-upfront">No</option><option value="partial-upfront">Partial</option><option value="all-upfront">All</option></select>
        <select id="aws-savings-plans-ec2instance-payment"><option value="no-upfront">No</option><option value="partial-upfront">Partial</option><option value="all-upfront">All</option></select>
        <select id="aws-savings-plans-sagemaker-payment"><option value="no-upfront">No</option><option value="partial-upfront">Partial</option><option value="all-upfront">All</option></select>
        <select id="aws-savings-plans-database-payment"><option value="no-upfront">No</option><option value="partial-upfront">Partial</option><option value="all-upfront">All</option></select>
        <!-- Azure term selects -->
        <select id="azure-vm-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="azure-sql-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="azure-cosmos-term"><option value="1">1</option><option value="3">3</option></select>
        <!-- GCP term selects -->
        <select id="gcp-compute-term"><option value="1">1</option><option value="3">3</option></select>
        <select id="gcp-sql-term"><option value="1">1</option><option value="3">3</option></select>
        <!-- Per-provider grace-period inputs (issue #466 reset-to-defaults assertions
             call populateGraceInput which needs these in the DOM). -->
        <input type="number" id="setting-grace-aws" value="7" min="0" max="30">
        <input type="number" id="setting-grace-azure" value="7" min="0" max="30">
        <input type="number" id="setting-grace-gcp" value="7" min="0" max="30">
        <!-- Issue #466: reflectDirtyState (in settings-subnav.ts) toggles
             .has-unsaved on #admin-tab-btn and .dirty/.settings-savebar on
             every .settings-buttons element. Seed both so the reset-to-
             defaults expansion + unsaved-indicator assertions have DOM to
             check. -->
        <div class="settings-buttons"></div>
      </form>
      <button id="admin-tab-btn"></button>
      <div id="settings-error" class="hidden"></div>
      <!-- Test element for copyToClipboard -->
      <code id="test-copy-element">test-value</code>
      <button class="copy-btn"><span class="copy-icon">Copy</span></button>
    `;
    // Seed the default selects to match prod's <option selected> markup so
    // cascade-confirm tests have an unambiguous baseline ("3 Years" term,
    // "all-upfront" payment) to transition away from.
    (document.getElementById('setting-default-term') as HTMLSelectElement).value = '3';
    (document.getElementById('setting-default-payment') as HTMLSelectElement).value = 'all-upfront';
    // Mirror the same baseline onto every per-service select so the cascade
    // diff only reports genuinely-changed rows.
    ['aws-ec2-term','aws-rds-term','aws-elasticache-term','aws-opensearch-term','aws-redshift-term','aws-savings-plans-compute-term','aws-savings-plans-ec2instance-term','aws-savings-plans-sagemaker-term','aws-savings-plans-database-term','azure-vm-term','azure-sql-term','azure-cosmos-term','gcp-compute-term','gcp-sql-term'].forEach(id => {
      const el = document.getElementById(id) as HTMLSelectElement | null;
      if (el) el.value = '3';
    });
    ['aws-ec2-payment','aws-rds-payment','aws-elasticache-payment','aws-opensearch-payment','aws-redshift-payment','aws-savings-plans-compute-payment','aws-savings-plans-ec2instance-payment','aws-savings-plans-sagemaker-payment','aws-savings-plans-database-payment'].forEach(id => {
      const el = document.getElementById(id) as HTMLSelectElement | null;
      if (el) el.value = 'all-upfront';
    });

    jest.clearAllMocks();
    // clearAllMocks only clears call history; flush the queued
    // mockResolvedValueOnce stack too so a test that doesn't consume a
    // queued value can't leak it into the next test.
    mockConfirmDialog.mockReset();
    mockConfirmDialog.mockImplementation(() => Promise.resolve(true));
  });

  describe('setupSettingsHandlers', () => {
    test('sets up provider checkbox event handlers', () => {
      setupSettingsHandlers();

      const awsCheck = document.getElementById('provider-aws') as HTMLInputElement;
      const awsSettings = document.getElementById('aws-settings');

      // Trigger change event
      awsCheck.checked = true;
      awsCheck.dispatchEvent(new Event('change'));

      expect(awsSettings?.classList.contains('hidden')).toBe(false);
    });

    test('sets up azure checkbox event handler', () => {
      setupSettingsHandlers();

      const azureCheck = document.getElementById('provider-azure') as HTMLInputElement;
      const azureSettings = document.getElementById('azure-settings');

      azureCheck.checked = true;
      azureCheck.dispatchEvent(new Event('change'));

      expect(azureSettings?.classList.contains('hidden')).toBe(false);
    });

    test('sets up gcp checkbox event handler', () => {
      setupSettingsHandlers();

      const gcpCheck = document.getElementById('provider-gcp') as HTMLInputElement;
      const gcpSettings = document.getElementById('gcp-settings');

      gcpCheck.checked = true;
      gcpCheck.dispatchEvent(new Event('change'));

      expect(gcpSettings?.classList.contains('hidden')).toBe(false);
    });

    test('sets up auto-collect checkbox to toggle schedule visibility', () => {
      setupSettingsHandlers();

      const autoCollect = document.getElementById('setting-auto-collect') as HTMLInputElement;
      const scheduleRow = document.getElementById('collection-schedule-row');

      autoCollect.checked = true;
      autoCollect.dispatchEvent(new Event('change'));

      expect(scheduleRow?.classList.contains('hidden')).toBe(false);

      autoCollect.checked = false;
      autoCollect.dispatchEvent(new Event('change'));

      expect(scheduleRow?.classList.contains('hidden')).toBe(true);
    });

    test('sets up default term to propagate to services (after confirm)', async () => {
      setupSettingsHandlers();
      mockConfirmDialog.mockResolvedValueOnce(true);

      // Baseline is "3" via the parent beforeEach; flip to "1" so the
      // cascade has real work to do.
      const defaultTerm = document.getElementById('setting-default-term') as HTMLSelectElement;
      defaultTerm.value = '1';
      defaultTerm.dispatchEvent(new Event('change'));
      await new Promise((r) => setTimeout(r, 0));

      expect(mockConfirmDialog).toHaveBeenCalledTimes(1);
      expect((document.getElementById('aws-ec2-term') as HTMLSelectElement).value).toBe('1');
      expect((document.getElementById('aws-rds-term') as HTMLSelectElement).value).toBe('1');
      expect((document.getElementById('azure-vm-term') as HTMLSelectElement).value).toBe('1');
      expect((document.getElementById('gcp-compute-term') as HTMLSelectElement).value).toBe('1');
    });

    test('sets up default payment to propagate to AWS services (after confirm), clamping where per-service constraints reject the value', async () => {
      setupSettingsHandlers();
      mockConfirmDialog.mockResolvedValueOnce(true);

      const defaultPayment = document.getElementById('setting-default-payment') as HTMLSelectElement;
      defaultPayment.value = 'no-upfront';
      defaultPayment.dispatchEvent(new Event('change'));
      await new Promise((r) => setTimeout(r, 0));

      expect(mockConfirmDialog).toHaveBeenCalledTimes(1);
      // EC2 accepts no-upfront at both terms — propagation lands as-is.
      expect((document.getElementById('aws-ec2-payment') as HTMLSelectElement).value).toBe('no-upfront');
      // RDS 3yr rejects no-upfront (parent beforeEach seeds all service
      // terms at "3"), so the constraint sync clamps RDS back to the
      // first valid payment option instead of persisting an invalid
      // combination the provider will refuse.
      const rdsPayment = (document.getElementById('aws-rds-payment') as HTMLSelectElement).value;
      expect(rdsPayment).not.toBe('no-upfront');
      expect(['partial-upfront', 'all-upfront']).toContain(rdsPayment);
    });

    test('cancelling the cascade restores the default term to its prior value', async () => {
      setupSettingsHandlers();
      mockConfirmDialog.mockResolvedValueOnce(false);

      const defaultTerm = document.getElementById('setting-default-term') as HTMLSelectElement;
      // Baseline: all services at "3" (seeded in beforeEach). Attempt to switch to "1".
      defaultTerm.value = '1';
      defaultTerm.dispatchEvent(new Event('change'));
      await new Promise((r) => setTimeout(r, 0));

      expect(defaultTerm.value).toBe('3');
      expect((document.getElementById('aws-ec2-term') as HTMLSelectElement).value).toBe('3');
    });

    test('cascade confirm skipped when no services would change', async () => {
      setupSettingsHandlers();
      // All services and default already at "3" (via parent beforeEach).
      // Flip default to "3" (self) and assert no prompt fires.
      const defaultTerm = document.getElementById('setting-default-term') as HTMLSelectElement;
      defaultTerm.value = '3';
      defaultTerm.dispatchEvent(new Event('change'));
      await new Promise((r) => setTimeout(r, 0));

      expect(mockConfirmDialog).not.toHaveBeenCalled();
    });

  });

  describe('loadGlobalSettings', () => {
    test('shows loading state and hides form initially', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['aws'],
          notification_email: 'test@example.com',
          auto_collect: true,
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3
        },
        credentials: {}
      });

      const loadingEl = document.getElementById('settings-loading');
      const formEl = document.getElementById('global-settings-form');

      const promise = loadGlobalSettings();

      // After starting, loading should be shown
      expect(loadingEl?.classList.contains('hidden')).toBe(false);
      expect(formEl?.classList.contains('hidden')).toBe(true);

      await promise;
    });

    test('populates form with config data', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['aws', 'azure'],
          notification_email: 'admin@example.com',
          auto_collect: true,
          default_term: 1,
          default_payment: 'partial-upfront',
          default_coverage: 90,
          notification_days_before: 5
        },
        credentials: {}
      });

      await loadGlobalSettings();

      expect((document.getElementById('provider-aws') as HTMLInputElement).checked).toBe(true);
      expect((document.getElementById('provider-azure') as HTMLInputElement).checked).toBe(true);
      expect((document.getElementById('provider-gcp') as HTMLInputElement).checked).toBe(false);
      expect((document.getElementById('setting-notification-email') as HTMLInputElement).value).toBe('admin@example.com');
      expect((document.getElementById('setting-auto-collect') as HTMLInputElement).checked).toBe(true);
      expect((document.getElementById('setting-default-term') as HTMLSelectElement).value).toBe('1');
      expect((document.getElementById('setting-default-payment') as HTMLSelectElement).value).toBe('partial-upfront');
      expect((document.getElementById('setting-default-coverage') as HTMLInputElement).value).toBe('90');
      expect((document.getElementById('setting-notification-days') as HTMLInputElement).value).toBe('5');
    });

    test('populates 4-eyes checkbox from config.global.require_different_approver (issue #1005)', async () => {
      const checkbox = document.createElement('input');
      checkbox.type = 'checkbox';
      checkbox.id = 'setting-require-different-approver';
      document.getElementById('global-settings-form')?.appendChild(checkbox);

      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: [],
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3,
          require_different_approver: true
        },
        credentials: {}
      });

      await loadGlobalSettings();

      expect(checkbox.checked).toBe(true);
    });

    test('populates collection schedule from config', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: [],
          collection_schedule: 'weekly',
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3
        },
        credentials: {}
      });

      await loadGlobalSettings();

      expect((document.getElementById('setting-collection-schedule') as HTMLSelectElement).value).toBe('weekly');
    });

    test('handles auto_collect being false explicitly', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: [],
          auto_collect: false,
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3
        },
        credentials: {}
      });

      await loadGlobalSettings();

      expect((document.getElementById('setting-auto-collect') as HTMLInputElement).checked).toBe(false);
    });

    test('shows error on API failure', async () => {
      (api.getConfig as jest.Mock).mockRejectedValue(new Error('API Error'));

      await loadGlobalSettings();

      const errorEl = document.getElementById('settings-error');
      expect(errorEl?.classList.contains('hidden')).toBe(false);
      expect(errorEl?.textContent).toContain('Failed to load settings');
    });

    test('hides loading and shows form on success', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: [],
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3
        },
        credentials: {}
      });

      await loadGlobalSettings();

      const loadingEl = document.getElementById('settings-loading');
      const formEl = document.getElementById('global-settings-form');

      expect(loadingEl?.classList.contains('hidden')).toBe(true);
      expect(formEl?.classList.contains('hidden')).toBe(false);
    });

    test('handles missing global config gracefully', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        credentials: {}
      });

      await loadGlobalSettings();

      // Should not throw and form should be visible
      const formEl = document.getElementById('global-settings-form');
      expect(formEl?.classList.contains('hidden')).toBe(false);
    });

    // Issue #301: recommendations cycle params populated from API
    test('populates recommendations_cache_stale_hours and recommendations_lookback_days', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['aws'],
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3,
          recommendations_cache_stale_hours: 48,
          recommendations_lookback_days: 30,
        },
      });

      await loadGlobalSettings();

      expect((document.getElementById('setting-recs-stale-hours') as HTMLInputElement).value).toBe('48');
      expect((document.getElementById('setting-recs-lookback-days') as HTMLSelectElement).value).toBe('30');
    });

    test('populates recommendations fields with defaults when absent from API response', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['aws'],
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3,
          // recommendations_cache_stale_hours and recommendations_lookback_days absent
        },
      });

      await loadGlobalSettings();

      expect((document.getElementById('setting-recs-stale-hours') as HTMLInputElement).value).toBe('24');
      expect((document.getElementById('setting-recs-lookback-days') as HTMLSelectElement).value).toBe('7');
    });

    test('handles missing credentials config gracefully', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['aws'],
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3
        }
      });

      await loadGlobalSettings();

      // Should not throw
      const formEl = document.getElementById('global-settings-form');
      expect(formEl?.classList.contains('hidden')).toBe(false);
    });

    test('populates service card selects from data.services', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['aws'],
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3
        },
        services: [
          { provider: 'aws', service: 'ec2', enabled: true, term: 1, payment: 'no-upfront', coverage: 70 },
          { provider: 'aws', service: 'rds', enabled: true, term: 3, payment: 'all-upfront', coverage: 80 }
        ]
      });

      await loadGlobalSettings();

      expect((document.getElementById('aws-ec2-term') as HTMLSelectElement).value).toBe('1');
      expect((document.getElementById('aws-ec2-payment') as HTMLSelectElement).value).toBe('no-upfront');
      expect((document.getElementById('aws-rds-term') as HTMLSelectElement).value).toBe('3');
    });
  });

  describe('saveGlobalSettings', () => {
    const fillForm = (): void => {
      // Set up form values
      (document.getElementById('provider-aws') as HTMLInputElement).checked = true;
      (document.getElementById('provider-azure') as HTMLInputElement).checked = false;
      (document.getElementById('provider-gcp') as HTMLInputElement).checked = true;
      (document.getElementById('setting-notification-email') as HTMLInputElement).value = 'test@test.com';
      (document.getElementById('setting-auto-collect') as HTMLInputElement).checked = true;
      (document.getElementById('setting-default-term') as HTMLSelectElement).value = '3';
      (document.getElementById('setting-default-payment') as HTMLSelectElement).value = 'all-upfront';
      (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '80';
      (document.getElementById('setting-notification-days') as HTMLInputElement).value = '3';
    };

    beforeEach(fillForm);

    // Load a pristine config so the saved snapshot matches the form, as in the
    // real page; an edit made afterwards is then the only dirty field.
    const loadPristine = async (): Promise<void> => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['aws'], default_term: 3, default_payment: 'all-upfront',
          default_coverage: 80, notification_days_before: 3,
        },
        services: [
          { provider: 'aws', service: 'savings-plans-compute', enabled: true, term: 3, payment: 'all-upfront', coverage: 80 },
          { provider: 'aws', service: 'savings-plans-sagemaker', enabled: true, term: 3, payment: 'all-upfront', coverage: 80 },
          { provider: 'aws', service: 'savings-plans-database', enabled: true, term: 3, payment: 'all-upfront', coverage: 80 },
        ],
      });
      await loadGlobalSettings();
    };

    test('prevents default form submission', async () => {
      (api.updateConfig as jest.Mock).mockResolvedValue({});

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      expect(event.preventDefault).toHaveBeenCalled();
    });

    test('collects form data and calls updateConfig', async () => {
      // Load a config that differs in every field, then fill the form, so each
      // field is dirty and the PUT carries all of them.
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['azure'],
          notification_email: 'old@test.com',
          auto_collect: false,
          default_term: 1,
          default_payment: 'no-upfront',
          default_coverage: 10,
          notification_days_before: 5,
          grace_period_days: { aws: 1, azure: 2, gcp: 3 },
          recommendations_cache_stale_hours: 12,
          recommendations_lookback_days: 30,
        },
        services: [],
      });
      await loadGlobalSettings();
      fillForm();
      (api.updateConfig as jest.Mock).mockClear().mockResolvedValue({});
      window.alert = jest.fn();

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      // The harness has no schedule, grace, stale-hours, lookback, offering-class
      // or approver elements, so those read the same before and after and are
      // not sent.
      expect(api.updateConfig).toHaveBeenCalledWith({
        enabled_providers: ['aws', 'gcp'],
        notification_email: 'test@test.com',
        auto_collect: true,
        default_term: 3,
        default_payment: 'all-upfront',
        default_coverage: 80,
        notification_days_before: 3,
      });
    });

    test('shows success alert on save', async () => {
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      window.alert = jest.fn();

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({
        message: 'Settings saved successfully',
        kind: 'success',
      }));
    });

    test('sends require_different_approver: true when the 4-eyes checkbox is checked (issue #1005)', async () => {
      const checkbox = document.createElement('input');
      checkbox.type = 'checkbox';
      checkbox.id = 'setting-require-different-approver';
      checkbox.checked = true;
      document.getElementById('global-settings-form')?.appendChild(checkbox);

      (api.updateConfig as jest.Mock).mockResolvedValue({});
      window.alert = jest.fn();

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      expect(api.updateConfig).toHaveBeenCalledWith(expect.objectContaining({
        require_different_approver: true,
      }));
    });

    test('shows error alert on failure', async () => {
      (api.updateConfig as jest.Mock).mockRejectedValue(new Error('Save failed'));
      window.alert = jest.fn();

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({
        message: 'Failed to save settings: Save failed',
        kind: 'error',
      }));
    });

    test('handles missing form elements gracefully', async () => {
      // Remove all form elements
      document.body.innerHTML = '';
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      window.alert = jest.fn();

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      // Should still call updateConfig with default values
      expect(api.updateConfig).toHaveBeenCalled();
    });

    // Issue #22 follow-up: AWS Savings Plans split into four per-plan-type
    // cards (Compute / EC2 Instance / SageMaker / Database). Each gets
    // a dedicated card in index.html so users can pin term/payment per
    // workload independently. Lambda is intentionally absent — it has no
    // standalone SP product (Lambda commitments roll into Compute SP).
    test('four per-plan-type SP cards present in index.html (issue #22)', () => {
      const fs = require('fs') as typeof import('fs');
      const path = require('path') as typeof import('path');
      const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf-8');
      expect(html).toMatch(/<h5>\s*Compute Savings Plans\s*<\/h5>/);
      expect(html).toMatch(/<h5>\s*EC2 Instance Savings Plans\s*<\/h5>/);
      expect(html).toMatch(/<h5>\s*SageMaker Savings Plans\s*<\/h5>/);
      expect(html).toMatch(/<h5>\s*Database Savings Plans\s*<\/h5>/);
      for (const planType of ['compute', 'ec2instance', 'sagemaker', 'database']) {
        expect(html).toMatch(new RegExp(`id="aws-savings-plans-${planType}-term"`));
        expect(html).toMatch(new RegExp(`id="aws-savings-plans-${planType}-payment"`));
        // Issue #136: coverage and enabled controls must be present on each card.
        expect(html).toMatch(new RegExp(`id="aws-savings-plans-${planType}-coverage"`));
        expect(html).toMatch(new RegExp(`id="aws-savings-plans-${planType}-enabled"`));
      }
    });

    test('no Lambda card in index.html — Lambda has no standalone SP, only Compute SP (issue #22)', () => {
      const fs = require('fs') as typeof import('fs');
      const path = require('path') as typeof import('path');
      const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf-8');
      // Guard against accidental re-introduction of a Lambda-specific card.
      expect(html).not.toMatch(/<h5>\s*Lambda Savings Plans\s*<\/h5>/);
      expect(html).not.toMatch(/id="aws-lambda-term"/);
      expect(html).not.toMatch(/id="aws-lambda-payment"/);
    });

    test('saveGlobalSettings sends per-plan-type SP term and payment (issue #22)', async () => {
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
      window.alert = jest.fn();

      // User pins SageMaker SP to 1yr / no-upfront while leaving Compute
      // SP at the global default (3yr / all-upfront, matching the seeded
      // baseline from beforeEach).
      (document.getElementById('aws-savings-plans-sagemaker-term') as HTMLSelectElement).value = '1';
      (document.getElementById('aws-savings-plans-sagemaker-payment') as HTMLSelectElement).value = 'no-upfront';

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      const call = (api.updateServiceConfig as jest.Mock).mock.calls.find(
        ([provider, service]) => provider === 'aws' && service === 'savings-plans-sagemaker',
      );
      expect(call).toBeDefined();
      const cfg = call![2];
      expect(cfg.term).toBe(1);
      expect(cfg.payment).toBe('no-upfront');
    });

    test('saveGlobalSettings sends per-card coverage from SP coverage input (issue #136)', async () => {
      await loadPristine();
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
      window.alert = jest.fn();

      // Pin compute SP to 60% coverage, leave others at default 80%.
      (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = '60';

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      const computeCall = (api.updateServiceConfig as jest.Mock).mock.calls.find(
        ([provider, service]) => provider === 'aws' && service === 'savings-plans-compute',
      );
      expect(computeCall).toBeDefined();
      expect(computeCall![2].coverage).toBe(60);

      // Other SP cards were not edited, so they are not sent (#539).
      const ec2Call = (api.updateServiceConfig as jest.Mock).mock.calls.find(
        ([provider, service]) => provider === 'aws' && service === 'savings-plans-ec2instance',
      );
      expect(ec2Call).toBeUndefined();
    });

    describe('global PUT carries only changed fields (issue #539)', () => {
      const loadStaleTab = async (): Promise<void> => {
        (api.getConfig as jest.Mock).mockResolvedValue({
          global: {
            enabled_providers: ['aws'],
            notification_email: 'old@test.com',
            default_term: 1,
            default_payment: 'no-upfront',
            default_coverage: 50,
            notification_days_before: 3,
          },
          services: [
            { provider: 'aws', service: 'ec2', enabled: true, term: 1, payment: 'no-upfront', coverage: 50 },
            { provider: 'aws', service: 'savings-plans-compute', enabled: true, term: 1, payment: 'no-upfront', coverage: 50 },
          ],
        });
        (api.updateConfig as jest.Mock).mockClear().mockResolvedValue({});
        (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
        await loadGlobalSettings();
      };
      const save = (): Promise<void> => saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

      test('saving an unrelated field does not send the cached term, payment or coverage', async () => {
        await loadStaleTab();
        (document.getElementById('setting-notification-email') as HTMLInputElement).value = 'new@test.com';

        await save();

        expect(api.updateConfig).toHaveBeenCalledTimes(1);
        expect(api.updateConfig).toHaveBeenCalledWith({ notification_email: 'new@test.com' });
      });

      test('changing only the term sends only the term', async () => {
        await loadStaleTab();
        (document.getElementById('setting-default-term') as HTMLSelectElement).value = '3';

        await save();

        expect(api.updateConfig).toHaveBeenCalledWith({ default_term: 3 });
      });

      test('changing the coverage sends only the coverage and no service PUTs', async () => {
        await loadStaleTab();
        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '70';

        await save();

        expect(api.updateConfig).toHaveBeenCalledWith({ default_coverage: 70 });
        expect(api.updateServiceConfig).not.toHaveBeenCalled();
      });

      test('a second save does not resend a field the first save persisted', async () => {
        await loadStaleTab();
        (document.getElementById('setting-notification-email') as HTMLInputElement).value = 'new@test.com';
        await save();
        (api.updateConfig as jest.Mock).mockClear();

        await save();

        expect(api.updateConfig).not.toHaveBeenCalled();
      });

      test('a rejected global PUT leaves the field dirty, so the next save resends it', async () => {
        await loadStaleTab();
        (document.getElementById('setting-notification-email') as HTMLInputElement).value = 'new@test.com';
        (api.updateConfig as jest.Mock).mockRejectedValueOnce(new Error('boom'));

        await save();
        expect(api.updateConfig).toHaveBeenCalledTimes(1);
        (api.updateConfig as jest.Mock).mockClear().mockResolvedValue({});

        await save();

        expect(api.updateConfig).toHaveBeenCalledTimes(1);
        expect(api.updateConfig).toHaveBeenCalledWith({ notification_email: 'new@test.com' });
      });

      test('a field edited while the global PUT is in flight is still sent by the next save', async () => {
        await loadStaleTab();
        (document.getElementById('setting-notification-email') as HTMLInputElement).value = 'new@test.com';
        let resolvePut!: () => void;
        (api.updateConfig as jest.Mock).mockImplementationOnce(
          () => new Promise<void>(resolve => { resolvePut = resolve; }),
        );

        const firstSave = save();
        (document.getElementById('setting-default-term') as HTMLSelectElement).value = '3';
        resolvePut();
        await firstSave;
        (api.updateConfig as jest.Mock).mockClear().mockResolvedValue({});

        await save();

        expect(api.updateConfig).toHaveBeenCalledWith({ default_term: 3 });
      });

      test('a sent field edited again while its PUT is in flight is still sent by the next save', async () => {
        await loadStaleTab();
        const email = document.getElementById('setting-notification-email') as HTMLInputElement;
        email.value = 'first@test.com';
        let resolvePut!: () => void;
        (api.updateConfig as jest.Mock).mockImplementationOnce(
          () => new Promise<void>(resolve => { resolvePut = resolve; }),
        );

        const firstSave = save();
        email.value = 'second@test.com';
        resolvePut();
        await firstSave;
        expect(api.updateConfig).toHaveBeenCalledWith({ notification_email: 'first@test.com' });
        (api.updateConfig as jest.Mock).mockClear().mockResolvedValue({});

        await save();

        expect(api.updateConfig).toHaveBeenCalledWith({ notification_email: 'second@test.com' });
      });

      test('an unchanged form sends no global PUT and no service PUTs', async () => {
        await loadStaleTab();

        await save();

        expect(api.updateConfig).not.toHaveBeenCalled();
        expect(api.updateServiceConfig).not.toHaveBeenCalled();
      });

      test('editing one service card PUTs only that row and only the edited field', async () => {
        await loadStaleTab();
        (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = '65';

        await save();

        expect(api.updateConfig).not.toHaveBeenCalled();
        expect(api.updateServiceConfig).toHaveBeenCalledTimes(1);
        expect(api.updateServiceConfig).toHaveBeenCalledWith('aws', 'savings-plans-compute', expect.objectContaining({ coverage: 65 }));
        const body = (api.updateServiceConfig as jest.Mock).mock.calls[0][2];
        expect(Object.keys(body).sort()).toEqual(['coverage', 'provider', 'service']);
      });

      test('editing only a recommendation filter sends only that filter', async () => {
        await loadStaleTab();
        const regions = document.createElement('input');
        regions.id = 'aws-ec2-include-regions';
        regions.value = 'us-east-1, eu-west-1';
        document.getElementById('global-settings-form')!.appendChild(regions);

        await save();

        expect(api.updateServiceConfig).toHaveBeenCalledTimes(1);
        expect(api.updateServiceConfig).toHaveBeenCalledWith('aws', 'ec2', {
          provider: 'aws', service: 'ec2', include_regions: ['us-east-1', 'eu-west-1'],
        });
      });

      test('a filter saved by one Save is not resent by the next', async () => {
        await loadStaleTab();
        const regions = document.createElement('input');
        regions.id = 'aws-ec2-include-regions';
        regions.value = 'us-east-1';
        document.getElementById('global-settings-form')!.appendChild(regions);
        await save();
        expect(api.updateServiceConfig).toHaveBeenCalledTimes(1);
        (api.updateServiceConfig as jest.Mock).mockClear();

        await save();

        expect(api.updateServiceConfig).not.toHaveBeenCalled();
      });

      test('an edited SP coverage cleared to empty blocks the save and stays dirty', async () => {
        await loadStaleTab();
        const card = document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement;
        card.value = '';
        mockShowToast.mockClear();

        await save();

        expect(api.updateConfig).not.toHaveBeenCalled();
        expect(api.updateServiceConfig).not.toHaveBeenCalled();
        expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({
          message: expect.stringContaining('coverage must be a whole number between 0 and 100 (savings-plans-compute)'),
          kind: 'error',
        }));
        expect(isUnsavedChanges()).toBe(true);

        card.value = '65';
        await save();
        expect(api.updateServiceConfig).toHaveBeenCalledWith('aws', 'savings-plans-compute', expect.objectContaining({ coverage: 65 }));
      });

      test('a second save after a service PUT succeeded resends nothing', async () => {
        await loadStaleTab();
        (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = '65';
        await save();
        (api.updateServiceConfig as jest.Mock).mockClear();

        await save();

        expect(api.updateServiceConfig).not.toHaveBeenCalled();
      });
    });

    describe('global coverage propagation (issue #522)', () => {
      const loadWithCoverage50 = async (): Promise<void> => {
        (api.getConfig as jest.Mock).mockResolvedValue({
          global: { enabled_providers: ['aws'], default_term: 3, default_payment: 'all-upfront', default_coverage: 50 },
          services: [
            { provider: 'aws', service: 'ec2', enabled: true, term: 3, payment: 'all-upfront', coverage: 50 },
            { provider: 'aws', service: 'savings-plans-compute', enabled: true, term: 3, payment: 'all-upfront', coverage: 50 },
          ],
        });
        (api.updateConfig as jest.Mock).mockResolvedValue({});
        (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
        await loadGlobalSettings();
      };
      const coverageSentFor = (service: string): number | undefined => {
        const call = (api.updateServiceConfig as jest.Mock).mock.calls.find(
          ([provider, svc]) => provider === 'aws' && svc === service,
        );
        return call?.[2].coverage;
      };

      test('a changed global coverage sends no service PUTs and shows the propagated value on the SP cards', async () => {
        await loadWithCoverage50();
        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '0';

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(api.updateConfig).toHaveBeenCalledWith({ default_coverage: 0 });
        expect(api.updateServiceConfig).not.toHaveBeenCalled();
        expect((document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value).toBe('0');
        expect(isUnsavedChanges()).toBe(false);
      });

      test('an SP card coverage edited in the same save wins over the new default and is the only service PUT', async () => {
        await loadWithCoverage50();
        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '70';
        (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = '60';

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(coverageSentFor('ec2')).toBeUndefined();
        expect(coverageSentFor('savings-plans-compute')).toBe(60);
        expect(api.updateServiceConfig).toHaveBeenCalledTimes(1);
        expect((document.getElementById('aws-savings-plans-ec2instance-coverage') as HTMLInputElement).value).toBe('70');
      });

      test('a second save without reload resends nothing', async () => {
        await loadWithCoverage50();
        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '70';
        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);
        (api.updateConfig as jest.Mock).mockClear();
        (api.updateServiceConfig as jest.Mock).mockClear();

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(api.updateConfig).not.toHaveBeenCalled();
        expect(api.updateServiceConfig).not.toHaveBeenCalled();
      });

      test('an SP card edit saved after a global change is not resent by a further save', async () => {
        await loadWithCoverage50();
        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '0';
        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);
        (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = '70';
        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);
        expect(coverageSentFor('savings-plans-compute')).toBe(70);
        (api.updateServiceConfig as jest.Mock).mockClear();

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(api.updateServiceConfig).not.toHaveBeenCalled();
        expect((document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value).toBe('70');
      });

      test('a saved SP card edit does not shield the card from a later global change', async () => {
        await loadWithCoverage50();
        (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = '70';
        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);
        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '20';
        (api.updateServiceConfig as jest.Mock).mockClear();

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(api.updateServiceConfig).not.toHaveBeenCalled();
        expect((document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value).toBe('20');
      });

      test('after a partial failure the retry resends only the failed SP card coverage', async () => {
        await loadWithCoverage50();
        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '0';
        (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = '70';
        (document.getElementById('aws-savings-plans-ec2instance-coverage') as HTMLInputElement).value = '30';
        (api.updateServiceConfig as jest.Mock).mockImplementation(async (provider: string, svc: string) => {
          if (provider === 'aws' && svc === 'savings-plans-ec2instance') throw new Error('sp failed');
        });

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);
        expect(coverageSentFor('savings-plans-compute')).toBe(70);

        (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(coverageSentFor('savings-plans-compute')).toBeUndefined();
        expect(coverageSentFor('savings-plans-ec2instance')).toBe(30);
      });

      test('after a failed service PUT a further global change still reaches an unedited SP card', async () => {
        await loadWithCoverage50();
        (document.getElementById('aws-savings-plans-ec2instance-coverage') as HTMLInputElement).value = '30';
        (api.updateServiceConfig as jest.Mock).mockImplementation(async (provider: string, svc: string) => {
          if (provider === 'aws' && svc === 'savings-plans-ec2instance') throw new Error('sp failed');
        });
        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '20';
        (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(coverageSentFor('savings-plans-ec2instance')).toBe(30);
        expect((document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value).toBe('20');
      });

      test('a rejected global PUT leaves the coverage dirty and sends no service PUTs', async () => {
        await loadWithCoverage50();
        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '70';
        (api.updateConfig as jest.Mock).mockRejectedValueOnce(new Error('boom'));

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(api.updateServiceConfig).not.toHaveBeenCalled();
        expect(isUnsavedChanges()).toBe(true);
      });

      test('an SP card coverage whose service PUT failed stays dirty and is resent', async () => {
        await loadWithCoverage50();
        (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = '70';
        (api.updateServiceConfig as jest.Mock).mockImplementation(async (provider: string, svc: string) => {
          if (provider === 'aws' && svc === 'savings-plans-compute') throw new Error('sp failed');
        });

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);
        expect(isUnsavedChanges()).toBe(true);

        (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(coverageSentFor('savings-plans-compute')).toBe(70);
        expect(isUnsavedChanges()).toBe(false);
      });

      test('an SP card coverage typed during its pending PUT is kept and sent by the next save (#576)', async () => {
        await loadWithCoverage50();
        const card = document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement;
        card.value = '60';
        let resolvePut!: () => void;
        (api.updateServiceConfig as jest.Mock).mockImplementation((provider: string, svc: string) =>
          provider === 'aws' && svc === 'savings-plans-compute'
            ? new Promise<void>(resolve => { resolvePut = resolve; })
            : Promise.resolve());

        const firstSave = saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);
        await new Promise(r => setTimeout(r, 0));
        card.value = '77';
        resolvePut();
        await firstSave;

        expect(coverageSentFor('savings-plans-compute')).toBe(60);
        expect(card.value).toBe('77');
        expect(isUnsavedChanges()).toBe(true);

        (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(coverageSentFor('savings-plans-compute')).toBe(77);
        expect(isUnsavedChanges()).toBe(false);
      });

      test.each(['12.7', '-40', '500'])('an SP card coverage of %s aborts the save before any PUT (#318)', async (value) => {
        await loadWithCoverage50();
        mockShowToast.mockClear();
        (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = value;

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(api.updateConfig).not.toHaveBeenCalled();
        expect(api.updateServiceConfig).not.toHaveBeenCalled();
        expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({
          kind: 'error',
          message: expect.stringContaining('whole number between 0 and 100 (savings-plans-compute)'),
        }));
        expect(isUnsavedChanges()).toBe(true);
      });

      test.each([['0', 0], ['100', 100]])('an SP card coverage of %s is within range and saved (#318)', async (value, sent) => {
        await loadWithCoverage50();
        (document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value = value;

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(coverageSentFor('savings-plans-compute')).toBe(sent);
      });

      test('a saved service payment edit leaves nothing unsaved (#576)', async () => {
        await loadWithCoverage50();
        (document.getElementById('aws-ec2-payment') as HTMLSelectElement).value = 'partial-upfront';
        expect(isUnsavedChanges()).toBe(true);

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(isUnsavedChanges()).toBe(false);
      });

      test('an empty global coverage field is rejected instead of saving 0', async () => {
        await loadWithCoverage50();
        (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '';

        await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

        expect(api.updateConfig).not.toHaveBeenCalled();
        expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({
          message: 'Dashboard coverage target must be a whole number between 0 and 100',
          kind: 'error',
        }));
      });
    });

    test('saveGlobalSettings sends per-card enabled=false when SP enabled checkbox unchecked (issue #136)', async () => {
      await loadPristine();
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
      window.alert = jest.fn();

      // Disable the database SP card.
      (document.getElementById('aws-savings-plans-database-enabled') as HTMLInputElement).checked = false;

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      const dbCall = (api.updateServiceConfig as jest.Mock).mock.calls.find(
        ([provider, service]) => provider === 'aws' && service === 'savings-plans-database',
      );
      expect(dbCall).toBeDefined();
      expect(dbCall![2].enabled).toBe(false);

      // Untouched SP cards are not sent at all (#539).
      const computeCall = (api.updateServiceConfig as jest.Mock).mock.calls.find(
        ([provider, service]) => provider === 'aws' && service === 'savings-plans-compute',
      );
      expect(computeCall).toBeUndefined();
    });

    test('loadGlobalSettings populates SP coverage and enabled from service config (issue #136)', async () => {
      // Seed a non-default initial DOM state on the fallback card so the test
      // fails if the fallback assignment is skipped (it would otherwise pass on
      // the HTML defaults, which happen to equal the asserted values).
      (document.getElementById('aws-savings-plans-ec2instance-coverage') as HTMLInputElement).value = '12';
      (document.getElementById('aws-savings-plans-ec2instance-enabled') as HTMLInputElement).checked = false;

      (api.getConfig as jest.Mock).mockResolvedValue({
        // Non-80 global default so the fallback writes an observably different value.
        global: { enabled_providers: ['aws'], default_term: 3, default_payment: 'all-upfront', default_coverage: 67 },
        services: [
          { provider: 'aws', service: 'savings-plans-compute', term: 1, payment: 'no-upfront', coverage: 65, enabled: false },
        ],
      });
      setupSettingsHandlers();
      await loadGlobalSettings();

      const coverageEl = document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement;
      const enabledEl = document.getElementById('aws-savings-plans-compute-enabled') as HTMLInputElement;
      expect(coverageEl.value).toBe('65');
      expect(enabledEl.checked).toBe(false);

      // Cards without an explicit service row fall back to the global default
      // (67, not the HTML default 80) and re-enable from the seeded false state.
      const ec2Coverage = document.getElementById('aws-savings-plans-ec2instance-coverage') as HTMLInputElement;
      const ec2Enabled = document.getElementById('aws-savings-plans-ec2instance-enabled') as HTMLInputElement;
      expect(ec2Coverage.value).toBe('67');
      expect(ec2Enabled.checked).toBe(true);
    });

    test('an unedited form PUTs no service rows, an edit PUTs only its row', async () => {
      await loadPristine();
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
      window.alert = jest.fn();

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);
      expect(api.updateServiceConfig).not.toHaveBeenCalled();

      (document.getElementById('aws-savings-plans-sagemaker-term') as HTMLSelectElement).value = '1';
      await saveGlobalSettings(event);
      expect(api.updateServiceConfig).toHaveBeenCalledTimes(1);
    });

    // Issue #301: configurable recommendations cache-staleness threshold + lookback
    test('sends recommendations_cache_stale_hours and recommendations_lookback_days', async () => {
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (document.getElementById('setting-recs-stale-hours') as HTMLInputElement).value = '48';
      (document.getElementById('setting-recs-lookback-days') as HTMLSelectElement).value = '30';

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      const call = (api.updateConfig as jest.Mock).mock.calls[0][0];
      expect(call.recommendations_cache_stale_hours).toBe(48);
      expect(call.recommendations_lookback_days).toBe(30);
    });

    test('rejects out-of-range recommendations_cache_stale_hours and does not call updateConfig', async () => {
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (document.getElementById('setting-recs-stale-hours') as HTMLInputElement).value = '9999';

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      expect(api.updateConfig).not.toHaveBeenCalled();
      expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'error' }));
    });

    test('rejects fractional recommendations_cache_stale_hours and does not call updateConfig', async () => {
      // Pin the Number.isInteger guard so a future refactor can't silently
      // truncate fractional input via parseInt and accept it as valid.
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (document.getElementById('setting-recs-stale-hours') as HTMLInputElement).value = '1.5';

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      expect(api.updateConfig).not.toHaveBeenCalled();
      expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'error' }));
    });

    test('accepts 0 for recommendations_cache_stale_hours (disable sentinel)', async () => {
      // Preserve the documented "0 = disable automatic background refresh"
      // semantic. The validator must NOT reject 0; updateConfig must be
      // called with the literal 0 so the persisted GlobalConfig disables
      // the stale-while-revalidate background refresh.
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (document.getElementById('setting-recs-stale-hours') as HTMLInputElement).value = '0';

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await saveGlobalSettings(event);

      expect(api.updateConfig).toHaveBeenCalled();
      const call = (api.updateConfig as jest.Mock).mock.calls[0][0];
      expect(call.recommendations_cache_stale_hours).toBe(0);
    });
  }); // end saveGlobalSettings

  describe('resetSettings', () => {
    beforeEach(() => {
      // Set non-default values
      (document.getElementById('provider-aws') as HTMLInputElement).checked = false;
      (document.getElementById('provider-azure') as HTMLInputElement).checked = true;
      (document.getElementById('provider-gcp') as HTMLInputElement).checked = true;
      (document.getElementById('setting-notification-email') as HTMLInputElement).value = 'custom@example.com';
      (document.getElementById('setting-auto-collect') as HTMLInputElement).checked = false;
      (document.getElementById('setting-default-term') as HTMLSelectElement).value = '1';
      (document.getElementById('setting-default-payment') as HTMLSelectElement).value = 'no-upfront';
      (document.getElementById('setting-default-coverage') as HTMLInputElement).value = '50';
      (document.getElementById('setting-notification-days') as HTMLInputElement).value = '7';
    });

    test('does nothing if user cancels confirmation', async () => {
      mockConfirmDialog.mockResolvedValueOnce(false);

      await resetSettings();

      // Values should not change
      expect((document.getElementById('provider-azure') as HTMLInputElement).checked).toBe(true);
    });

    test('resets all fields to defaults on confirmation', async () => {
      mockConfirmDialog.mockResolvedValueOnce(true);

      await resetSettings();

      expect((document.getElementById('provider-aws') as HTMLInputElement).checked).toBe(true);
      expect((document.getElementById('provider-azure') as HTMLInputElement).checked).toBe(false);
      expect((document.getElementById('provider-gcp') as HTMLInputElement).checked).toBe(false);
      expect((document.getElementById('setting-notification-email') as HTMLInputElement).value).toBe('');
      expect((document.getElementById('setting-auto-collect') as HTMLInputElement).checked).toBe(true);
      expect((document.getElementById('setting-default-term') as HTMLSelectElement).value).toBe('3');
      expect((document.getElementById('setting-default-payment') as HTMLSelectElement).value).toBe('all-upfront');
      expect((document.getElementById('setting-default-coverage') as HTMLInputElement).value).toBe('80');
      expect((document.getElementById('setting-notification-days') as HTMLInputElement).value).toBe('3');
    });

    // Issue #301: recommendations cycle params reset to defaults
    test('resets recommendations_cache_stale_hours to 24 and lookback to 7', async () => {
      mockConfirmDialog.mockResolvedValueOnce(true);
      (document.getElementById('setting-recs-stale-hours') as HTMLInputElement).value = '48';
      (document.getElementById('setting-recs-lookback-days') as HTMLSelectElement).value = '60';

      await resetSettings();

      expect((document.getElementById('setting-recs-stale-hours') as HTMLInputElement).value).toBe('24');
      expect((document.getElementById('setting-recs-lookback-days') as HTMLSelectElement).value).toBe('7');
    });

    // Issue #466: Reset to Defaults must expand the Collection Schedule
    // section (auto-collect re-flips ON, so the dependent controls need to
    // be visible) AND mark the settings form as dirty so the user sees
    // the "Unsaved changes" affordance and remembers to click Save.
    test('expands the Collection Schedule row when re-enabling Auto-collect (#466)', async () => {
      mockConfirmDialog.mockResolvedValueOnce(true);
      // Pre-reset state: auto-collect off, schedule row hidden.
      (document.getElementById('setting-auto-collect') as HTMLInputElement).checked = false;
      const scheduleRow = document.getElementById('collection-schedule-row');
      scheduleRow?.classList.add('hidden');

      await resetSettings();

      expect((document.getElementById('setting-auto-collect') as HTMLInputElement).checked).toBe(true);
      expect(scheduleRow?.classList.contains('hidden')).toBe(false);
    });

    test('surfaces "Unsaved changes" indicator after reset (#466)', async () => {
      mockConfirmDialog.mockResolvedValueOnce(true);
      // Clear any prior dirty flag from a previous test.
      const tabBtn = document.getElementById('admin-tab-btn');
      tabBtn?.classList.remove('has-unsaved');
      document.querySelectorAll('.settings-buttons').forEach(el => el.classList.remove('dirty'));

      await resetSettings();

      // reflectDirtyState toggles .has-unsaved on #admin-tab-btn and .dirty
      // on .settings-buttons whenever any tracked field differs from the
      // saved snapshot. After Reset, every field has been overwritten
      // relative to the (still-empty) module-level snapshot, so the
      // indicator must be active.
      expect(tabBtn?.classList.contains('has-unsaved')).toBe(true);
      const saveBar = document.querySelector('.settings-buttons') as HTMLElement | null;
      expect(saveBar?.classList.contains('dirty')).toBe(true);
    });
  });

  describe('copyToClipboard', () => {
    let originalClipboard: Clipboard;

    beforeEach(() => {
      originalClipboard = navigator.clipboard;
    });

    afterEach(() => {
      Object.defineProperty(navigator, 'clipboard', {
        value: originalClipboard,
        writable: true
      });
    });

    test('copies text from element to clipboard', async () => {
      const writeTextMock = jest.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, 'clipboard', {
        value: { writeText: writeTextMock },
        writable: true
      });

      copyToClipboard('test-copy-element');

      await new Promise(resolve => setTimeout(resolve, 0));

      expect(writeTextMock).toHaveBeenCalledWith('test-value');
    });

    test('shows copied feedback on button', async () => {
      jest.useFakeTimers();

      const writeTextMock = jest.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, 'clipboard', {
        value: { writeText: writeTextMock },
        writable: true
      });

      copyToClipboard('test-copy-element');

      // Flush microtasks for the promise to resolve
      await Promise.resolve();
      jest.runAllTicks();

      const btn = document.querySelector('.copy-btn') as HTMLButtonElement;
      // The innerHTML will contain the actual checkmark character, not the HTML entity
      expect(btn.classList.contains('copied')).toBe(true);

      // Fast-forward timer
      jest.advanceTimersByTime(2000);

      expect(btn.innerHTML).toContain('Copy');
      expect(btn.classList.contains('copied')).toBe(false);

      jest.useRealTimers();
    });

    test('handles non-existent element gracefully', () => {
      const writeTextMock = jest.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, 'clipboard', {
        value: { writeText: writeTextMock },
        writable: true
      });

      // Should not throw
      expect(() => copyToClipboard('non-existent-element')).not.toThrow();
      expect(writeTextMock).not.toHaveBeenCalled();
    });

    test('handles clipboard error gracefully', async () => {
      jest.useFakeTimers();

      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      const writeTextMock = jest.fn().mockRejectedValue(new Error('Clipboard error'));
      Object.defineProperty(navigator, 'clipboard', {
        value: { writeText: writeTextMock },
        writable: true
      });

      copyToClipboard('test-copy-element');

      // Flush all promises
      await Promise.resolve();
      jest.runAllTicks();
      await Promise.resolve();

      expect(consoleError).toHaveBeenCalledWith('Failed to copy:', expect.any(Error));
      consoleError.mockRestore();
      jest.useRealTimers();
    });
  });

  describe('Azure payment defaults (issue #12)', () => {
    // Shared fixture covers term selects only; inject the four Azure payment
    // selects this issue introduces so loadGlobalSettings / saveGlobalSettings
    // have DOM nodes to read / write.
    const injectAzurePaymentSelects = () => {
      const form = document.getElementById('global-settings-form');
      if (!form) throw new Error('global-settings-form fixture missing');
      for (const svc of ['vm', 'sql', 'cosmosdb', 'redis']) {
        if (document.getElementById(`azure-${svc}-payment`)) continue;
        const select = document.createElement('select');
        select.id = `azure-${svc}-payment`;
        for (const [value, label] of [['upfront', 'Upfront'], ['monthly', 'Monthly']] as const) {
          const opt = document.createElement('option');
          opt.value = value;
          opt.textContent = label;
          if (value === 'upfront') opt.selected = true;
          select.appendChild(opt);
        }
        form.appendChild(select);
      }
    };

    test('loadGlobalSettings applies persisted azure-vm payment', async () => {
      injectAzurePaymentSelects();
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['aws', 'azure'],
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
        },
        services: [
          { provider: 'azure', service: 'vm', term: 3, payment: 'monthly', enabled: true, coverage: 80 },
        ],
      });

      await loadGlobalSettings();

      const vmPayment = document.getElementById('azure-vm-payment') as HTMLSelectElement;
      expect(vmPayment.value).toBe('monthly');
    });

    test('saveGlobalSettings sends the per-service Azure payment, not the global default', async () => {
      injectAzurePaymentSelects();
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: { enabled_providers: ['aws', 'azure'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80 },
        services: [],
      });
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);

      await loadGlobalSettings();
      // User flips Azure VM to Monthly while global default stays all-upfront.
      (document.getElementById('azure-vm-payment') as HTMLSelectElement).value = 'monthly';
      (document.getElementById('setting-default-payment') as HTMLSelectElement).value = 'all-upfront';

      await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);

      const azureVmCall = (api.updateServiceConfig as jest.Mock).mock.calls.find(
        ([provider, service]) => provider === 'azure' && service === 'compute',
      );
      expect(azureVmCall).toBeDefined();
      const cfg = azureVmCall![2];
      expect(cfg.payment).toBe('monthly');
    });

    test('help text describes the correct per-provider payment semantics', () => {
      // The help copy lives in index.html (loaded into the test fixture
      // verbatim at test startup). Guard against regressions in the
      // wording — the old copy falsely claimed Azure was always upfront.
      const fs = require('fs') as typeof import('fs');
      const path = require('path') as typeof import('path');
      const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf-8');
      expect(html).toMatch(/Azure reservations support upfront or monthly/);
      expect(html).not.toMatch(/Azure and GCP reservations are always paid upfront/);
    });
  });

  // Guard against the RDS 3yr + no-upfront regression from follow-up to
  // issue #12. The backend rejects that combination (and EC/OpenSearch/
  // Redshift 3yr no-upfront), so the Settings form must not allow it.
  // Rules live in commitmentOptions.ts; these tests exercise the wiring
  // that applies them to the per-service dropdowns.
  describe('per-service term/payment combination constraints', () => {
    const optVisible = (sel: HTMLSelectElement, value: string): boolean => {
      const opt = Array.from(sel.options).find(o => o.value === value);
      if (!opt) return false;
      return !opt.hidden && !opt.disabled;
    };

    test('RDS 3yr hides "no-upfront" and keeps partial/all upfront selectable', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: { enabled_providers: ['aws'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80 },
        services: [{ provider: 'aws', service: 'rds', term: 3, payment: 'all-upfront' }],
      });
      setupSettingsHandlers();
      await loadGlobalSettings();

      const rdsPayment = document.getElementById('aws-rds-payment') as HTMLSelectElement;
      expect(optVisible(rdsPayment, 'no-upfront')).toBe(false);
      expect(optVisible(rdsPayment, 'partial-upfront')).toBe(true);
      expect(optVisible(rdsPayment, 'all-upfront')).toBe(true);
    });

    test('RDS 1yr keeps all three payment options visible', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: { enabled_providers: ['aws'], default_term: 1, default_payment: 'no-upfront', default_coverage: 80 },
        services: [{ provider: 'aws', service: 'rds', term: 1, payment: 'no-upfront' }],
      });
      setupSettingsHandlers();
      await loadGlobalSettings();

      const rdsPayment = document.getElementById('aws-rds-payment') as HTMLSelectElement;
      expect(optVisible(rdsPayment, 'no-upfront')).toBe(true);
      expect(optVisible(rdsPayment, 'partial-upfront')).toBe(true);
      expect(optVisible(rdsPayment, 'all-upfront')).toBe(true);
    });

    test('switching RDS term 1yr → 3yr while "no-upfront" is selected auto-clamps payment', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: { enabled_providers: ['aws'], default_term: 1, default_payment: 'no-upfront', default_coverage: 80 },
        services: [{ provider: 'aws', service: 'rds', term: 1, payment: 'no-upfront' }],
      });
      setupSettingsHandlers();
      await loadGlobalSettings();

      const rdsTerm = document.getElementById('aws-rds-term') as HTMLSelectElement;
      const rdsPayment = document.getElementById('aws-rds-payment') as HTMLSelectElement;
      expect(rdsPayment.value).toBe('no-upfront');

      rdsTerm.value = '3';
      rdsTerm.dispatchEvent(new Event('change'));

      // no-upfront is now invalid; payment should snap to first valid option
      expect(rdsPayment.value).not.toBe('no-upfront');
      expect(['partial-upfront', 'all-upfront']).toContain(rdsPayment.value);
      expect(optVisible(rdsPayment, 'no-upfront')).toBe(false);
    });

    test('legacy-persisted invalid combo (RDS 3yr + no-upfront) is clamped on load', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: { enabled_providers: ['aws'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80 },
        // Simulate a config stored before this guardrail existed.
        services: [{ provider: 'aws', service: 'rds', term: 3, payment: 'no-upfront' }],
      });
      setupSettingsHandlers();
      await loadGlobalSettings();

      const rdsPayment = document.getElementById('aws-rds-payment') as HTMLSelectElement;
      expect(rdsPayment.value).not.toBe('no-upfront');
    });

    test('EC2 3yr keeps all three payment options visible (no service-level restriction)', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: { enabled_providers: ['aws'], default_term: 3, default_payment: 'no-upfront', default_coverage: 80 },
        services: [{ provider: 'aws', service: 'ec2', term: 3, payment: 'no-upfront' }],
      });
      setupSettingsHandlers();
      await loadGlobalSettings();

      const ec2Payment = document.getElementById('aws-ec2-payment') as HTMLSelectElement;
      expect(optVisible(ec2Payment, 'no-upfront')).toBe(true);
      expect(optVisible(ec2Payment, 'partial-upfront')).toBe(true);
      expect(optVisible(ec2Payment, 'all-upfront')).toBe(true);
      expect(ec2Payment.value).toBe('no-upfront');
    });

    test.each(['elasticache', 'opensearch', 'redshift'])(
      '%s 3yr keeps "no-upfront" visible (AWS only restricts RDS)',
      async (service) => {
        (api.getConfig as jest.Mock).mockResolvedValue({
          global: { enabled_providers: ['aws'], default_term: 3, default_payment: 'no-upfront', default_coverage: 80 },
          services: [{ provider: 'aws', service, term: 3, payment: 'no-upfront' }],
        });
        setupSettingsHandlers();
        await loadGlobalSettings();

        const payment = document.getElementById(`aws-${service}-payment`) as HTMLSelectElement;
        expect(optVisible(payment, 'no-upfront')).toBe(true);
        // And the selected value round-trips cleanly — the backend persists
        // this service with no-upfront, and the UI should not clamp it.
        expect(payment.value).toBe('no-upfront');
      },
    );

    // Empty-state regression: on a fresh install where no per-product
    // service_configs rows exist (e.g., right after migration 000040 ran
    // but before any user save), loadGlobalSettings must not blow up,
    // and the four SP card selects must remain present + interactable.
    // This protects against a regression where the load path tried to
    // touch a missing service entry and threw, leaving the cards in a
    // broken state. The selects keep their HTML-default values (3 /
    // all-upfront) per the existing per-service load semantics — the
    // wider "apply globalCfg.default_term / default_payment to empty
    // selects" cascade is OUT OF SCOPE for this PR (see plan §6.5).
    test('per-plan-type SP cards remain interactable when no service rows exist (issue #22 follow-up)', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: { enabled_providers: ['aws'], default_term: 1, default_payment: 'no-upfront', default_coverage: 80 },
        services: [], // no per-service overrides
      });
      setupSettingsHandlers();
      await loadGlobalSettings();

      for (const planType of ['compute', 'ec2instance', 'sagemaker', 'database']) {
        const term = document.getElementById(`aws-savings-plans-${planType}-term`) as HTMLSelectElement;
        const payment = document.getElementById(`aws-savings-plans-${planType}-payment`) as HTMLSelectElement;
        expect(term).not.toBeNull();
        expect(payment).not.toBeNull();
        // Cards keep the HTML-default 3yr/all-upfront baseline (matching
        // the existing umbrella card's pre-rev-2 behaviour).
        expect(term.value).toBe('3');
        expect(payment.value).toBe('all-upfront');
        // All 6 (term × payment) combos remain selectable on the new
        // SP keys via the _default fallback in commitmentOptions.
        expect(optVisible(payment, 'no-upfront')).toBe(true);
        expect(optVisible(payment, 'partial-upfront')).toBe(true);
        expect(optVisible(payment, 'all-upfront')).toBe(true);
      }
    });

    test('SageMaker 3yr keeps "no-upfront" visible (issue #22 — no service-level restriction)', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: { enabled_providers: ['aws'], default_term: 3, default_payment: 'no-upfront', default_coverage: 80 },
        services: [{ provider: 'aws', service: 'savings-plans-sagemaker', term: 3, payment: 'no-upfront' }],
      });
      setupSettingsHandlers();
      await loadGlobalSettings();

      const payment = document.getElementById('aws-savings-plans-sagemaker-payment') as HTMLSelectElement;
      expect(optVisible(payment, 'no-upfront')).toBe(true);
      expect(optVisible(payment, 'partial-upfront')).toBe(true);
      expect(optVisible(payment, 'all-upfront')).toBe(true);
      expect(payment.value).toBe('no-upfront');
    });

    test('propagating global "no-upfront" to all services while term=3 clamps restricted services', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: { enabled_providers: ['aws'], default_term: 3, default_payment: 'all-upfront', default_coverage: 80 },
        services: [
          { provider: 'aws', service: 'ec2', term: 3, payment: 'all-upfront' },
          { provider: 'aws', service: 'rds', term: 3, payment: 'all-upfront' },
        ],
      });
      setupSettingsHandlers();
      await loadGlobalSettings();

      // User changes the global default to no-upfront and confirms the propagation.
      mockConfirmDialog.mockResolvedValue(true);
      const defaultPayment = document.getElementById('setting-default-payment') as HTMLSelectElement;
      defaultPayment.dataset['previous'] = 'all-upfront';
      defaultPayment.value = 'no-upfront';
      defaultPayment.dispatchEvent(new Event('change'));

      // Allow the async confirmDialog promise to resolve.
      await Promise.resolve();
      await Promise.resolve();

      const ec2Payment = document.getElementById('aws-ec2-payment') as HTMLSelectElement;
      const rdsPayment = document.getElementById('aws-rds-payment') as HTMLSelectElement;
      // EC2 accepts the propagated no-upfront (no restriction).
      expect(ec2Payment.value).toBe('no-upfront');
      // RDS 3yr rejects no-upfront, so it clamps back to the first valid option.
      expect(rdsPayment.value).not.toBe('no-upfront');
    });
  });

  // -----------------------------------------------------------------------
  // Inline range validation — top-level numeric settings (issue #1411)
  // -----------------------------------------------------------------------
  // These inputs have min/max in the test DOM (added alongside the fix) so
  // wireInlineRangeValidation creates the error element and wires the
  // 'input'/'blur' events. Tests confirm the regression is gone: inline
  // error appears immediately as the user types, not only after Save.
  describe('inline range validation on top-level numeric settings (#1411)', () => {
    function fire(el: HTMLInputElement, type: 'input' | 'blur'): void {
      el.dispatchEvent(new Event(type));
    }

    beforeEach(() => {
      setupSettingsHandlers();
    });

    it.each([
      ['setting-notification-days', '0',     'Must be a whole number between 1 and 30'],
      ['setting-notification-days', '31',    'Must be a whole number between 1 and 30'],
      ['setting-recs-stale-hours',  '-1',    'Must be a whole number between 0 and 8760'],
      ['setting-default-coverage',  '101',   'Must be a whole number between 0 and 100'],
    ])('typing %s=%s shows inline error with message %s', (id, val, msg) => {
      const input = document.getElementById(id) as HTMLInputElement;
      input.value = val;
      fire(input, 'input');

      expect(input.getAttribute('aria-invalid')).toBe('true');
      const errEl = document.getElementById(`${id}-range-error`);
      expect(errEl).not.toBeNull();
      expect(errEl!.classList.contains('hidden')).toBe(false);
      expect(errEl!.textContent).toBe(msg);
    });

    it('clearing the default coverage field shows the inline error instead of passing as 0 (#522)', () => {
      const input = document.getElementById('setting-default-coverage') as HTMLInputElement;
      input.value = '';
      fire(input, 'input');

      expect(input.getAttribute('aria-invalid')).toBe('true');
      const errEl = document.getElementById('setting-default-coverage-range-error');
      expect(errEl!.classList.contains('hidden')).toBe(false);
      expect(errEl!.textContent).toBe('Must be a whole number between 0 and 100');
    });

    it('clearing an optional numeric field stays without an inline error', () => {
      const input = document.getElementById('setting-notification-days') as HTMLInputElement;
      input.value = '';
      fire(input, 'input');

      expect(input.getAttribute('aria-invalid')).toBeNull();
    });

    it('typing a fractional value into notification-days is rejected inline', () => {
      const input = document.getElementById('setting-notification-days') as HTMLInputElement;
      input.value = '1.5';
      fire(input, 'input');

      expect(input.getAttribute('aria-invalid')).toBe('true');
      const errEl = document.getElementById('setting-notification-days-range-error');
      expect(errEl!.classList.contains('hidden')).toBe(false);
    });

    it('restoring a valid value clears the inline error', () => {
      const input = document.getElementById('setting-notification-days') as HTMLInputElement;
      input.value = '0';
      fire(input, 'input');
      expect(input.getAttribute('aria-invalid')).toBe('true');

      input.value = '5';
      fire(input, 'input');
      expect(input.getAttribute('aria-invalid')).toBeNull();
      const errEl = document.getElementById('setting-notification-days-range-error');
      expect(errEl!.classList.contains('hidden')).toBe(true);
    });
  });

  // -----------------------------------------------------------------------
  // Inline range validation — SP coverage inputs (issue #1411 row 7.13)
  // -----------------------------------------------------------------------
  describe('inline range validation wired for SP coverage inputs (#1411 row 7.13)', () => {
    function fire(el: HTMLInputElement, type: 'input' | 'blur'): void {
      el.dispatchEvent(new Event(type));
    }

    beforeEach(() => {
      setupSettingsHandlers();
    });

    it.each([
      'aws-savings-plans-compute-coverage',
      'aws-savings-plans-ec2instance-coverage',
      'aws-savings-plans-sagemaker-coverage',
      'aws-savings-plans-database-coverage',
    ])('typing 101 into %s shows inline error', (id) => {
      const input = document.getElementById(id) as HTMLInputElement;
      input.value = '101';
      fire(input, 'input');

      expect(input.getAttribute('aria-invalid')).toBe('true');
      const errEl = document.getElementById(`${id}-range-error`);
      expect(errEl).not.toBeNull();
      expect(errEl!.classList.contains('hidden')).toBe(false);
      expect(errEl!.textContent).toBe('Must be a whole number between 0 and 100');
    });

    it('valid SP coverage value clears any prior error', () => {
      const id = 'aws-savings-plans-compute-coverage';
      const input = document.getElementById(id) as HTMLInputElement;
      input.value = '200';
      fire(input, 'input');
      expect(input.getAttribute('aria-invalid')).toBe('true');

      input.value = '80';
      fire(input, 'input');
      expect(input.getAttribute('aria-invalid')).toBeNull();
      const errEl = document.getElementById(`${id}-range-error`);
      expect(errEl!.classList.contains('hidden')).toBe(true);
    });
  });

  // -----------------------------------------------------------------------
  // Cancel on Global Defaults clears dirty state (issue #1412 rows 7.4/7.9)
  // -----------------------------------------------------------------------
  // These tests use loadGlobalSettings() to establish a proper dirty-tracking
  // snapshot (identical to the real app flow where settings are loaded before
  // the user interacts). Without the snapshot every field looks dirty regardless
  // of its value, so the test cannot distinguish "cancel cleared the marker"
  // from "marker was never set".
  describe('cancel on Global Defaults propagation clears dirty state (#1412 7.4/7.9)', () => {
    // Minimal getConfig response that satisfies loadGlobalSettings without errors.
    const baseConfigResponse = {
      global: {
        enabled_providers: ['aws'] as string[],
        notification_email: '',
        auto_collect: true,
        default_term: 3,
        default_payment: 'all-upfront',
        default_coverage: 80,
        notification_days_before: 3,
        recommendations_cache_stale_hours: 24,
        recommendations_lookback_days: 7,
        grace_period_days: { aws: 7, azure: 7, gcp: 7 },
        laddering_enabled: false,
      },
      services: [],
      credentials: {},
    };

    test('cancelling term cascade clears has-unsaved and dirty markers', async () => {
      // loadGlobalSettings populates the form AND calls snapshotAllFields() so
      // dirty tracking has a clean baseline (term=3 in the snapshot).
      (api.getConfig as jest.Mock).mockResolvedValue(baseConfigResponse);
      await loadGlobalSettings();
      setupSettingsHandlers();
      mockConfirmDialog.mockResolvedValueOnce(false);

      // Change term to 1 (differs from snapshot=3) -> confirm appears -> cancel.
      const defaultTerm = document.getElementById('setting-default-term') as HTMLSelectElement;
      defaultTerm.value = '1';
      defaultTerm.dispatchEvent(new Event('change'));
      await new Promise((r) => setTimeout(r, 0));

      // Cancel must restore the previous value.
      expect(defaultTerm.value).toBe('3');

      // With snapshot=3 and restored=3 the diff is clean, so markers must clear.
      const tabBtn = document.getElementById('admin-tab-btn');
      const saveBar = document.querySelector('.settings-buttons');
      expect(tabBtn?.classList.contains('has-unsaved')).toBe(false);
      expect(saveBar?.classList.contains('dirty')).toBe(false);
    });

    test('cancelling payment cascade clears has-unsaved and dirty markers', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue(baseConfigResponse);
      await loadGlobalSettings();
      setupSettingsHandlers();
      mockConfirmDialog.mockResolvedValueOnce(false);

      const defaultPayment = document.getElementById('setting-default-payment') as HTMLSelectElement;
      defaultPayment.value = 'no-upfront';
      defaultPayment.dispatchEvent(new Event('change'));
      await new Promise((r) => setTimeout(r, 0));

      expect(defaultPayment.value).toBe('all-upfront');

      const tabBtn = document.getElementById('admin-tab-btn');
      const saveBar = document.querySelector('.settings-buttons');
      expect(tabBtn?.classList.contains('has-unsaved')).toBe(false);
      expect(saveBar?.classList.contains('dirty')).toBe(false);
    });
  });

  // -----------------------------------------------------------------------
  // Global Defaults popup uses display names (issue #1412 rows 7.3/7.8)
  // -----------------------------------------------------------------------
  describe('Global Defaults popup shows display names not backend IDs (#1412 7.3/7.8)', () => {
    test('confirmAndPropagateTerm passes display names to confirmDialog', async () => {
      setupSettingsHandlers();
      mockConfirmDialog.mockResolvedValueOnce(false);

      const defaultTerm = document.getElementById('setting-default-term') as HTMLSelectElement;
      defaultTerm.dataset['previous'] = '3';
      // Set one service to a different value so the diff is non-empty.
      (document.getElementById('aws-ec2-term') as HTMLSelectElement).value = '3';
      defaultTerm.value = '1';
      defaultTerm.dispatchEvent(new Event('change'));
      await new Promise((r) => setTimeout(r, 0));
      await new Promise((r) => setTimeout(r, 0));

      expect(mockConfirmDialog).toHaveBeenCalledTimes(1);
      const callArg = mockConfirmDialog.mock.calls[0]![0] as { body: Node };
      // body is the HTMLDivElement from buildAffectedList; serialize to check text.
      const bodyText = (callArg.body as HTMLElement).textContent ?? '';
      // Must include display name, not raw backend ID.
      expect(bodyText).toMatch(/EC2 Reserved Instances/);
      expect(bodyText).not.toMatch(/^aws$/m);
    });
  });

  // -----------------------------------------------------------------------
  // isUnsavedChanges -- no false prompt on pristine load or before load
  // (issue #1441)
  // -----------------------------------------------------------------------
  // The navigation guard in navigation.ts calls isUnsavedChanges() whenever
  // the user leaves the Admin tab. Before the fix, savedSnapshot started as
  // an empty object ({}), so comparing any live DOM value against undefined
  // always returned true -- causing the "You have unsaved settings changes.
  // Leave without saving?" modal to fire even without any edits.
  describe('isUnsavedChanges -- navigation guard (issue #1441)', () => {
    const baseConfigResponse = {
      global: {
        enabled_providers: ['aws'] as string[],
        notification_email: '',
        auto_collect: true,
        default_term: 3,
        default_payment: 'all-upfront',
        default_coverage: 80,
        notification_days_before: 3,
        recommendations_cache_stale_hours: 24,
        recommendations_lookback_days: 7,
        grace_period_days: { aws: 7, azure: 7, gcp: 7 },
        laddering_enabled: false,
      },
      services: [],
      credentials: {},
    };

    test('returns false before settings have been loaded (empty snapshot -- regression for #1441)', () => {
      // Use a fresh module instance so savedSnapshot starts as the empty object
      // {} that it is at module initialisation, before any loadGlobalSettings()
      // call has run snapshotAllFields(). This is the exact state that caused
      // the bug: the navigation guard fired on every first Admin-tab visit
      // because `getFieldValue(id) !== undefined` was always true.
      //
      // Pre-fix: TRACKED_FIELDS.some(id => getFieldValue(id) !== savedSnapshot[id])
      //   = TRACKED_FIELDS.some(id => '' !== undefined) = true -> prompt shown (BUG)
      // Post-fix: Object.keys(savedSnapshot).length === 0 -> return false (CORRECT)
      let freshIsUnsavedChanges!: () => boolean;
      jest.isolateModules(() => {
        // eslint-disable-next-line @typescript-eslint/no-require-imports
        const s = require('../settings') as { isUnsavedChanges: () => boolean };
        freshIsUnsavedChanges = s.isUnsavedChanges;
      });
      expect(freshIsUnsavedChanges()).toBe(false);
    });

    test('returns false immediately after settings load without any user edits', async () => {
      // loadGlobalSettings() populates all tracked fields from the API response
      // and then calls snapshotAllFields() to establish the clean baseline.
      // A user who navigates away right after load (no edits) must see no prompt.
      (api.getConfig as jest.Mock).mockResolvedValue(baseConfigResponse);
      await loadGlobalSettings();

      expect(isUnsavedChanges()).toBe(false);
    });

    test('returns true after a tracked field is edited post-load', async () => {
      // After a clean load + snapshot, mutating a tracked field must be detected
      // so the navigation guard can show the prompt for real unsaved changes.
      // Verifies the fix does not suppress legitimate dirty-state detection.
      (api.getConfig as jest.Mock).mockResolvedValue(baseConfigResponse);
      await loadGlobalSettings();

      // Simulate the user changing the notification email -- a tracked field.
      const emailInput = document.getElementById('setting-notification-email') as HTMLInputElement;
      emailInput.value = 'edited@example.com';
      emailInput.dispatchEvent(new Event('input'));

      expect(isUnsavedChanges()).toBe(true);
    });
  });

  // Issue #135: `default_coverage || 80` rewrote a stored 0 to 80, and the
  // next Save persisted 80 without the user touching the field.
  describe('default_coverage 0 round-trips through load and save (issue #135)', () => {
    const configWithCoverage = (global: Record<string, unknown>, services: unknown[] = []) => ({
      global: {
        enabled_providers: ['aws'],
        notification_email: '',
        auto_collect: true,
        default_term: 3,
        default_payment: 'all-upfront',
        notification_days_before: 3,
        ...global,
      },
      services,
      credentials: {},
    });

    const saveUnchanged = async () => {
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
      await saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);
    };

    test('a stored 0 renders as 0 and an unedited Save sends no global PUT', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue(configWithCoverage({ default_coverage: 0 }));
      await loadGlobalSettings();

      expect((document.getElementById('setting-default-coverage') as HTMLInputElement).value).toBe('0');
      expect((document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value).toBe('0');

      await saveUnchanged();

      expect(api.updateConfig).not.toHaveBeenCalled();
      expect(api.updateServiceConfig).not.toHaveBeenCalled();
    });

    test('an absent default_coverage renders the 80 default and an unedited Save sends no global PUT', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue(configWithCoverage({}));
      await loadGlobalSettings();

      expect((document.getElementById('setting-default-coverage') as HTMLInputElement).value).toBe('80');

      await saveUnchanged();

      expect(api.updateConfig).not.toHaveBeenCalled();
    });

    test('a per-service coverage of 0 round-trips under a non-zero global', async () => {
      (api.getConfig as jest.Mock).mockResolvedValue(configWithCoverage(
        { default_coverage: 50 },
        [{ provider: 'aws', service: 'savings-plans-compute', enabled: true, term: 3, payment: 'all-upfront', coverage: 0 }],
      ));
      await loadGlobalSettings();

      expect((document.getElementById('aws-savings-plans-compute-coverage') as HTMLInputElement).value).toBe('0');
      expect((document.getElementById('aws-savings-plans-ec2instance-coverage') as HTMLInputElement).value).toBe('50');

      await saveUnchanged();

      expect(api.updateServiceConfig).not.toHaveBeenCalled();
    });

    test('the override modal inherit label shows the stored 0%', async () => {
      const modal = document.createElement('div');
      modal.id = 'override-modal';
      const covInput = document.createElement('input');
      covInput.id = 'override-coverage';
      modal.appendChild(covInput);
      document.body.appendChild(modal);

      (api.getConfig as jest.Mock).mockResolvedValue(configWithCoverage({ default_coverage: 0 }));
      await loadGlobalSettings();
      openOverrideModal('acc-1', 'aws', [], document.createElement('div'));

      expect(covInput.placeholder).toBe('Inherit (currently: 0%)');
    });
  });

  describe('global PUT against the real index.html (issue #539)', () => {
    const loadRealPage = async (): Promise<void> => {
      // eslint-disable-next-line @typescript-eslint/no-require-imports
      const fs = require('fs') as typeof import('fs');
      // eslint-disable-next-line @typescript-eslint/no-require-imports
      const path = require('path') as typeof import('path');
      const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
      document.body.innerHTML = html
        .replace(/^[\s\S]*<body[^>]*>/i, '')
        .replace(/<\/body>[\s\S]*$/i, '')
        .replace(/<script[\s\S]*?<\/script>/gi, '');
      (api.getConfig as jest.Mock).mockResolvedValue({
        global: {
          enabled_providers: ['aws'],
          notification_email: 'old@test.com',
          auto_collect: true,
          collection_schedule: 'daily',
          default_term: 3,
          default_payment: 'all-upfront',
          default_coverage: 80,
          notification_days_before: 3,
          grace_period_days: { aws: 7, azure: 7, gcp: 7 },
          recommendations_cache_stale_hours: 24,
          recommendations_lookback_days: 7,
          offering_class: 'convertible',
          require_different_approver: false,
          laddering_enabled: true,
        },
        services: [],
      });
      (api.updateConfig as jest.Mock).mockClear().mockResolvedValue({});
      (api.updateServiceConfig as jest.Mock).mockClear().mockResolvedValue(undefined);
      await loadGlobalSettings();
      await new Promise(resolve => setTimeout(resolve, 0));
    };
    const save = (): Promise<void> => saveGlobalSettings({ preventDefault: jest.fn() } as unknown as Event);
    const globalBody = (): unknown => (api.updateConfig as jest.Mock).mock.calls[0]?.[0];

    test('the laddering toggle is rendered and an unedited Save sends no global PUT', async () => {
      await loadRealPage();
      expect(document.getElementById('setting-laddering-enabled')).not.toBeNull();

      await save();

      expect(api.updateConfig).not.toHaveBeenCalled();
    });

    test('editing only the notification email sends only the email', async () => {
      await loadRealPage();
      (document.getElementById('setting-notification-email') as HTMLInputElement).value = 'new@test.com';

      await save();

      expect(globalBody()).toEqual({ notification_email: 'new@test.com' });
    });

    test('editing one grace input saves grace_period_days', async () => {
      await loadRealPage();
      (document.getElementById('setting-grace-azure') as HTMLInputElement).value = '9';

      await save();

      expect(globalBody()).toEqual({ grace_period_days: { aws: 7, azure: 9, gcp: 7 } });
    });

    test('editing only a provider toggle saves enabled_providers', async () => {
      await loadRealPage();
      (document.getElementById('provider-gcp') as HTMLInputElement).checked = true;

      await save();

      expect(globalBody()).toEqual({ enabled_providers: ['aws', 'gcp'] });
    });

    test('editing only the collection schedule saves it', async () => {
      await loadRealPage();
      (document.getElementById('setting-collection-schedule') as HTMLSelectElement).value = 'weekly';

      await save();

      expect(globalBody()).toEqual({ collection_schedule: 'weekly' });
    });

    test('editing only the approver toggle saves it', async () => {
      await loadRealPage();
      (document.getElementById('setting-require-different-approver') as HTMLInputElement).checked = true;

      await save();

      expect(globalBody()).toEqual({ require_different_approver: true });
    });

    test('editing only the offering class saves it', async () => {
      await loadRealPage();
      (document.getElementById('setting-ec2-offering-class') as HTMLSelectElement).value = 'standard';

      await save();

      expect(globalBody()).toEqual({ offering_class: 'standard' });
    });

    test('a global field persisted before a filter validation failure is not treated as unsaved', async () => {
      await loadRealPage();
      const minCount = document.getElementById('aws-ec2-min-count') as HTMLInputElement;
      expect(minCount).not.toBeNull();
      (document.getElementById('setting-notification-email') as HTMLInputElement).value = 'new@test.com';
      minCount.value = '-1';

      await save();
      expect(api.updateConfig).toHaveBeenCalledTimes(1);
      expect(api.updateConfig).toHaveBeenLastCalledWith({ notification_email: 'new@test.com' });

      // The user undoes the edit and fixes the filter. The stored email is
      // new@test.com, so reverting the field must be sent.
      (document.getElementById('setting-notification-email') as HTMLInputElement).value = 'old@test.com';
      minCount.value = '0';
      await save();

      expect(api.updateConfig).toHaveBeenCalledTimes(2);
      expect(api.updateConfig).toHaveBeenLastCalledWith({ notification_email: 'old@test.com' });
    });
  });
});
