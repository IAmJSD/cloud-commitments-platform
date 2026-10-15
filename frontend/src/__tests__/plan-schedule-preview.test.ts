import { openPlanSchedulePreview, PlanSchedulePreviewInput } from '../plan-schedule-preview';
import { closeModal } from '../modal';
import { suggestedPurchaseStartDate } from '../lib/calendar-date';

const draft: PlanSchedulePreviewInput = { name: 'Compute ramp', preset: 'weekly-25pct', coverage: '80', stepPercent: '25', intervalDays: '7', notificationDays: '3', term: '1', payment: 'monthly' };
let parent: HTMLElement;
beforeEach(() => {
  jest.useFakeTimers({ now: new Date('2026-10-15T23:59:00Z') });
  document.body.innerHTML = '<div id="plan-modal"><input value="unchanged"><button id="trigger">Preview</button></div>';
  parent = document.getElementById('plan-modal')!;
  document.getElementById('trigger')!.focus();
});
afterEach(() => {
  const preview = document.getElementById('plan-schedule-preview');
  if (preview) closeModal(preview);
  jest.useRealTimers();
});

test('shows truthful hypothetical dates, preserves parent values, and restores focus on Escape', () => {
  openPlanSchedulePreview(draft, parent);
  const preview = document.getElementById('plan-schedule-preview')!;
  expect(preview.textContent).toContain('These dates are not saved or scheduled');
  expect(preview.textContent).toContain('Planned cumulative target (%)');
  expect(preview.textContent).toContain('Amount unavailable');
  expect(preview.querySelectorAll('[data-preview-step]')).toHaveLength(4);
  expect(parent.inert).toBe(true);
  const point = preview.querySelector('[data-preview-step="3"]') as unknown as HTMLElement;
  point.dispatchEvent(new Event('click'));
  expect((document.getElementById('preview-selected-date') as HTMLInputElement).value).toBe('2026-11-10');
  point.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true }));
  expect(document.getElementById('preview-selected-step')?.textContent).toBe('Step 3');
  expect(preview.querySelector<HTMLDetailsElement>('aside details')?.open).toBe(false);
  preview.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  expect(document.getElementById('plan-schedule-preview')).toBeNull();
  expect(parent.inert).toBeFalsy();
  expect(parent.querySelector('input')?.value).toBe('unchanged');
  expect(document.activeElement?.id).toBe('trigger');
});

test('preserves legacy custom33 shares and safely renders text when reopened', () => {
  openPlanSchedulePreview({ ...draft, preset: 'custom', stepPercent: '33', name: '<img src=x onerror=alert(1)>' }, parent);
  expect(document.querySelectorAll('[data-preview-step]')).toHaveLength(3);
  expect(document.querySelector('[data-preview-step="2"]')?.getAttribute('aria-label')).toContain('79.2%');
  expect(document.querySelector('img')).toBeNull();
  openPlanSchedulePreview({ ...draft, coverage: '0', preset: 'immediate' }, parent);
  expect(document.querySelectorAll('#plan-schedule-preview')).toHaveLength(1);
  expect(document.querySelector('[data-preview-step]')?.getAttribute('aria-label')).toContain('target 0%');
  openPlanSchedulePreview({ ...draft, preset: 'custom', stepPercent: '1' }, parent);
  expect(document.querySelectorAll('[data-preview-step]')).toHaveLength(100);
});

test.each([
  { coverage: '' }, { coverage: '101' }, { coverage: '1.5' }, { notificationDays: '0' }, { notificationDays: 'Infinity' },
  { preset: 'toString' }, { preset: 'custom', stepPercent: '0' }, { preset: 'custom', intervalDays: '366' },
])('rejects invalid draft %j without hiding the form', change => {
  expect(() => openPlanSchedulePreview({ ...draft, ...change }, parent)).toThrow();
  expect(document.getElementById('plan-schedule-preview')).toBeNull();
  expect(parent.inert).toBeFalsy();
});

test('uses UTC calendar dates across a year boundary for both real callers', () => {
  expect(suggestedPurchaseStartDate(3, new Date('2026-12-31T23:59:00Z'))).toBe('2027-01-05');
});

const field = (id: string): HTMLInputElement => document.getElementById(id) as HTMLInputElement;
const changeDate = (id: string, value: string): void => {
  const input = field(id);
  input.focus(); input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
};
const point = (index: number): SVGGElement => document.querySelector(`[data-preview-step="${index}"]`)!;

