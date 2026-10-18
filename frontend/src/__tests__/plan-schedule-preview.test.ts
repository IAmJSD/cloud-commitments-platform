import { openPlanSchedulePreview, PlanSchedulePreviewInput } from '../plan-schedule-preview';
import { closeModal } from '../modal';
import { suggestedPurchaseStartDate } from '../lib/calendar-date';
import { renderSchedulePlot, updateSchedulePlot, schedulePlotFrame, schedulePlotX } from '../plan-schedule-plot';

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
  point.dispatchEvent(new Event('click', { bubbles: true }));
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
  point(2).dispatchEvent(new Event('click', { bubbles: true }));
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
  point(2).dispatchEvent(new Event('click', { bubbles: true }));
  jest.setSystemTime(new Date('2026-10-21T00:00:00Z'));
  point(2).dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', altKey: true, bubbles: true }));
  expect(point(2).getAttribute('aria-label')).toContain('2026-11-04');
  expect(document.getElementById('preview-date-error')?.textContent).toMatch(/future/);
});


const pointerPreview = (input: PlanSchedulePreviewInput = draft) => {
  openPlanSchedulePreview(input, parent);
  const svg = document.querySelector<SVGSVGElement>('#plan-schedule-preview svg')!;
  let captured = false;
  Object.assign(svg, {
    getScreenCTM: () => ({ inverse: () => ({}) }),
    createSVGPoint: () => ({ x: 0, y: 0, matrixTransform() { return { x: this.x, y: this.y }; } }),
    setPointerCapture: jest.fn(() => { captured = true; }),
    hasPointerCapture: () => captured,
    releasePointerCapture: jest.fn(() => { captured = false; }),
  });
  const send = (type: string, target: Element, x: number, id = 1, primary = true): void => {
    target.dispatchEvent(Object.assign(new Event(type, { bubbles: true, cancelable: true }),
      { pointerId: id, isPrimary: primary, button: 0, clientX: x, clientY: 0 }));
  };
  const x = (index: number): number => Number(point(index).querySelector('circle')!.getAttribute('cx'));
  return { svg, send, x, day: 544 / ((input.preset === 'immediate' ? 0 : (Math.floor(100 / Number(input.stepPercent)) - 1) * 7) + 8) };
};

test('drags dates from off-center grabs without jumping and keeps the frozen scale, shares and nodes', () => {
  const { svg, send, x, day } = pointerPreview();
  const input = field('preview-selected-date'), node = point(1), origin = x(1) + 18;
  send('pointerdown', node, origin);
  Object.assign(svg, { getScreenCTM: () => { throw new Error('Coordinate frame must stay frozen'); } });
  send('pointermove', svg, origin + day / 4);
  expect(node.getAttribute('aria-label')).toContain('2026-10-27');
  send('pointermove', svg, origin + day);
  expect(node.getAttribute('aria-label')).toContain('2026-10-28');
  expect(input.value).toBe('2026-10-28');
  expect(document.activeElement).toBe(node);
  expect(input.disabled).toBe(true);
  expect(x(1)).toBeCloseTo(origin - 18 + day);
  send('pointerup', svg, origin + day);
  expect(input.disabled).toBe(false);
  expect(document.activeElement).toBe(node);
  expect(point(1)).toBe(node);
  expect(point(1).getAttribute('aria-label')).toContain('target 40%');
  expect(parent.querySelector('input')?.value).toBe('unchanged');
});

test.each(['pointercancel', 'lostpointercapture', 'Escape'])('rolls back a captured draft on %s and closes only on a subsequent Escape', reason => {
  const { svg, send, x, day } = pointerPreview(), origin = x(1);
  send('pointerdown', point(1), origin);
  send('pointermove', svg, origin + day);
  expect(field('preview-selected-date').value).toBe('2026-10-28');
  if (reason === 'Escape') point(1).dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  else send(reason, svg, origin + day);
  expect(field('preview-selected-date').value).toBe('2026-10-27');
  expect(document.getElementById('preview-date-changes')?.textContent).toContain('(0)');
  expect(svg.releasePointerCapture).toHaveBeenCalled();
  expect(document.getElementById('plan-schedule-preview')).not.toBeNull();
  point(1).dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  expect(document.getElementById('plan-schedule-preview')).toBeNull();
});

test('redistributes endpoint gestures, focuses their owner before disabling inputs and supplies keyboard activation', () => {
  const { svg, send, day } = pointerPreview();
  const handle = document.querySelector<SVGGElement>('[data-preview-boundary="end"]')!;
  const origin = Number(handle.querySelector('.preview-handle-marker')!.getAttribute('x')) + 6;
  field('preview-range-end').focus();
  send('pointerdown', handle, origin);
  expect(document.activeElement).toBe(handle);
  send('pointermove', svg, origin + day * 3);
  expect(field('preview-range-end').value).toBe('2026-11-13');
  expect(point(1).getAttribute('aria-label')).toContain('2026-10-28');
  handle.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true }));
  expect(field('preview-range-end').value).toBe('2026-11-13');
  send('pointerup', svg, origin + day * 3);
  handle.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
  expect(field('preview-range-end').value).toBe('2026-11-14');
  handle.dispatchEvent(new KeyboardEvent('keydown', { key: ' ', bubbles: true, cancelable: true }));
  expect(document.activeElement).toBe(field('preview-range-end'));
});

