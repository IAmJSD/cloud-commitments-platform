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
  expect(preview.querySelector('details')?.open).toBe(false);
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