test('edits actual draft dates, guards collisions and preserves nodes, shares and parent values', () => {
  openPlanSchedulePreview(draft, parent);
  const input = field('preview-selected-date'), first = point(0);
  expect(input.readOnly).toBe(false);
  changeDate(input.id, '2026-10-21');
  expect(first.getAttribute('aria-label')).toContain('2026-10-21, target 20%');
  expect(point(0)).toBe(first);
  first.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', altKey: true, bubbles: true }));
  expect(input.value).toBe('2026-10-22');
  expect(document.querySelectorAll('[data-preview-step]')).toHaveLength(4);
  changeDate(input.id, '2026-10-27');
  expect(first.getAttribute('aria-label')).toContain('2026-10-22');
  expect(input.value).toBe('2026-10-27');
  expect(field('preview-range-start').disabled).toBe(true);
  point(2).dispatchEvent(new Event('click'));
  expect(field(input.id)).toBe(input);
  expect(document.activeElement).toBe(input);
  expect(document.getElementById('preview-selected-step')?.textContent).toBe('Step 1');
  expect(document.getElementById('preview-date-error')?.textContent).toMatch(/order/);
  field('preview-reset-dates').click();
  expect(input.value).toBe('2026-10-20');
  expect(document.getElementById('preview-date-error')?.textContent).toBe('');
  expect(parent.querySelector('input')?.value).toBe('unchanged');
  expect(document.body.textContent).toContain('Save Plan does not apply these date edits');
});

test('retains partial paired inputs for correction and shows initial-to-draft differences', () => {
  openPlanSchedulePreview(draft, parent);
  const start = field('preview-range-start'), end = field('preview-range-end');
  start.focus(); start.value = ''; start.dispatchEvent(new Event('input'));
  point(0).dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
  expect(start.value).toBe('');
  expect(document.activeElement).toBe(start);
  changeDate(start.id, '2026-11-20');
  expect(start.value).toBe('2026-11-20');
  expect(end.disabled).toBe(false);
  expect(point(0).getAttribute('aria-label')).toContain('2026-10-20');
  changeDate(end.id, '2026-12-10');
  expect(document.activeElement).toBe(end);
  expect(field(end.id)).toBe(end);
  expect(point(2).getAttribute('aria-label')).toContain('2026-12-03');
  expect(document.getElementById('preview-date-changes')?.textContent).toContain('2026-10-20 → 2026-11-20');
  expect(field('preview-selected-date').disabled).toBe(false);
});

test('keeps single-step and legacy33 targets while exposing overlapping points through the Step selector', () => {
  openPlanSchedulePreview({ ...draft, preset: 'immediate' }, parent);
  expect(document.getElementById('preview-range-end')).toBeNull();
  changeDate('preview-range-start', '2026-10-21');
  expect(point(0).getAttribute('aria-label')).toContain('2026-10-21, target 80%');
  openPlanSchedulePreview({ ...draft, preset: 'custom', stepPercent: '33' }, parent);
  changeDate('preview-range-end', '2026-10-22');
  expect(point(2).getAttribute('aria-label')).toContain('2026-10-22, target 79.2%');
  openPlanSchedulePreview({ ...draft, coverage: '0' }, parent);
  changeDate('preview-range-end', '2027-10-20');
  const selector = document.getElementById('preview-step-select') as HTMLSelectElement;
  for (const [index, date] of [[1, '2026-10-21'], [2, '2026-10-22']] as const) {
    selector.value = String(index); selector.dispatchEvent(new Event('change'));
    changeDate('preview-selected-date', date);
  }
  const left = (index: number): number => Number(point(index).querySelector('circle')!.getAttribute('cx'));
  expect(left(1) - left(0)).toBeLessThan(3);
  for (let index = 0; index < 4; index++) {
    selector.value = String(index); selector.dispatchEvent(new Event('change'));
    expect(point(index).getAttribute('aria-pressed')).toBe('true');
    expect(field('preview-selected-date').value).toBe(['2026-10-20', '2026-10-21', '2026-10-22', '2027-10-20'][index]);
  }
});

test('rechecks operation-time UTC midnight and refuses stale reset without replacing repaired dates', () => {
  openPlanSchedulePreview(draft, parent);
  jest.setSystemTime(new Date('2026-10-20T00:00:00Z'));
  changeDate('preview-selected-date', '2026-10-21');
  expect(point(0).getAttribute('aria-label')).toContain('2026-10-21');
  changeDate('preview-range-end', '2026-11-11');
  field('preview-reset-dates').click();
  expect(point(0).getAttribute('aria-label')).toContain('2026-10-21');
  expect(document.getElementById('preview-date-error')?.textContent).toMatch(/reopen/i);
  point(2).dispatchEvent(new Event('click'));
  jest.setSystemTime(new Date('2026-10-21T00:00:00Z'));
  point(2).dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', altKey: true, bubbles: true }));
  expect(point(2).getAttribute('aria-label')).toContain('2026-11-04');
  expect(document.getElementById('preview-date-error')?.textContent).toMatch(/future/);
});
