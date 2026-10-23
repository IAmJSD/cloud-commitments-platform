/**
 * Commitment Laddering settings module tests (issue #1333 phase 3).
 *
 * Covers the two security/correctness-sensitive surfaces:
 *  (a) renderConfigTable escapes attacker-controlled cloud_account_id/provider
 *      so an injected tag never lands raw in innerHTML (XSS).
 *  (b) saveLadderConfig's max_hourly_commit_per_run guard: blank -> null,
 *      positive -> passthrough, non-positive/invalid -> rejected (no store call).
 */
import { initLadderingSettings, renderConfigTable, saveLadderConfig } from '../ladder';
import type { LadderConfig } from '../api';
import { waitFor } from '@testing-library/dom';
import { listAccountsMinimal, type AccountSummary } from '../api/accounts';
import { canAccess } from '../permissions';

jest.mock('../api/accounts', () => ({ listAccountsMinimal: jest.fn() }));
jest.mock('../ladder-timeline', () => ({ initLadderTimeline: jest.fn() }));

jest.mock('../api', () => ({
  getLadderConfigs: jest.fn(),
  upsertLadderConfig: jest.fn(),
  getConfig: jest.fn(),
  updateConfig: jest.fn(),
}));

jest.mock('../permissions', () => ({
  canAccess: jest.fn(() => true),
}));

const mockShowToast = jest.fn();
jest.mock('../toast', () => ({
  showToast: (opts: unknown) => mockShowToast(opts),
}));

import * as api from '../api';

function baseConfig(overrides: Partial<LadderConfig> = {}): LadderConfig {
  return {
    cloud_account_id: '11111111-1111-1111-1111-111111111111',
    provider: 'aws',
    enabled: true,
    mode: 'email_approval',
    cadence: 'daily',
    target_coverage: 100,
    buffer_fraction: 0.1,
    baseline_percentile: 5,
    lookback_days: 30,
    buffer_utilization_threshold: 90,
    max_hourly_commit_per_run: null,
    max_actions_per_run: 10,
    ramp_schedule: { steps: [{ after_days: 0, fraction: 1.0 }] },
    updated_at: '2026-01-15T10:00:00Z',
    ...overrides,
  } as LadderConfig;
}

// Render the full section (incl. the modal form) into the DOM so the exported
// helpers have the elements they read.
async function renderSection(configs: LadderConfig[] = []): Promise<void> {
  document.body.innerHTML = '<div id="commitment-laddering-settings"></div>';
  (api.getLadderConfigs as jest.Mock).mockResolvedValue(configs);
  await initLadderingSettings(false);
}

