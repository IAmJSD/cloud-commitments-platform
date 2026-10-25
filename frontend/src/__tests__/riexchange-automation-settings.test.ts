/**
 * loadAutomationSettings permission-gating tests (issue #1413).
 *
 * GET /api/ri-exchange/config requires view:config. Before migration 000088
 * granted view:config to non-admin groups, the handler returned 403 and
 * loadAutomationSettings showed an error paragraph ("Failed to load settings:
 * permission denied: requires view on config") at the bottom of the Purchasing
 * Policies panel. The page should degrade gracefully instead: clear the
 * container and return without showing any error.
 *
 * After the migration non-admin users have view:config, so the 403 no longer
 * occurs in normal operation. The graceful-degradation path remains as defense
 * in depth for custom roles that do not include view:config.
 */

jest.mock('../api', () => ({
  getRIExchangeConfig: jest.fn(),
}));

jest.mock('../settings', () => ({
  applyReadOnlySettings: jest.fn(),
  isPermissionDeniedError: jest.fn(),
}));

jest.mock('../state', () => ({
  getCurrentUser: jest.fn(),
}));

import * as api from '../api';
import * as settings from '../settings';
import { loadAutomationSettings } from '../riexchange';

function makePermissionError(status: number, message: string): Error & { status: number } {
  const err = new Error(message) as Error & { status: number };
  err.status = status;
  return err;
}

const setupContainer = () => {
  document.body.replaceChildren();
  const container = document.createElement('div');
  container.id = 'ri-exchange-automation-settings';
  // Simulate the loading paragraph loadAutomationSettings prepends.
  const loading = document.createElement('p');
  loading.className = 'loading';
  loading.textContent = 'Loading settings...';
  container.appendChild(loading);
  document.body.appendChild(container);
  return container;
};

describe('loadAutomationSettings: graceful degradation on permission errors (issue #1413)', () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  test(
    '403 permission-denied: container is cleared, no error paragraph shown',
    async () => {
      setupContainer();
      const permErr = makePermissionError(403, 'permission denied: requires view on config');
      (api.getRIExchangeConfig as jest.Mock).mockRejectedValueOnce(permErr);
      // isPermissionDeniedError is imported from settings and must return true
      // for the graceful-degradation branch to fire.
      (settings.isPermissionDeniedError as jest.Mock).mockReturnValue(true);

      await loadAutomationSettings();

      const container = document.getElementById('ri-exchange-automation-settings');
      // No error paragraph — the section should be empty (silently cleared).
      const errorP = container?.querySelector('.error');
      expect(errorP).toBeNull();
      // The container itself must be present in the DOM (not removed).
      expect(container).not.toBeNull();
      // No child nodes: both the loading paragraph and any error content cleared.
      expect(container?.childNodes.length).toBe(0);
    },
  );

  test(
    '403 permission-denied: no retry button is shown',
    async () => {
      setupContainer();
      const permErr = makePermissionError(403, 'permission denied: requires view on config');
      (api.getRIExchangeConfig as jest.Mock).mockRejectedValueOnce(permErr);
      (settings.isPermissionDeniedError as jest.Mock).mockReturnValue(true);

      await loadAutomationSettings();

      const container = document.getElementById('ri-exchange-automation-settings');
      const retryBtn = container?.querySelector('button');
      expect(retryBtn).toBeNull();
    },
  );

  test(
    'non-permission 403 (e.g. IP block): error paragraph IS shown',
    async () => {
      setupContainer();
      const nonPermErr = makePermissionError(403, 'access blocked by IP policy');
      (api.getRIExchangeConfig as jest.Mock).mockRejectedValueOnce(nonPermErr);
      // isPermissionDeniedError returns false for non-permission 403s.
      (settings.isPermissionDeniedError as jest.Mock).mockReturnValue(false);

      await loadAutomationSettings();

      const container = document.getElementById('ri-exchange-automation-settings');
      const errorP = container?.querySelector('.error');
      expect(errorP).not.toBeNull();
      expect(errorP?.textContent).toContain('access blocked by IP policy');
    },
  );

  test(
    'network error: error paragraph IS shown with message',
    async () => {
      setupContainer();
      (api.getRIExchangeConfig as jest.Mock).mockRejectedValueOnce(
        new Error('Failed to fetch'),
      );
      (settings.isPermissionDeniedError as jest.Mock).mockReturnValue(false);

      await loadAutomationSettings();

      const container = document.getElementById('ri-exchange-automation-settings');
      const errorP = container?.querySelector('.error');
      expect(errorP).not.toBeNull();
      expect(errorP?.textContent).toContain('Failed to fetch');
    },
  );

  test(
    'network error: retry button IS shown',
    async () => {
      setupContainer();
      (api.getRIExchangeConfig as jest.Mock).mockRejectedValueOnce(
        new Error('Failed to fetch'),
      );
      (settings.isPermissionDeniedError as jest.Mock).mockReturnValue(false);

      await loadAutomationSettings();

      const container = document.getElementById('ri-exchange-automation-settings');
      const retryBtn = container?.querySelector('button');
      expect(retryBtn).not.toBeNull();
      expect(retryBtn?.textContent).toBe('Retry');
    },
  );
});

