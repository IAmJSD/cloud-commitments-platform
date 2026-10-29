/**
 * Ramp schedule step editor in the ladder config modal: rows <-> JSON sync
 * both ways, live validation, add/remove/presets with focus, and the save
 * path refusing exactly what the backend would refuse.
 */
import { initLadderingSettings, saveLadderConfig } from '../ladder';
import type { LadderConfig } from '../api';
import { waitFor } from '@testing-library/dom';
import { listAccountsMinimal } from '../api/accounts';
import { canAccess } from '../permissions';

jest.mock('../api/accounts', () => ({ listAccountsMinimal: jest.fn() }));
jest.mock('../ladder-timeline', () => ({ initLadderTimeline: jest.fn(), refreshLadderTimeline: jest.fn() }));
jest.mock('../api', () => ({
  getLadderConfigs: jest.fn(),
  upsertLadderConfig: jest.fn(),
  getConfig: jest.fn(),
  updateConfig: jest.fn(),
}));
jest.mock('../confirmDialog', () => ({ confirmDialog: jest.fn(async () => true) }));
jest.mock('../permissions', () => ({ canAccess: jest.fn(() => true) }));
const mockShowToast = jest.fn();
jest.mock('../toast', () => ({ showToast: (opts: unknown) => mockShowToast(opts) }));

import * as api from '../api';

function config(ramp: unknown): LadderConfig {
  return {
    id: 'config-1', cloud_account_id: 'acct-1', provider: 'aws', enabled: false,
    mode: 'email_approval', cadence: 'daily', target_coverage: 100, buffer_fraction: 0.1,
    baseline_percentile: 5, lookback_days: 30, buffer_utilization_threshold: 90,
    max_hourly_commit_per_run: null, max_actions_per_run: 10,
    ramp_schedule: ramp as LadderConfig['ramp_schedule'],
  };
}

const $ = <T extends HTMLElement = HTMLElement>(id: string): T => document.getElementById(id) as T;
const textarea = (): HTMLTextAreaElement => $('ladder-cfg-ramp-schedule');
const rowEls = (): HTMLLIElement[] => Array.from(document.querySelectorAll<HTMLLIElement>('#ladder-ramp-steps li'));
const afterInput = (i: number): HTMLInputElement => $(`ladder-ramp-after-${i}`);
const shareInput = (i: number): HTMLInputElement => $(`ladder-ramp-share-${i}`);
const summary = (): string => $('ladder-ramp-summary').textContent ?? '';
const rowValues = (): Array<[string, string]> => rowEls().map((_, i) => [afterInput(i).value, shareInput(i).value]);
const jsonSteps = (): unknown => JSON.parse(textarea().value).steps;

