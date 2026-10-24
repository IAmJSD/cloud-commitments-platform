/**
 * Issue #747: charts are updated in place instead of destroyed and rebuilt,
 * so the entry animation doesn't replay on every filter or range change.
 */
jest.mock('chart.js', () => {
  const MockChart = jest.fn().mockImplementation((canvas: HTMLCanvasElement, config: { type: string; data: unknown; options?: unknown }) => ({
    canvas,
    config: { type: config.type },
    data: config.data,
    options: config.options,
    update: jest.fn(),
    destroy: jest.fn(),
  }));
  return { Chart: MockChart, registerables: [] };
});

import { Chart, type ChartConfiguration } from 'chart.js';
import { upsertChart } from '../modules/chart-utils';

type MockInstance = { canvas: HTMLCanvasElement; data: unknown; options: unknown; update: jest.Mock; destroy: jest.Mock };

const config = (value: number, type: 'line' | 'bar' = 'line'): ChartConfiguration => ({
  type,
  data: { datasets: [{ label: 'x', data: [value] }] },
  options: { responsive: true },
});

describe('upsertChart', () => {
  beforeEach(() => (Chart as unknown as jest.Mock).mockClear());

  test('creates a chart on first render', () => {
    const canvas = document.createElement('canvas');
    const chart = upsertChart(null, canvas, config(1));
    expect(Chart).toHaveBeenCalledTimes(1);
    expect((chart as unknown as MockInstance).canvas).toBe(canvas);
  });

  test('updates an existing chart in place without animation', () => {
    const canvas = document.createElement('canvas');
    const first = upsertChart(null, canvas, config(1));
    const next = config(2);
    const second = upsertChart(first, canvas, next);

    expect(second).toBe(first);
    expect(Chart).toHaveBeenCalledTimes(1);
    const inst = second as unknown as MockInstance;
    expect(inst.data).toBe(next.data);
    expect(inst.options).toBe(next.options);
    expect(inst.update).toHaveBeenCalledWith('none');
    expect(inst.destroy).not.toHaveBeenCalled();
  });

  test('recreates the chart when its canvas was replaced', () => {
    const first = upsertChart(null, document.createElement('canvas'), config(1));
    const fresh = document.createElement('canvas');
    const second = upsertChart(first, fresh, config(2));

    expect((first as unknown as MockInstance).destroy).toHaveBeenCalledTimes(1);
    expect(second).not.toBe(first);
    expect((second as unknown as MockInstance).canvas).toBe(fresh);
    expect(Chart).toHaveBeenCalledTimes(2);
  });

  // #749's Purchases series toggle switches between a line and a bar chart;
  // Chart.js can't change the chart type of an instance in place.
  test('recreates the chart when the chart type changes', () => {
    const canvas = document.createElement('canvas');
    const first = upsertChart(null, canvas, config(1, 'bar'));
    const second = upsertChart(first, canvas, config(2, 'line'));

    expect((first as unknown as MockInstance).destroy).toHaveBeenCalledTimes(1);
    expect((first as unknown as MockInstance).update).not.toHaveBeenCalled();
    expect(second).not.toBe(first);
    expect(Chart).toHaveBeenCalledTimes(2);
  });
});