describe('loadAutomationSettings: loading and error stay inside the section frame (issue #190)', () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  test('while loading, a titled Exchange Automation fieldset holds the place', async () => {
    setupContainer();
    let resolve!: (v: unknown) => void;
    (api.getRIExchangeConfig as jest.Mock).mockReturnValueOnce(new Promise((r) => { resolve = r; }));

    const pending = loadAutomationSettings();

    const container = document.getElementById('ri-exchange-automation-settings')!;
    const fieldset = container.querySelector('fieldset.settings-category');
    expect(container.children).toHaveLength(1);
    expect(fieldset?.querySelector('legend')?.textContent).toBe('Exchange Automation');
    expect(fieldset?.getAttribute('aria-busy')).toBe('true');
    // Issue #186: the shared skeleton, not a visible loading paragraph.
    expect(fieldset?.querySelector('.skeleton-group')).not.toBeNull();
    expect(fieldset?.querySelector('.skeleton-status')?.textContent).toBe('Loading…');
    expect(fieldset?.querySelector('p.loading')).toBeNull();
    expect(container.textContent).not.toContain('Loading settings...');

    resolve(Promise.reject(new Error('Failed to fetch')));
    (settings.isPermissionDeniedError as jest.Mock).mockReturnValue(false);
    await pending;
  });

  test('an error is shown inside the same titled frame', async () => {
    setupContainer();
    (api.getRIExchangeConfig as jest.Mock).mockRejectedValueOnce(new Error('Failed to fetch'));
    (settings.isPermissionDeniedError as jest.Mock).mockReturnValue(false);

    await loadAutomationSettings();

    const fieldset = document.querySelector('#ri-exchange-automation-settings > fieldset.settings-category');
    expect(fieldset?.querySelector('legend')?.textContent).toBe('Exchange Automation');
    expect(fieldset?.querySelector('.error')?.textContent).toContain('Failed to fetch');
    expect(fieldset?.querySelector('button')?.getAttribute('type')).toBe('button');
  });
});

describe('loadAutomationSettings: number fields are escaped into value attributes (issue #37)', () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  // RIExchangeConfig's number types are erased at runtime, so a string
  // that reaches a value="..." attribute must not be able to close it.
  test('a non-number config field cannot break out of its input value', async () => {
    setupContainer();
    const hostile = '5" autofocus onfocus="alert(1)';
    (api.getRIExchangeConfig as jest.Mock).mockResolvedValueOnce({
      auto_exchange_enabled: false,
      mode: 'manual',
      utilization_threshold: hostile,
      max_payment_per_exchange_usd: hostile,
      max_payment_daily_usd: hostile,
      lookback_days: hostile,
    });

    await loadAutomationSettings();

    const container = document.getElementById('ri-exchange-automation-settings') as HTMLElement;
    expect(container.querySelector('[onfocus]')).toBeNull();
    for (const id of ['ri-exchange-threshold', 'ri-exchange-max-per-exchange', 'ri-exchange-max-daily', 'ri-exchange-lookback']) {
      const input = container.querySelector<HTMLInputElement>('#' + id);
      expect(input).not.toBeNull();
      expect(input?.getAttribute('value')).toBe(hostile);
    }
  });

  test('numeric config fields still prefill the inputs', async () => {
    setupContainer();
    (api.getRIExchangeConfig as jest.Mock).mockResolvedValueOnce({
      auto_exchange_enabled: true,
      mode: 'manual',
      utilization_threshold: 95.5,
      max_payment_per_exchange_usd: 0,
      max_payment_daily_usd: 1000,
      lookback_days: 30,
    });

    await loadAutomationSettings();

    const value = (id: string) => (document.getElementById(id) as HTMLInputElement).value;
    expect(value('ri-exchange-threshold')).toBe('95.5');
    expect(value('ri-exchange-max-per-exchange')).toBe('0');
    expect(value('ri-exchange-max-daily')).toBe('1000');
    expect(value('ri-exchange-lookback')).toBe('30');
  });
});