function type(el: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

async function openEdit(ramp: unknown): Promise<void> {
  document.body.innerHTML = '<div id="commitment-laddering-settings"></div>';
  (api.getLadderConfigs as jest.Mock).mockResolvedValue([config(ramp)]);
  await initLadderingSettings(false);
  document.querySelector<HTMLButtonElement>('.ladder-edit-btn')!.click();
  await waitFor(() => expect($('ladder-cfg-account')).toHaveValue('acct-1'));
}

async function openNew(): Promise<void> {
  document.body.innerHTML = '<div id="commitment-laddering-settings"></div>';
  (api.getLadderConfigs as jest.Mock).mockResolvedValue([]);
  await initLadderingSettings(false);
  $('ladder-add-config-btn').click();
  await waitFor(() => expect($('ladder-cfg-account')).toBeEnabled());
  $<HTMLSelectElement>('ladder-cfg-account').value = 'acct-1';
}

const sentRamp = (): unknown => ((api.upsertLadderConfig as jest.Mock).mock.calls[0][0] as LadderConfig).ramp_schedule;

describe('ladder ramp schedule editor', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    (canAccess as jest.Mock).mockReturnValue(true);
    (listAccountsMinimal as jest.Mock).mockResolvedValue([{ id: 'acct-1', name: 'AWS account', external_id: 'aws-ext', provider: 'aws' }]);
    (api.upsertLadderConfig as jest.Mock).mockImplementation(async (cfg: LadderConfig) => cfg);
  });

  test('Edit draws one row per stored step, with a summary and bar; the JSON stays hidden', async () => {
    await openEdit({ steps: [{ after_days: 0, fraction: 0.5 }, { after_days: 7, fraction: 0.25 }, { after_days: 60, fraction: 0.25 }] });
    expect(rowValues()).toEqual([['0', '50'], ['7', '25'], ['60', '25']]);
    expect(summary()).toBe('Total 100% across 3 steps · last purchase on day 60');
    expect(document.querySelectorAll('#ladder-ramp-bar .ramp-bar-seg')).toHaveLength(3);
    expect($('ladder-ramp-json')).not.toBeVisible();
    expect($('ladder-ramp-json-toggle')).toHaveAttribute('aria-expanded', 'false');
    expect(afterInput(1)).toHaveAccessibleName('Step 2 after (days)');
    expect(shareInput(1)).toHaveAccessibleName('Step 2 share (%)');
    expect(rowEls()[1]!.querySelector('.ramp-remove')).toHaveAccessibleName('Remove step 2');
  });

  test('a new config starts from the default single step, which cannot be removed', async () => {
    await openNew();
    expect(rowValues()).toEqual([['0', '100']]);
    expect(summary()).toBe('Total 100% in 1 step · purchase on day 0');
    expect(rowEls()[0]!.querySelector('.ramp-remove')).toBeDisabled();
    expect(JSON.parse(textarea().value)).toEqual({ steps: [{ after_days: 0, fraction: 1 }] });
  });

  test('Edit as JSON toggles the textarea', async () => {
    await openNew();
    const toggle = $('ladder-ramp-json-toggle');
    toggle.click();
    expect($('ladder-ramp-json')).toBeVisible();
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(toggle).toHaveTextContent('Hide JSON');
    toggle.click();
    expect($('ladder-ramp-json')).not.toBeVisible();
    expect(toggle).toHaveTextContent('Edit as JSON');
  });

  test('editing a row rewrites the JSON; untouched shares keep their exact fraction', async () => {
    await openEdit({ steps: [{ after_days: 0, fraction: 1 / 3 }, { after_days: 7, fraction: 1 / 3 }, { after_days: 14, fraction: 1 / 3 }] });
    expect(shareInput(0).value).toBe('33.3333333333');
    type(afterInput(2), '30');
    expect(jsonSteps()).toEqual([
      { after_days: 0, fraction: 1 / 3 }, { after_days: 7, fraction: 1 / 3 }, { after_days: 30, fraction: 1 / 3 },
    ]);
    expect(summary()).toBe('Total 100% across 3 steps · last purchase on day 30');
  });

  test('a share typed as a percent is stored as the matching fraction', async () => {
    await openEdit({ steps: [{ after_days: 0, fraction: 0.5 }, { after_days: 7, fraction: 0.5 }] });
    type(shareInput(0), '66.7');
    type(shareInput(1), '33.3');
    expect(jsonSteps()).toEqual([{ after_days: 0, fraction: 0.667 }, { after_days: 7, fraction: 0.333 }]);
    expect(summary()).toBe('Total 100% across 2 steps · last purchase on day 7');
  });

  test('other keys of the stored schedule survive a row edit', async () => {
    await openEdit({ note: 'keep', steps: [{ after_days: 0, fraction: 1 }] });
    type(afterInput(0), '2');
    expect(JSON.parse(textarea().value)).toEqual({ note: 'keep', steps: [{ after_days: 2, fraction: 1 }] });
  });

  test('row problems show beside the row, linked to the input, and in the summary', async () => {
    await openEdit({ steps: [{ after_days: 0, fraction: 0.5 }, { after_days: 7, fraction: 0.5 }] });
    type(afterInput(1), '0');
    const error = $('ladder-ramp-error-1');
    expect(error).toBeVisible();
    expect(error).toHaveTextContent('Must be later than step 1 (day 0).');
    expect(afterInput(1)).toHaveAttribute('aria-invalid', 'true');
    expect(afterInput(1)).toHaveAccessibleDescription('Must be later than step 1 (day 0).');
    expect(shareInput(1)).not.toHaveAttribute('aria-invalid');
    expect(summary()).toBe('Total 100% across 2 steps · 1 step needs fixing');
    expect($('ladder-ramp-summary')).toHaveClass('is-invalid');

    type(shareInput(1), '40');
    type(afterInput(1), '7');
    expect(error).not.toBeVisible();
    expect(afterInput(1)).not.toHaveAttribute('aria-invalid');
    expect(summary()).toBe('Total 90% across 2 steps · shares must add up to 100%');
    expect($('ladder-ramp-bar')).toHaveClass('is-invalid');

    type(shareInput(1), '150');
    expect($('ladder-ramp-error-1')).toHaveTextContent('Share can be at most 100%.');
    type(shareInput(1), '0');
    expect($('ladder-ramp-error-1')).toHaveTextContent('Share must be more than 0%.');
    type(afterInput(0), '-3');
    expect($('ladder-ramp-error-0')).toHaveTextContent('Days must be 0 or more.');
  });

  test('a blank row value keeps the last good JSON and blocks the save', async () => {
    await openNew();
    const before = textarea().value;
    type(shareInput(0), '');
    expect(textarea().value).toBe(before);
    expect($('ladder-ramp-error-0')).toHaveTextContent('Enter a share.');
    await saveLadderConfig();
    expect(api.upsertLadderConfig).not.toHaveBeenCalled();
    expect(mockShowToast).toHaveBeenCalledWith({ message: 'Check the ramp schedule. Step 1: enter a share.', kind: 'error' });
    expect(document.activeElement).toBe(shareInput(0));
  });

  test('valid JSON typed in the textarea redraws the rows', async () => {
    await openNew();
    $('ladder-ramp-json-toggle').click();
    type(textarea(), '{"steps":[{"after_days":0,"fraction":0.25},{"after_days":14,"fraction":0.75}]}');
    expect(rowValues()).toEqual([['0', '25'], ['14', '75']]);
    expect(summary()).toBe('Total 100% across 2 steps · last purchase on day 14');
  });

  test('invalid JSON keeps the rows and shows the error under the textarea until fixed', async () => {
    await openNew();
    $('ladder-ramp-json-toggle').click();
    type(textarea(), '{"steps": [');
    expect(rowValues()).toEqual([['0', '100']]);
    const error = $('ladder-cfg-ramp-schedule-error');
    expect(error).toBeVisible();
    expect(error.textContent).toMatch(/^Not valid JSON: /);
    expect(textarea()).toHaveAttribute('aria-invalid', 'true');
    expect(textarea().getAttribute('aria-describedby')).toContain('ladder-cfg-ramp-schedule-error');

    type(textarea(), '{"steps":[{"after_days":"7","fraction":1}]}');
    expect(error).toHaveTextContent('Step 1: after_days must be a whole number.');
    expect(rowValues()).toEqual([['0', '100']]);

    type(textarea(), '{"steps":[{"after_days":3,"fraction":1}]}');
    expect(error).not.toBeVisible();
    expect(textarea()).not.toHaveAttribute('aria-invalid');
    expect(rowValues()).toEqual([['3', '100']]);
  });

  test('Add step continues a week after the last step with the remaining share and focuses it', async () => {
    await openEdit({ steps: [{ after_days: 0, fraction: 0.5 }, { after_days: 10, fraction: 0.2 }] });
    $('ladder-ramp-add').click();
    expect(rowValues()).toEqual([['0', '50'], ['10', '20'], ['17', '30']]);
    expect(document.activeElement).toBe(afterInput(2));
    expect(jsonSteps()).toEqual([
      { after_days: 0, fraction: 0.5 }, { after_days: 10, fraction: 0.2 }, { after_days: 17, fraction: 0.3 },
    ]);
    expect(summary()).toBe('Total 100% across 3 steps · last purchase on day 17');
    // When the shares already total 100% the new step splits the last step's share.
    $('ladder-ramp-add').click();
    expect(rowValues().slice(2)).toEqual([['17', '15'], ['24', '15']]);
    expect(summary()).toBe('Total 100% across 4 steps · last purchase on day 24');
    expect((jsonSteps() as Array<{ fraction: number }>).map(s => s.fraction)).toEqual([0.5, 0.2, 0.15, 0.15]);
  });

  test('removing a step renumbers the rows and moves focus to the row that took its place', async () => {
    await openEdit({ steps: [{ after_days: 0, fraction: 0.5 }, { after_days: 7, fraction: 0.25 }, { after_days: 14, fraction: 0.25 }] });
    rowEls()[1]!.querySelector<HTMLButtonElement>('.ramp-remove')!.click();
    expect(rowValues()).toEqual([['0', '50'], ['14', '25']]);
    expect(afterInput(1)).toHaveAccessibleName('Step 2 after (days)');
    expect(document.activeElement).toBe(rowEls()[1]!.querySelector('.ramp-remove'));
    expect(jsonSteps()).toEqual([{ after_days: 0, fraction: 0.5 }, { after_days: 14, fraction: 0.25 }]);
    // Removing the last row moves focus up; the sole remaining row's input takes it.
    rowEls()[1]!.querySelector<HTMLButtonElement>('.ramp-remove')!.click();
    expect(document.activeElement).toBe(afterInput(0));
    expect(rowEls()[0]!.querySelector('.ramp-remove')).toBeDisabled();
  });

  test('presets replace the steps', async () => {
    await openNew();
    document.querySelector<HTMLButtonElement>('[data-preset="weekly-4"]')!.click();
    expect(rowValues()).toEqual([['0', '25'], ['7', '25'], ['14', '25'], ['21', '25']]);
    document.querySelector<HTMLButtonElement>('[data-preset="monthly-2"]')!.click();
    expect(jsonSteps()).toEqual([{ after_days: 0, fraction: 0.5 }, { after_days: 30, fraction: 0.5 }]);
    document.querySelector<HTMLButtonElement>('[data-preset="once"]')!.click();
    expect(JSON.parse(textarea().value)).toEqual({ steps: [{ after_days: 0, fraction: 1 }] });
  });

  test('the save sends the schedule built from the rows', async () => {
    await openNew();
    $('ladder-ramp-add').click();
    type(shareInput(0), '60');
    type(shareInput(1), '40');
    type(afterInput(1), '30');
    await saveLadderConfig();
    expect(sentRamp()).toEqual({ steps: [{ after_days: 0, fraction: 0.6 }, { after_days: 30, fraction: 0.4 }] });
  });

  test('an untouched stored schedule is sent back unchanged', async () => {
    const stored = { steps: [{ after_days: 0, fraction: 0.1 }, { after_days: 5, fraction: 0.2 }, { after_days: 9, fraction: 0.7 }] };
    await openEdit(stored);
    await saveLadderConfig();
    expect(sentRamp()).toEqual(stored);
  });

  test.each([
    ['fractions not summing to 1', '{"steps":[{"after_days":0,"fraction":0.5}]}', 'Shares add up to 50%; they must total 100%.'],
    ['duplicate after_days', '{"steps":[{"after_days":0,"fraction":0.5},{"after_days":0,"fraction":0.5}]}', 'Step 2: must be later than step 1 (day 0).'],
    ['no steps', '{"steps":[]}', 'Add at least one step.'],
    ['a fraction over 1', '{"steps":[{"after_days":0,"fraction":1.5},{"after_days":1,"fraction":-0.5}]}', 'Step 1: share can be at most 100%.'],
    ['a type error', '{"steps":[{"after_days":0,"fraction":"1"}]}', 'Step 1: fraction must be a number.'],
  ])('the save is refused for %s, as the backend would', async (_name, json, message) => {
    await openNew();
    $('ladder-ramp-json-toggle').click();
    type(textarea(), json);
    await saveLadderConfig();
    expect(api.upsertLadderConfig).not.toHaveBeenCalled();
    expect(mockShowToast).toHaveBeenCalledWith({ message: `Check the ramp schedule. ${message}`, kind: 'error' });
  });

  test('invalid JSON at save keeps the existing toast, reveals the textarea and focuses it', async () => {
    await openNew();
    textarea().value = '{"steps":';
    await saveLadderConfig();
    expect(api.upsertLadderConfig).not.toHaveBeenCalled();
    expect(mockShowToast).toHaveBeenCalledWith({ message: 'Ramp schedule is not valid JSON.', kind: 'error' });
    expect($('ladder-ramp-json')).toBeVisible();
    expect($('ladder-cfg-ramp-schedule-error').textContent).toMatch(/^Not valid JSON: /);
    expect(document.activeElement).toBe(textarea());
  });

  test('a stored value the rows cannot show opens in the JSON view', async () => {
    await openEdit({ steps: [{ after_days: '7', fraction: 1 }] });
    expect(rowEls()).toHaveLength(0);
    expect($('ladder-ramp-json')).toBeVisible();
    expect($('ladder-cfg-ramp-schedule-error')).toHaveTextContent('Step 1: after_days must be a whole number.');
    expect(summary()).toBe('Fix the JSON below to edit the steps here.');
  });

  test('reopening the modal resets the JSON view and errors', async () => {
    await openNew();
    $('ladder-ramp-json-toggle').click();
    type(textarea(), '{');
    $('ladder-modal-cancel-btn').click();
    $('ladder-add-config-btn').click();
    expect($('ladder-ramp-json')).not.toBeVisible();
    expect($('ladder-cfg-ramp-schedule-error')).not.toBeVisible();
    expect(rowValues()).toEqual([['0', '100']]);
  });
});
