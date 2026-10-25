/**
 * Inline range errors re-run after programmatic value sets (issue #642).
 *
 * wireInlineRangeValidation listens to 'input'/'blur', which `.value = x`
 * never fires, so an error from an earlier edit used to stay visible (with
 * aria-invalid) after Reset settings or a reload put a valid value back.
 * Uses the real index.html so the fields carry their production min/max.
 */
import * as fs from 'fs';
import * as path from 'path';
import { loadGlobalSettings, resetSettings, setupSettingsHandlers } from '../settings';

jest.mock('../api', () => ({
  getConfig: jest.fn(),
  updateConfig: jest.fn(),
  updateServiceConfig: jest.fn(),
  getLadderConfigs: jest.fn().mockResolvedValue([]),
}));
jest.mock('../federation', () => ({
  initFederationPanel: jest.fn().mockResolvedValue(undefined),
}));
jest.mock('../confirmDialog', () => ({
  confirmDialog: jest.fn(() => Promise.resolve(true)),
}));
jest.mock('../toast', () => ({
  showToast: jest.fn(() => ({ dismiss: jest.fn() })),
}));

import * as api from '../api';

const COVERAGE = 'setting-default-coverage';
const NOTIFY = 'setting-notification-days';

function input(id: string): HTMLInputElement {
  return document.getElementById(id) as HTMLInputElement;
}

function typeValue(id: string, value: string): void {
  input(id).value = value;
  input(id).dispatchEvent(new Event('input'));
}

function errorShown(id: string): boolean {
  const err = document.getElementById(`${id}-range-error`);
  return input(id).getAttribute('aria-invalid') === 'true' && !!err && !err.classList.contains('hidden');
}

describe('inline range errors after programmatic value sets (#642)', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf-8');
    document.body.innerHTML = new DOMParser().parseFromString(html, 'text/html').body.innerHTML;
    (api.getConfig as jest.Mock).mockResolvedValue({
      global: { enabled_providers: ['aws'], default_term: 3, default_payment: 'all-upfront', default_coverage: 70, notification_days_before: 5 },
      services: [],
    });
    setupSettingsHandlers();
  });

  it('Reset settings clears an error left by an emptied coverage field', async () => {
    typeValue(COVERAGE, '');
    expect(errorShown(COVERAGE)).toBe(true);

    await resetSettings();

    expect(input(COVERAGE).value).toBe('80');
    expect(errorShown(COVERAGE)).toBe(false);
    expect(input(COVERAGE).hasAttribute('aria-invalid')).toBe(false);
  });

  it('Reset settings clears an out-of-range error on notification days', async () => {
    typeValue(NOTIFY, '99');
    expect(errorShown(NOTIFY)).toBe(true);

    await resetSettings();

    expect(input(NOTIFY).value).toBe('3');
    expect(errorShown(NOTIFY)).toBe(false);
  });

  it('reloading settings clears an error once the persisted value is filled back in', async () => {
    typeValue(COVERAGE, '');
    typeValue(NOTIFY, '0');
    expect(errorShown(COVERAGE) && errorShown(NOTIFY)).toBe(true);

    await loadGlobalSettings();

    expect(input(COVERAGE).value).toBe('70');
    expect(input(NOTIFY).value).toBe('5');
    expect(errorShown(COVERAGE)).toBe(false);
    expect(errorShown(NOTIFY)).toBe(false);
  });

  it('a field Reset does not touch keeps its error', async () => {
    const sp = document.querySelector<HTMLInputElement>('input[id$="-coverage"][data-range-validated]:not(#setting-default-coverage)');
    expect(sp).not.toBeNull();
    typeValue(sp!.id, '150');

    await resetSettings();

    expect(errorShown(sp!.id)).toBe(true);
  });

  it('0 is a valid default coverage (lower bound, not treated as empty)', () => {
    typeValue(COVERAGE, '0');
    expect(errorShown(COVERAGE)).toBe(false);
    expect(input(COVERAGE).hasAttribute('aria-invalid')).toBe(false);
  });
});