describe('ladder.ts', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    mockShowToast.mockReset();
    (canAccess as jest.Mock).mockReturnValue(true);
    (listAccountsMinimal as jest.Mock).mockResolvedValue([
      { id: 'acct-1', name: 'AWS account', external_id: 'aws-external', provider: 'aws' },
      { id: 'azure-id', name: 'Azure account', external_id: 'azure-external', provider: 'azure' },
      { id: 'gcp-id', name: 'GCP account', external_id: 'gcp-external', provider: 'gcp' },
    ]);
  });

  test('config modal moves focus inside, traps Tab, closes on Escape and restores focus (#731)', async () => {
    await renderSection([baseConfig({ id: 'config-1' })]);
    const editBtn = document.querySelector<HTMLButtonElement>('.ladder-edit-btn')!;
    editBtn.focus();
    editBtn.click();

    const modal = document.getElementById('ladder-config-modal')!;
    expect(modal).not.toHaveClass('hidden');
    expect(modal).toHaveAttribute('role', 'dialog');
    expect(modal.contains(document.activeElement)).toBe(true);

    // Shift+Tab from the first focusable wraps to the last one inside the modal.
    modal.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', shiftKey: true, bubbles: true }));
    expect(modal.contains(document.activeElement)).toBe(true);

    modal.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    expect(modal).toHaveClass('hidden');
    expect(document.activeElement).toBe(editBtn);
  });

  test('lookback input advertises the 10-day baseline minimum', async () => {
    await renderSection([baseConfig({ id: 'config-1' })]);
    expect(document.getElementById('ladder-cfg-lookback-days')).toHaveAttribute('min', '10');
  });

  test.each([
    ['aws', 'email_approval', 'daily'],
    ['azure', 'auto_approve', 'weekly'],
    ['gcp', 'auto_approve', 'weekly'],
  ] as const)('editing %s preserves the selected values in the submitted config', async (provider, mode, cadence) => {
    const config = baseConfig({ id: 'config-1', provider, mode, cadence, target_coverage: 80 });
    await renderSection([config]);
    (api.upsertLadderConfig as jest.Mock).mockImplementation(async (cfg: LadderConfig) => cfg);

    document.querySelector<HTMLButtonElement>('.ladder-edit-btn')!.click();
    expect(document.getElementById('ladder-cfg-provider')).toHaveValue(provider);
    expect(document.getElementById('ladder-cfg-mode')).toHaveValue(mode);
    expect(document.getElementById('ladder-cfg-cadence')).toHaveValue(cadence);
    expect(document.getElementById('ladder-cfg-provider')).toBeDisabled();
    expect(document.getElementById('ladder-cfg-account')).toBeDisabled();

    (document.getElementById('ladder-cfg-target-coverage') as HTMLInputElement).value = '75';
    document.getElementById('ladder-config-form')!.dispatchEvent(new Event('submit', { cancelable: true }));
    await waitFor(() => expect(document.getElementById('ladder-config-modal')).toHaveClass('hidden'));
    expect(api.upsertLadderConfig).toHaveBeenCalledTimes(1);
    const { updated_at: _updatedAt, ...expected } = config;
    expect(api.upsertLadderConfig).toHaveBeenCalledWith({ ...expected, target_coverage: 75 });
  });

  describe('renderLadderingSection: toggle label accessibility (issue #1412 row 3.1)', () => {
    // Regression guard: the Enable Commitment Laddering label text must NOT be
    // a <label for="setting-laddering-enabled"> because that makes the entire
    // text string clickable (toggling the checkbox on click). Pattern matches
    // the Auto-collect row fix from issue #464: use a <span> + aria-labelledby
    // instead.
    test('enable-laddering label is a span, not a <label for> element', async () => {
      await renderSection();
      // A <label for="setting-laddering-enabled"> would make the text clickable.
      const badLabel = document.querySelector('label[for="setting-laddering-enabled"]');
      expect(badLabel).toBeNull();
    });

    test('enable-laddering input carries aria-labelledby referencing the span', async () => {
      await renderSection();
      const input = document.getElementById('setting-laddering-enabled') as HTMLInputElement | null;
      expect(input).not.toBeNull();
      const labelledById = input!.getAttribute('aria-labelledby');
      expect(labelledById).toBe('setting-laddering-enabled-label');
      // The referenced element must exist and contain the label text.
      const labelSpan = document.getElementById('setting-laddering-enabled-label');
      expect(labelSpan).not.toBeNull();
      expect(labelSpan!.textContent).toMatch(/Enable Commitment Laddering/);
    });

    test('enable-laddering checkbox is wrapped in a .toggle-label element', async () => {
      await renderSection();
      const input = document.getElementById('setting-laddering-enabled') as HTMLInputElement | null;
      expect(input).not.toBeNull();
      // The input must be inside a .toggle-label so the toggle switch is the
      // only click target (not the surrounding descriptive text).
      expect(input!.closest('.toggle-label')).not.toBeNull();
    });
  });

  describe('kill-switch toggle', () => {
    const flip = (): HTMLInputElement => {
      const toggle = document.getElementById('setting-laddering-enabled') as HTMLInputElement;
      toggle.checked = !toggle.checked;
      toggle.dispatchEvent(new Event('change'));
      return toggle;
    };

    test.each([true, false])('sends exactly the new laddering_enabled value and nothing else (was %s)', async (before) => {
      await renderSection();
      (api.updateConfig as jest.Mock).mockResolvedValue({});
      const toggle = document.getElementById('setting-laddering-enabled') as HTMLInputElement;
      toggle.checked = before;

      flip();
      await waitFor(() => expect(api.updateConfig).toHaveBeenCalledTimes(1));

      expect(api.updateConfig).toHaveBeenCalledWith({ laddering_enabled: !before });
      expect(toggle.checked).toBe(!before);
      await waitFor(() => expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'success' })));
    });

    test('a failed PUT reverts the toggle and shows an error toast', async () => {
      await renderSection();
      (api.updateConfig as jest.Mock).mockRejectedValue(new Error('boom'));
      jest.spyOn(console, 'error').mockImplementation(() => undefined);
      const toggle = document.getElementById('setting-laddering-enabled') as HTMLInputElement;
      const before = toggle.checked;

      flip();

      await waitFor(() => expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'error' })));
      expect(api.updateConfig).toHaveBeenCalledWith({ laddering_enabled: !before });
      expect(toggle.checked).toBe(before);
    });
  });

  describe('renderConfigTable XSS escaping', () => {
    test('renders a malicious cloud_account_id/provider as inert text, not markup', async () => {
      await renderSection();
      const evil = '<img src=x onerror=alert(1)>';
      renderConfigTable([baseConfig({ cloud_account_id: evil, provider: evil })]);

      const container = document.getElementById('ladder-configs-table-container')!;

      // The injected payload must not create a live element anywhere in the
      // table (DOM-level assertion; reading serialized innerHTML is unreliable
      // because jsdom re-serializes attribute values with raw </> which are
      // inert inside a quoted attribute).
      expect(container.querySelector('img')).toBeNull();

      // The account-id cell holds the payload as TEXT (no child elements =>
      // it was escaped, not parsed as HTML).
      const firstCell = container.querySelector('tbody tr td');
      expect(firstCell?.textContent).toBe(evil);
      expect(firstCell?.children.length).toBe(0);

      // Exactly one edit button exists (no extra nodes injected) and it carries
      // the value as an inert data-* attribute rather than as markup.
      const btns = container.querySelectorAll<HTMLButtonElement>('button.ladder-edit-btn');
      expect(btns.length).toBe(1);
      expect(btns[0]!.dataset['provider']).toBe(evil);
    });
  });

  describe('saveLadderConfig max_hourly guard', () => {
    // Fill the modal form with a valid baseline so save reaches the store,
    // then override max-hourly per case.
    async function primeValidForm(maxHourly: string): Promise<void> {
      await renderSection();
      document.getElementById('ladder-add-config-btn')!.click();
      await waitFor(() => expect(document.getElementById('ladder-cfg-account')).toBeEnabled());
      (document.getElementById('ladder-cfg-account') as HTMLSelectElement).value = 'acct-1';
      (document.getElementById('ladder-cfg-ramp-schedule') as HTMLTextAreaElement).value =
        '{"steps":[{"after_days":0,"fraction":1.0}]}';
      (document.getElementById('ladder-cfg-max-hourly') as HTMLInputElement).value = maxHourly;
    }

    test('blank max-hourly serializes to null in the payload', async () => {
      await primeValidForm('');
      (api.upsertLadderConfig as jest.Mock).mockResolvedValue(baseConfig());

      await saveLadderConfig();

      expect(api.upsertLadderConfig).toHaveBeenCalledTimes(1);
      const sent = (api.upsertLadderConfig as jest.Mock).mock.calls[0][0] as LadderConfig;
      expect(sent.max_hourly_commit_per_run).toBeNull();
    });

    test('positive max-hourly passes through unchanged', async () => {
      await primeValidForm('5');
      (api.upsertLadderConfig as jest.Mock).mockResolvedValue(baseConfig());

      await saveLadderConfig();

      expect(api.upsertLadderConfig).toHaveBeenCalledTimes(1);
      const sent = (api.upsertLadderConfig as jest.Mock).mock.calls[0][0] as LadderConfig;
      expect(sent.max_hourly_commit_per_run).toBe(5);
    });

    test('non-positive max-hourly is rejected (guarded, no store call)', async () => {
      await primeValidForm('-1');
      await saveLadderConfig();

      expect(api.upsertLadderConfig).not.toHaveBeenCalled();
      expect(mockShowToast).toHaveBeenCalledWith(
        expect.objectContaining({ kind: 'error' }),
      );
    });
  });

  describe('registered account selector', () => {
    const account = (): HTMLSelectElement => document.getElementById('ladder-cfg-account') as HTMLSelectElement;
    const provider = (): HTMLSelectElement => document.getElementById('ladder-cfg-provider') as HTMLSelectElement;
    const status = (): HTMLElement => document.getElementById('ladder-cfg-account-status')!;
    const saveButton = (): HTMLElement => document.getElementById('ladder-config-save-btn')!;
    const changeProvider = (value: string): void => { provider().value = value; provider().dispatchEvent(new Event('change')); };
    async function open(): Promise<void> {
      await renderSection();
      document.getElementById('ladder-add-config-btn')!.click();
    }
    async function ready(): Promise<void> { await waitFor(() => expect(account()).toBeEnabled()); }

    test('offers only registered accounts for the chosen provider and submits their internal ID', async () => {
      await open();
      await ready();
      expect(account().tagName).toBe('SELECT');
      expect(account()).toHaveValue('');
      expect(Array.from(account().options).map(option => option.value)).toEqual(['', 'acct-1']);
      expect(account().options[1]!.textContent).toBe('AWS account (aws-external)');
      account().value = 'acct-1';
      changeProvider('azure');
      expect(account()).toHaveValue('');
      expect(Array.from(account().options).map(option => option.value)).toEqual(['', 'azure-id']);
      changeProvider('gcp');
      expect(Array.from(account().options).map(option => option.value)).toEqual(['', 'gcp-id']);
      account().value = 'gcp-id';
      (api.upsertLadderConfig as jest.Mock).mockImplementation(async config => config);
      await saveLadderConfig();
      expect(api.upsertLadderConfig).toHaveBeenCalledWith(expect.objectContaining({ cloud_account_id: 'gcp-id', provider: 'gcp' }));
      expect(listAccountsMinimal).toHaveBeenCalledWith();
      expect(api.updateConfig).not.toHaveBeenCalled();
    });

    test.each(['invented-id', 'azure-id'])('rejects an injected option %s for AWS', async id => {
      await open();
      await ready();
      if (account() instanceof HTMLSelectElement) account().add(new Option('Injected', id));
      account().value = id;
      (api.upsertLadderConfig as jest.Mock).mockImplementation(async config => config);
      await saveLadderConfig();
      expect(api.upsertLadderConfig).not.toHaveBeenCalled();
      expect(mockShowToast).toHaveBeenCalledWith(expect.objectContaining({ kind: 'error' }));
    });

    test('renders hostile names and external IDs as text', async () => {
      const evil = '<img src=x onerror=alert(1)>';
      (listAccountsMinimal as jest.Mock).mockResolvedValue([{ id: 'safe-id', name: evil, external_id: evil, provider: 'aws' }]);
      await open();
      await ready();
      expect(account().options[1]!.textContent).toBe(`${evil} (${evil})`);
      expect(document.querySelector('#ladder-config-form img')).toBeNull();
    });

    test('blocks save while loading and filters the provider selected before response', async () => {
      let resolve!: (accounts: AccountSummary[]) => void;
      (listAccountsMinimal as jest.Mock).mockReturnValue(new Promise<AccountSummary[]>(done => { resolve = done; }));
      await open();
      expect(account()).toBeDisabled();
      expect(saveButton()).toBeDisabled();
      expect(status()).toHaveTextContent(/loading/i);
      await saveLadderConfig();
      expect(api.upsertLadderConfig).not.toHaveBeenCalled();
      changeProvider('gcp');
      resolve([{ id: 'gcp-late', name: 'Late', external_id: 'external', provider: 'gcp' }]);
      await ready();
      expect(Array.from(account().options).map(option => option.value)).toEqual(['', 'gcp-late']);
    });

    test('a response from a closed modal cannot replace a reopened form', async () => {
      let resolve!: (accounts: AccountSummary[]) => void;
      (listAccountsMinimal as jest.Mock).mockReturnValueOnce(new Promise<AccountSummary[]>(done => { resolve = done; }));
      await open();
      document.getElementById('ladder-modal-cancel-btn')!.click();
      document.getElementById('ladder-add-config-btn')!.click();
      await ready();
      account().value = 'acct-1';
      resolve([{ id: 'stale-id', name: 'Stale', external_id: 'stale', provider: 'aws' }]);
      await Promise.resolve();
      expect(account()).toHaveValue('acct-1');
      expect(Array.from(account().options).map(option => option.value)).toEqual(['', 'acct-1']);
    });

    test.each(['empty', 'error'])('blocks creation when account list is %s', async state => {
      if (state === 'error') (listAccountsMinimal as jest.Mock).mockRejectedValue(new Error('Forbidden'));
      else (listAccountsMinimal as jest.Mock).mockResolvedValue([]);
      await open();
      await waitFor(() => expect(status()).toHaveTextContent(state === 'error' ? /could not load/i : /no registered/i));
      expect(account()).toBeDisabled();
      expect(saveButton()).toBeDisabled();
      account().add(new Option('Injected', 'invented-id'));
      account().value = 'invented-id';
      await saveLadderConfig();
      expect(api.upsertLadderConfig).not.toHaveBeenCalled();
    });

    test.each(['empty', 'error'])('keeps the captured edit key with a visible warning when list is %s', async state => {
      if (state === 'error') (listAccountsMinimal as jest.Mock).mockRejectedValue(new Error('Forbidden'));
      else (listAccountsMinimal as jest.Mock).mockResolvedValue([]);
      const config = baseConfig({ id: 'existing', cloud_account_id: 'saved-id', provider: 'azure' });
      await renderSection([config]);
      document.querySelector<HTMLButtonElement>('.ladder-edit-btn')!.click();
      await waitFor(() => expect(status()).toHaveTextContent(/identity is unchanged/i));
      expect(account()).toBeDisabled();
      expect(provider()).toBeDisabled();
      account().add(new Option('Injected', 'different-id'));
      account().value = 'different-id';
      provider().value = 'aws';
      (api.upsertLadderConfig as jest.Mock).mockImplementation(async saved => saved);
      await saveLadderConfig();
      expect(api.upsertLadderConfig).toHaveBeenCalledWith(expect.objectContaining({ cloud_account_id: 'saved-id', provider: 'azure' }));
    });

    test('permission loss prevents submitting a selected account', async () => {
      await open();
      await ready();
      account().value = 'acct-1';
      (canAccess as jest.Mock).mockReturnValue(false);
      await saveLadderConfig();
      expect(api.upsertLadderConfig).not.toHaveBeenCalled();
      await renderSection();
      expect(document.getElementById('ladder-add-config-btn')).toBeNull();
      expect(listAccountsMinimal).toHaveBeenCalledTimes(1);
    });
  });
});