test('rejects collisions/outside release, ignores other pointers and dirty input, and releases capture on close', () => {
  const { svg, send, x, day } = pointerPreview(), origin = x(1);
  send('pointerdown', svg, 100); expect(svg.setPointerCapture).not.toHaveBeenCalled();
  send('pointerdown', point(1), origin, 2, false);
  send('pointermove', svg, origin + day, 2);
  expect(point(1).getAttribute('aria-label')).toContain('2026-10-27');
  send('pointerdown', point(1), origin);
  send('pointermove', svg, origin + day, 2);
  send('pointermove', svg, origin + day * 7);
  expect(field('preview-selected-date').value).toBe('2026-10-27');
  expect(document.getElementById('preview-date-error')?.textContent).toMatch(/order/);
  send('pointerup', svg, 1000);
  expect(document.getElementById('preview-date-error')?.textContent).toMatch(/plot/i);
  field('preview-selected-date').value = '';
  field('preview-selected-date').dispatchEvent(new Event('input'));
  send('pointerdown', point(1), origin);
  expect(field('preview-selected-date').value).toBe('');
  field('preview-reset-dates').click();
  send('pointerdown', point(1), x(1));
  field('close-schedule-preview').click();
  expect(svg.releasePointerCapture).toHaveBeenCalledTimes(2);
  openPlanSchedulePreview({ ...draft, preset: 'immediate' }, parent);
  expect(document.querySelector('[data-preview-boundary]')).toBeNull();
});

test('rechecks midnight on release and rolls back exactly with a reopen warning', () => {
  const { svg, send, x, day } = pointerPreview(), origin = x(1);
  send('pointerdown', point(1), origin);
  send('pointermove', svg, origin + day);
  jest.setSystemTime(new Date('2026-10-20T00:00:00Z'));
  send('pointerup', svg, origin + day);
  expect(point(1).getAttribute('aria-label')).toContain('2026-10-27');
  expect(document.getElementById('preview-date-error')?.textContent).toMatch(/reopen/i);
  expect(field('preview-selected-date').disabled).toBe(false);
});


test('reports capture failure and leaves dates and date-input access intact', () => {
  const { svg, send, x } = pointerPreview();
  Object.assign(svg, { setPointerCapture: () => { throw new Error('Capture unavailable'); } });
  const hit = point(1).querySelector<HTMLElement>('.preview-touch-target')!;
  expect(hit.namespaceURI).toBe('http://www.w3.org/1999/xhtml');
  expect(hit.tabIndex).toBe(-1);
  send('pointerdown', hit, x(1));
  expect(point(1).getAttribute('aria-label')).toContain('2026-10-27');
  expect(field('preview-selected-date').disabled).toBe(false);
  expect(document.getElementById('preview-date-error')?.textContent).toContain('Use date inputs');
});


test('plots logical ordinals without bridging missing dates or relabeling retained steps', () => {
  const points = [{ step: 2, date: '2026-10-22', target: 33 }, { step: 4, date: '2026-10-29', target: 99 }];
  const frame = schedulePlotFrame(points.map(point => point.date));
  const host = document.createElement('div');
  host.innerHTML = renderSchedulePlot(points, frame, 3, false);
  const svg = host.querySelector('svg')!;
  expect(svg.querySelector('[data-preview-step="1"]')?.getAttribute('aria-label')).toContain('Step 2,');
  expect(svg.querySelector('[data-preview-step="3"]')?.hasAttribute('data-selected')).toBe(true);
  expect(svg.querySelector('[data-preview-step="0"]')).toBeNull();
  expect(svg.querySelector('.preview-staircase')?.getAttribute('d')).toBe(` M ${schedulePlotX(points[0]!.date, frame)} ${260 - 33 * 2.2} M ${schedulePlotX(points[1]!.date, frame)} ${260 - 99 * 2.2}`);
  expect(svg.querySelector('[role="button"]')).toBeNull();
  const retained = svg.querySelector('[data-preview-step="3"]');
  const updated = [points[0]!, { ...points[1]!, step: 3, target: 66 }];
  updateSchedulePlot(svg, updated, frame, 2, false);
  expect(svg.querySelector('[data-preview-step="2"]')).toBe(retained);
  expect(retained?.getAttribute('aria-label')).toContain('Step 3,');
  expect(svg.querySelector('.preview-staircase')?.getAttribute('d')).toBe(` M ${schedulePlotX(points[0]!.date, frame)} ${260 - 33 * 2.2} H ${schedulePlotX(points[1]!.date, frame)} V ${260 - 66 * 2.2}`);
});

test('preserves contiguous plot geometry and uses only the known calendar extent', () => {
  const points = [{ step: 1, date: '2026-10-22', target: 25 }, { step: 2, date: '2026-10-29', target: 50 }];
  const frame = schedulePlotFrame(points.map(point => point.date));
  const host = document.createElement('div');
  host.innerHTML = renderSchedulePlot(points, frame, 0, true);
  expect(host.querySelector('.preview-staircase')?.getAttribute('d')).toBe(`M 48 260 H ${schedulePlotX(points[0]!.date, frame)} V 205 H ${schedulePlotX(points[1]!.date, frame)} V 150`);
  expect(host.querySelectorAll('[data-preview-boundary]')).toHaveLength(2);
  expect(host.querySelector('[data-preview-step="0"]')?.getAttribute('tabindex')).toBe('0');
  expect(schedulePlotFrame([...points.map(point => point.date)].reverse())).toEqual(frame);
  expect(() => schedulePlotFrame([])).toThrow('No dated steps');
  expect(schedulePlotFrame(['2026-10-22']).span).toBe(8);
  expect(schedulePlotFrame(Array.from({ length: 100 }, () => '2026-10-22')).width).toBe(4896);
});
