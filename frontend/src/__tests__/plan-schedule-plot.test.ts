import { renderSchedulePlot, updateSchedulePlot, schedulePlotFrame, schedulePlotX } from '../plan-schedule-plot';

test('plots logical ordinals without bridging missing dates or relabeling retained steps', () => {
  const points = [{ step: 2, date: '2026-10-22', target: 33 }, { step: 4, date: '2026-10-29', target: 99 }];
  const frame = schedulePlotFrame(points.map(point => point.date));
  const host = document.createElement('div');
  host.innerHTML = renderSchedulePlot(points, frame, 3, false);
  const svg = host.querySelector('svg')!;
  expect(svg.getAttribute('role')).toBe('img');
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

test('sizes the plot frame from the point count and rejects an empty schedule', () => {
  const dates = ['2026-10-22', '2026-10-29'];
  const frame = schedulePlotFrame(dates);
  expect(schedulePlotFrame([...dates].reverse())).toEqual(frame);
  expect(() => schedulePlotFrame([])).toThrow('No dated steps');
  expect(schedulePlotFrame(['2026-10-22']).span).toBe(8);
  expect(schedulePlotFrame(Array.from({ length: 5 }, () => '2026-10-22')).width).toBe(640);
  expect(schedulePlotFrame(Array.from({ length: 100 }, () => '2026-10-22')).width).toBe(4896);
});

test('starts the staircase at zero for contiguous steps from step 1', () => {
  const points = [{ step: 1, date: '2026-10-22', target: 25 }, { step: 2, date: '2026-10-29', target: 50 }];
  const frame = schedulePlotFrame(points.map(point => point.date));
  const host = document.createElement('div');
  host.innerHTML = renderSchedulePlot(points, frame, 0, false);
  expect(host.querySelector('.preview-staircase')?.getAttribute('d')).toBe(`M 48 260 H ${schedulePlotX(points[0]!.date, frame)} V 205 H ${schedulePlotX(points[1]!.date, frame)} V 150`);
});
